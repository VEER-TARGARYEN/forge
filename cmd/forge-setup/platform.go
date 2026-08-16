package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Desktop integration, per platform, with no third-party packages.
//
// On Windows that means driving built-in tooling — PowerShell is present on
// every supported release, and it is the only dependency-free way to author a
// .lnk (the shell link format is a documented binary blob, but hand-rolling
// one to save a process launch would be a lot of fragile code for nothing) and
// to set a user environment variable safely. setx is deliberately avoided: it
// truncates PATH at 1024 characters, which silently destroys the variable on
// any machine with a normal amount installed.

// psQuote wraps a string as a PowerShell single-quoted literal.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runPS(script string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", firstLines(msg, 3))
	}
	return nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

// ---------- shortcuts ----------

func createShortcuts(target, iconPath string, log func(string, ...any)) error {
	switch runtime.GOOS {
	case "windows":
		return windowsShortcuts(target, iconPath, log)
	case "darwin":
		return macAppBundle(target, log)
	default:
		return linuxDesktopEntry(target, iconPath, log)
	}
}

func windowsShortcuts(target, iconPath string, log func(string, ...any)) error {
	exe := filepath.Join(target, desktopBinary())
	home, _ := os.UserHomeDir()

	locations := map[string]string{
		"Start Menu": filepath.Join(os.Getenv("APPDATA"),
			`Microsoft\Windows\Start Menu\Programs`, appName+".lnk"),
		"Desktop": filepath.Join(home, "Desktop", appName+".lnk"),
	}

	for label, lnk := range locations {
		if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
			return err
		}
		icon := iconPath
		if icon == "" {
			icon = exe
		}
		script := strings.Join([]string{
			"$s = (New-Object -ComObject WScript.Shell).CreateShortcut(" + psQuote(lnk) + ")",
			"$s.TargetPath = " + psQuote(exe),
			// The workspace defaults to the user's home rather than wherever
			// the shortcut happens to be invoked from, so a double-click never
			// points the agent at Program Files.
			"$s.WorkingDirectory = " + psQuote(home),
			"$s.IconLocation = " + psQuote(icon),
			"$s.Description = 'A coding agent that runs on your machine'",
			"$s.Save()",
		}, "; ")
		if err := runPS(script); err != nil {
			return fmt.Errorf("%s shortcut: %w", label, err)
		}
		log("  %s shortcut", label)
	}
	return nil
}

// macAppBundle completes the .app around the binaries already written into
// Contents/MacOS.
func macAppBundle(target string, log func(string, ...any)) error {
	contents := filepath.Dir(target) // .../FORGE.app/Contents
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>%s</string>
  <key>CFBundleDisplayName</key><string>%s</string>
  <key>CFBundleIdentifier</key><string>com.veertargaryen.forge</string>
  <key>CFBundleVersion</key><string>%s</string>
  <key>CFBundleShortVersionString</key><string>%s</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleExecutable</key><string>%s</string>
  <key>CFBundleIconFile</key><string>%s</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`, appName, appName, appVersion, appVersion, appName, appName)

	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(plist), 0o644); err != nil {
		return err
	}

	// Launching a bundle runs CFBundleExecutable with no arguments, and the
	// CLI build prints usage in that case. A two-line launcher turns the same
	// binary into the app without a second Go build.
	launcher := "#!/bin/sh\nexec \"$(dirname \"$0\")/forge\" app \"$@\"\n"
	if err := os.WriteFile(filepath.Join(target, appName), []byte(launcher), 0o755); err != nil {
		return err
	}
	log("  application bundle")
	return nil
}

func linuxDesktopEntry(target, iconPath string, log func(string, ...any)) error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	icon := "forge"
	if iconPath != "" {
		icon = iconPath
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=%s
GenericName=Coding Agent
Comment=A coding agent that runs on your machine
Exec=%s app
Icon=%s
Terminal=false
Categories=Development;IDE;
Keywords=agent;code;ai;llm;
StartupWMClass=%s
`, appName, filepath.Join(target, "forge"), icon, appName)

	p := filepath.Join(dir, "forge.desktop")
	if err := os.WriteFile(p, []byte(entry), 0o644); err != nil {
		return err
	}
	// Best effort: without it some desktops only notice on next login.
	_ = exec.Command("update-desktop-database", dir).Run()
	log("  desktop entry")
	return nil
}

// ---------- PATH ----------

func addToPath(target string, log func(string, ...any)) error {
	if runtime.GOOS == "windows" {
		// Joined with newlines, not spaces: these are separate statements and
		// PowerShell needs a real terminator between them. Spaces parse as one
		// malformed expression.
		script := strings.Join([]string{
			"$p = [Environment]::GetEnvironmentVariable('Path','User')",
			"$d = " + psQuote(target),
			// Split and compare so a re-install does not append a duplicate.
			"if (($p -split ';') -notcontains $d) {",
			"  if ([string]::IsNullOrEmpty($p)) { $new = $d } else { $new = $p.TrimEnd(';') + ';' + $d }",
			"  [Environment]::SetEnvironmentVariable('Path', $new, 'User')",
			"}",
		}, "\n")
		if err := runPS(script); err != nil {
			return err
		}
		log("  added to PATH (new terminals only)")
		return nil
	}

	// A symlink into ~/.local/bin beats editing shell startup files: that
	// directory is on PATH by default on every current distribution and on
	// macOS, and removing one link is a clean uninstall where unpicking an
	// edit from someone's .bashrc is not.
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(binDir, "forge")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join(target, "forge"), link); err != nil {
		return err
	}
	log("  linked into %s", binDir)
	return nil
}

