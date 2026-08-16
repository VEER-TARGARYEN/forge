// mkdist builds the distributable installers.
//
// The sequence matters and is easy to get wrong by hand: the application
// binaries have to exist before forge-setup is compiled, because it embeds
// them. So this builds forge, builds the windowless desktop variant, stages
// both into cmd/forge-setup/payload, then builds the installer around them —
// and clears the payload afterwards so a stale binary can never be embedded
// into a later build by accident.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	appName = "FORGE"
	// installerName is what the Windows installer for this machine is called.
	// Title case rather than FORGE.exe: it is a file people see in Downloads,
	// not a command they type.
	installerName = "Forge.exe"
	// desktopBinaryName is the windowless Windows build. Deliberately not
	// FORGE.exe — see the note where it is built.
	desktopBinaryName = "forge-app.exe"
)

type target struct {
	goos, goarch string
}

func (t target) String() string { return t.goos + "/" + t.goarch }

func (t target) exeSuffix() string {
	if t.goos == "windows" {
		return ".exe"
	}
	return ""
}

func main() {
	var (
		outDir  = flag.String("out", "dist", "directory to write installers to")
		only    = flag.String("target", "", "build one target only, e.g. windows/amd64 (default: host)")
		all     = flag.Bool("all", false, "build every supported target")
		keepTmp = flag.Bool("keep-payload", false, "leave the staged payload in place for inspection")
		version = flag.String("version", "", "version to stamp into the binaries (default: the git describe)")
		clean   = flag.Bool("clean", false, "empty the output directory first")
	)
	flag.Parse()

	if *version == "" {
		*version = gitVersion()
	}

	root, err := repoRoot()
	if err != nil {
		die(err)
	}

	targets := []target{{runtime.GOOS, runtime.GOARCH}}
	switch {
	case *all:
		targets = []target{
			{"windows", "amd64"}, {"windows", "arm64"},
			{"darwin", "amd64"}, {"darwin", "arm64"},
			{"linux", "amd64"}, {"linux", "arm64"},
		}
	case *only != "":
		goos, goarch, ok := strings.Cut(*only, "/")
		if !ok {
			die(fmt.Errorf("-target must look like windows/amd64"))
		}
		targets = []target{{goos, goarch}}
	}

	dist := filepath.Join(root, *outDir)
	if *clean {
		if err := os.RemoveAll(dist); err != nil {
			die(err)
		}
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		die(err)
	}

	fmt.Printf("%s %s\n", appName, *version)

	var built []string
	for _, t := range targets {
		start := time.Now()
		fmt.Printf("\n▸ %s\n", t)
		out, err := build(root, *outDir, t, *version, *keepTmp)
		if err != nil {
			die(fmt.Errorf("%s: %w", t, err))
		}
		built = append(built, out)
		info, _ := os.Stat(out)
		fmt.Printf("  → %s  (%.1f MB, %s)\n",
			mustRel(root, out), float64(info.Size())/(1<<20), time.Since(start).Round(time.Millisecond))
	}

	if dup, err := copyForHumans(dist, built); err != nil {
		die(err)
	} else if dup != "" {
		built = append(built, dup)
		fmt.Printf("\n  → %s  (copy of windows/amd64)\n", mustRel(root, dup))
	}

	// A checksum file is what lets the install scripts verify what they just
	// downloaded. A one-line installer that pipes an unverified binary into a
	// shell is exactly the pattern people are right to distrust.
	sums, err := writeChecksums(dist, built)
	if err != nil {
		die(err)
	}
	fmt.Printf("\n  → %s\n", mustRel(root, sums))

	fmt.Printf("\nInstallers are in %s\n", *outDir)
	fmt.Printf("Run one to install %s for the current user. No administrator rights needed.\n", appName)
}

// gitVersion derives a version from the repository so a build always says
// where it came from. Falls back to a marker rather than inventing a number.
func gitVersion() string {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "0.0.0-unknown"
	}
	v := strings.TrimSpace(string(out))
	return strings.TrimPrefix(v, "v")
}

