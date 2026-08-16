// forge-setup installs FORGE for the current user.
//
// It ships as one executable with the application binaries embedded, so there
// is nothing to unzip and no second download. Everything it writes goes under
// the user's own profile: no administrator rights, no UAC prompt, no files
// outside a directory the user already owns. That is a deliberate trade — a
// machine-wide install would need elevation, and a coding agent is a per-user
// tool anyway.
//
// Uninstalling is the same executable with -uninstall, and it removes exactly
// what installation created.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/gui"
)

// payload holds the binaries to install. mkdist writes them here before
// building this program; the checked-in directory has only a placeholder so
// the tree still compiles from a clean checkout.
//
//go:embed all:payload
var payload embed.FS

const (
	appName    = "FORGE"
	appPublish = "VEER-TARGARYEN"
	appVersion = "1.0.0"
	appURL     = "https://github.com/VEER-TARGARYEN/forge"
	// uninstallKey is the registry subkey Windows lists in Installed apps.
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\FORGE`
)

func main() {
	var (
		uninstall = flag.Bool("uninstall", false, "remove a previous installation")
		dir       = flag.String("dir", "", "install directory (default: per-user application folder)")
		noShort   = flag.Bool("no-shortcuts", false, "do not create menu or desktop shortcuts")
		noPath    = flag.Bool("no-path", false, "do not add the install directory to PATH")
		silent    = flag.Bool("silent", false, "no output except errors")
		launch    = flag.Bool("launch", true, "start FORGE when installation finishes")
	)
	flag.Parse()

	log := func(format string, a ...any) {
		if !*silent {
			fmt.Printf(format+"\n", a...)
		}
	}

	target, err := installDir(*dir)
	if err != nil {
		fail(err)
	}

	if *uninstall {
		if err := doUninstall(target, log); err != nil {
			fail(err)
		}
		log("\n%s has been removed.", appName)
		log("Your config and history in %s were left alone.", stateDirHint())
		return
	}

	if err := doInstall(target, *noShort, *noPath, *launch, log); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "\nsetup failed: %v\n", err)
	// A double-clicked installer's window closes the instant it exits, taking
	// the error with it. Hold it open long enough to be read.
	if runtime.GOOS == "windows" && !isRedirected() {
		fmt.Fprintf(os.Stderr, "\nPress Enter to close.")
		fmt.Scanln()
	}
	os.Exit(1)
}

func isRedirected() bool {
	st, err := os.Stdout.Stat()
	return err != nil || (st.Mode()&os.ModeCharDevice) == 0
}

// installDir resolves where the application lives.
func installDir(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	switch runtime.GOOS {
	case "windows":
		// %LOCALAPPDATA%\Programs is where per-user installs belong, and is
		// where VS Code and others put themselves.
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "Programs", appName), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Applications", appName+".app", "Contents", "MacOS"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", appName), nil
	}
}

func stateDirHint() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge")
}

// ---------- install ----------

func doInstall(target string, noShortcuts, noPath, launch bool, log func(string, ...any)) error {
	log("%s %s", appName, appVersion)
	log("Installing to %s\n", target)

	bins, err := payloadBinaries()
	if err != nil {
		return err
	}
	if len(bins) == 0 {
		return fmt.Errorf(
			"this installer was built without its payload\n" +
				"build it with: go run ./cmd/mkdist")
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}

	for name, data := range bins {
		dst := filepath.Join(target, name)
		// Replacing a running executable fails on Windows with a sharing
		// violation. Renaming it out of the way first always works, and the
		// stale copy is removed on the next run.
		if _, err := os.Stat(dst); err == nil {
			old := dst + ".old"
			_ = os.Remove(old)
			if err := os.Rename(dst, old); err != nil {
				return fmt.Errorf("replace %s (is it running?): %w", name, err)
			}
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		log("  installed %s", name)
	}
	cleanStale(target)

	// Copy this installer alongside what it installed. Without it the
	// uninstall entry would point at whatever path the user happened to run
	// it from — their Downloads folder, most likely — and stop working the
	// moment they tidied up.
	if err := installSelf(target); err != nil {
		log("  (uninstaller not copied: %v)", err)
	} else {
		log("  installed %s", setupName())
	}

	iconPath, err := writeIcon(target)
	if err != nil {
		// An install without an icon is cosmetically poorer but entirely
		// usable, so this must not abort it.
		log("  (icon not written: %v)", err)
	}

	if !noShortcuts {
		if err := createShortcuts(target, iconPath, log); err != nil {
			log("  (shortcuts not created: %v)", err)
		}
	}
	if !noPath {
		if err := addToPath(target, log); err != nil {
			log("  (PATH not updated: %v)", err)
		}
	}
	if err := writeUninstallEntry(target, iconPath); err != nil {
		log("  (uninstall entry not registered: %v)", err)
	}

	log("\nDone.")
	log("  Launch    %s from your %s", appName, launcherHint())
	log("  Terminal  forge do \"...\"   (open a new terminal first)")
	log("  Remove    %s -uninstall", filepath.Join(target, setupName()))

	if launch {
		exe := filepath.Join(target, desktopBinary())
		cmd := exec.Command(exe)
		if runtime.GOOS != "windows" {
			cmd = exec.Command(exe, "app")
		}
		_ = cmd.Start()
	}
	return nil
}

// payloadBinaries returns the embedded files to install, keyed by base name.
func payloadBinaries() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(payload, "payload", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := filepath.Base(p)
		if strings.HasPrefix(name, ".") || name == "README.md" {
			return nil
		}
		b, err := payload.ReadFile(p)
		if err != nil {
			return err
		}
		out[name] = b
		return nil
	})
	return out, err
}

// installSelf places a copy of this executable in the install directory so the
// uninstaller is always where the registry says it is.
func installSelf(target string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	dst := filepath.Join(target, setupName())
	if same, _ := sameFile(self, dst); same {
		return nil // re-running the installed copy
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		old := dst + ".old"
		_ = os.Remove(old)
		_ = os.Rename(dst, old)
	}
	return os.WriteFile(dst, b, 0o755)
}

func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// cleanStale removes copies renamed aside by a previous upgrade.
func cleanStale(target string) {
	entries, err := os.ReadDir(target)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".old") {
			_ = os.Remove(filepath.Join(target, e.Name()))
		}
	}
}

// desktopBinary is the executable a shortcut should point at: the windowless
// build on Windows, the ordinary one elsewhere.
//
// It is not FORGE.exe. Windows paths are case-insensitive, so that name is the
// same file as the CLI's forge.exe — shipping both would install one on top of
// the other, and which one survived would depend on map iteration order.
func desktopBinary() string {
	if runtime.GOOS == "windows" {
		return "forge-app.exe"
	}
	return "forge"
}

func setupName() string {
	if runtime.GOOS == "windows" {
		return "forge-setup.exe"
	}
	return "forge-setup"
}

func launcherHint() string {
	switch runtime.GOOS {
	case "windows":
		return "Start Menu or Desktop"
	case "darwin":
		return "Applications folder"
	default:
		return "application launcher"
	}
}

// writeIcon renders the mark into the platform's icon format and returns its
// path. The image is generated rather than copied, so there is no binary asset
// to keep in step with the design.
func writeIcon(target string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		b, err := gui.IconICO()
		if err != nil {
			return "", err
		}
		p := filepath.Join(target, appName+".ico")
		return p, os.WriteFile(p, b, 0o644)
	case "darwin":
		b, err := gui.IconICNS()
		if err != nil {
			return "", err
		}
		// Resources sits alongside MacOS inside the bundle.
		res := filepath.Join(filepath.Dir(target), "Resources")
		if err := os.MkdirAll(res, 0o755); err != nil {
			return "", err
		}
		p := filepath.Join(res, appName+".icns")
		return p, os.WriteFile(p, b, 0o644)
	default:
		b, err := gui.IconPNG(512, false)
		if err != nil {
			return "", err
		}
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".local", "share", "icons", "hicolor", "512x512", "apps")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		p := filepath.Join(dir, "forge.png")
		return p, os.WriteFile(p, b, 0o644)
	}
}