func removeFromPath(target string) error {
	if runtime.GOOS == "windows" {
		script := strings.Join([]string{
			"$p = [Environment]::GetEnvironmentVariable('Path','User')",
			"if ($p) {",
			"  $d = " + psQuote(target),
			"  $new = ($p -split ';' | Where-Object { $_ -ne $d -and $_ -ne '' }) -join ';'",
			"  [Environment]::SetEnvironmentVariable('Path', $new, 'User')",
			"}",
		}, "\n")
		return runPS(script)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	link := filepath.Join(home, ".local", "bin", "forge")
	// Only remove the link if it is ours; someone may have their own build
	// there and deleting it would be rude.
	if dst, err := os.Readlink(link); err == nil && strings.HasPrefix(dst, target) {
		return os.Remove(link)
	}
	return nil
}

// ---------- uninstall registration ----------

func writeUninstallEntry(target, iconPath string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	setup := filepath.Join(target, setupName())
	icon := iconPath
	if icon == "" {
		icon = filepath.Join(target, desktopBinary())
	}
	// Size is what Installed apps shows; report the real directory size rather
	// than a made-up number.
	kb := dirSizeKB(target)

	set := func(name, kind, value string) string {
		return fmt.Sprintf("New-ItemProperty -Path $k -Name %s -PropertyType %s -Value %s -Force | Out-Null",
			psQuote(name), kind, psQuote(value))
	}
	script := strings.Join([]string{
		"$k = " + psQuote(`HKCU:\`+uninstallKey),
		"New-Item -Path $k -Force | Out-Null",
		set("DisplayName", "String", appName),
		set("DisplayVersion", "String", appVersion),
		set("Publisher", "String", appPublish),
		set("InstallLocation", "String", target),
		set("DisplayIcon", "String", icon),
		set("URLInfoAbout", "String", appURL),
		set("UninstallString", "String", fmt.Sprintf(`"%s" -uninstall`, setup)),
		set("QuietUninstallString", "String", fmt.Sprintf(`"%s" -uninstall -silent`, setup)),
		fmt.Sprintf("New-ItemProperty -Path $k -Name 'EstimatedSize' -PropertyType DWord -Value %d -Force | Out-Null", kb),
		"New-ItemProperty -Path $k -Name 'NoModify' -PropertyType DWord -Value 1 -Force | Out-Null",
		"New-ItemProperty -Path $k -Name 'NoRepair' -PropertyType DWord -Value 1 -Force | Out-Null",
	}, "; ")
	return runPS(script)
}

func dirSizeKB(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total / 1024
}

// ---------- uninstall ----------

func doUninstall(target string, log func(string, ...any)) error {
	log("Removing %s from %s\n", appName, target)

	if err := removeFromPath(target); err != nil {
		log("  (PATH not cleaned: %v)", err)
	} else {
		log("  PATH cleaned")
	}

	for _, p := range shortcutPaths(target) {
		if err := os.Remove(p); err == nil {
			log("  removed %s", filepath.Base(p))
		}
	}

	if runtime.GOOS == "windows" {
		script := "$k = " + psQuote(`HKCU:\`+uninstallKey) +
			"; if (Test-Path $k) { Remove-Item -Path $k -Recurse -Force }"
		if err := runPS(script); err == nil {
			log("  removed uninstall entry")
		}
	}

	// The running executable cannot delete itself on Windows, so the files are
	// removed individually and a scheduled command clears the rest.
	root := target
	if runtime.GOOS == "darwin" {
		root = filepath.Dir(filepath.Dir(target)) // the .app bundle
	}
	if err := removeInstallTree(root); err != nil {
		log("  some files are still in use; they will be removed shortly")
	} else {
		log("  removed program files")
	}
	return nil
}

func shortcutPaths(target string) []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{
			filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`, appName+".lnk"),
			filepath.Join(home, "Desktop", appName+".lnk"),
		}
	case "darwin":
		return nil // the bundle is the shortcut
	default:
		return []string{
			filepath.Join(home, ".local", "share", "applications", "forge.desktop"),
			filepath.Join(home, ".local", "share", "icons", "hicolor", "512x512", "apps", "forge.png"),
		}
	}
}

func removeInstallTree(root string) error {
	self, _ := os.Executable()
	self, _ = filepath.Abs(self)

	if err := os.RemoveAll(root); err == nil {
		return nil
	}
	// Most likely this executable is inside the directory it is deleting.
	// Remove everything else, then have the shell delete the remainder once
	// this process has exited.
	var lastErr error
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		abs, _ := filepath.Abs(p)
		if abs == self {
			return nil
		}
		if err := os.Remove(p); err != nil {
			lastErr = err
		}
		return nil
	})
	scheduleSelfDelete(root)
	return lastErr
}

// scheduleSelfDelete asks the shell to remove the install directory after this
// process exits, which is the only way to delete a running executable.
func scheduleSelfDelete(root string) {
	if runtime.GOOS == "windows" {
		// ping supplies the delay: timeout needs a console this detached
		// process does not have.
		_ = exec.Command("cmd", "/C", "ping 127.0.0.1 -n 3 >nul & rmdir /S /Q "+root).Start()
		return
	}
	_ = exec.Command("sh", "-c", "sleep 2; rm -rf "+strings.ReplaceAll(root, " ", `\ `)).Start()
}