// writeChecksums emits a SHA256SUMS file in the format sha256sum -c expects.
func writeChecksums(dist string, files []string) (string, error) {
	var b strings.Builder
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		// Two spaces, then the bare name: the paths are meaningless to whoever
		// downloads the assets individually.
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), filepath.Base(f))
	}
	p := filepath.Join(dist, "SHA256SUMS")
	return p, os.WriteFile(p, []byte(b.String()), 0o644)
}

func build(root, outDir string, t target, version string, keepPayload bool) (string, error) {
	payload := filepath.Join(root, "cmd", "forge-setup", "payload")

	// Always start from a clean payload. Embedding whatever a previous run
	// left behind is the kind of bug that ships a stale binary inside a
	// correct-looking installer.
	if err := clearPayload(payload); err != nil {
		return "", err
	}
	if !keepPayload {
		defer func() { _ = clearPayload(payload) }()
	}

	stamp := []string{"-s", "-w", "-X", "main.version=" + version}

	// 1. The CLI binary.
	cli := filepath.Join(payload, "forge"+t.exeSuffix())
	if err := goBuild(root, t, cli, stamp, "./cmd/forge"); err != nil {
		return "", fmt.Errorf("build forge: %w", err)
	}
	fmt.Printf("  built forge%s\n", t.exeSuffix())

	// 2. The desktop binary. On Windows this is a separate link: -H=windowsgui
	//    suppresses the console window that would otherwise flash up behind
	//    the app, and defaultCommand makes a bare double-click open the app
	//    rather than print usage to a console that is not there.
	//
	//    It cannot be called FORGE.exe: Windows paths are case-insensitive, so
	//    that is the same file as forge.exe and one silently overwrites the
	//    other. The shortcut carries the display name anyway.
	if t.goos == "windows" {
		desktop := filepath.Join(payload, desktopBinaryName)
		ld := append([]string{"-H=windowsgui", "-X", "main.defaultCommand=app"}, stamp...)
		if err := goBuild(root, t, desktop, ld, "./cmd/forge"); err != nil {
			return "", fmt.Errorf("build desktop binary: %w", err)
		}
		fmt.Printf("  built %s (windowless)\n", desktopBinaryName)
	}

	// 3. The installer, wrapped around them.
	// Asset names are canonical for every target, because the install scripts
	// construct them from uname output. A "nicer" name for the host build
	// would be a name those scripts cannot predict.
	name := fmt.Sprintf("%s-setup-%s-%s%s", appName, t.goos, t.goarch, t.exeSuffix())
	out := filepath.Join(root, outDir, name)
	setupLD := []string{"-s", "-w", "-X", "main.appVersion=" + version}
	if err := goBuild(root, t, out, setupLD, "./cmd/forge-setup"); err != nil {
		return "", fmt.Errorf("build installer: %w", err)
	}
	return out, nil
}

// copyForHumans duplicates the ordinary-desktop Windows installer under a
// plain name. Someone browsing the releases page should see something they
// recognise as the thing to click, not have to work out their architecture.
func copyForHumans(dist string, built []string) (string, error) {
	src := ""
	for _, f := range built {
		if strings.HasSuffix(f, "-windows-amd64.exe") {
			src = f
			break
		}
	}
	if src == "" {
		return "", nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dist, installerName)
	return dst, os.WriteFile(dst, b, 0o755)
}

func clearPayload(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".gitkeep" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	// go:embed fails outright on a directory with nothing in it, so the
	// placeholder has to survive cleaning.
	keep := filepath.Join(dir, ".gitkeep")
	if _, err := os.Stat(keep); os.IsNotExist(err) {
		return os.WriteFile(keep, nil, 0o644)
	}
	return nil
}

func goBuild(root string, t target, out string, ldflags []string, pkg string) error {
	args := []string{"build", "-trimpath"}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	args = append(args, "-o", out, pkg)

	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOOS="+t.goos,
		"GOARCH="+t.goarch,
		// The whole project is cgo-free; setting this explicitly means a
		// cross-build cannot silently pick up a C toolchain and produce a
		// binary that only runs on the build machine.
		"CGO_ENABLED=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", wd)
		}
		dir = parent
	}
}

func mustRel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "\nmkdist: %v\n", err)
	os.Exit(1)
}
