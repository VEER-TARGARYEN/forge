package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/VEER-TARGARYEN/forge/internal/gui"
	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// The desktop app is the browser you already have, without the browser.
//
// Every Chromium build ships an app mode: --app=<url> opens a window with no
// tab strip, no address bar, and its own taskbar entry. It is indistinguishable
// from a native window, and it costs nothing — no Electron runtime to bundle
// (120 MB), no Rust toolchain and WebView2 SDK to build against, no cgo.
//
// That matters more here than usual. forge is a single static binary with no
// dependencies; shipping a desktop shell that needs a C compiler to build would
// undo the property the whole project is organised around.

// chromiumCandidates are searched in order. Edge first on Windows because it is
// always present; Chrome first elsewhere for the same reason.
func chromiumCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		return []string{
			filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(pf86, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(local, `Programs\Opera\opera.exe`),
			filepath.Join(local, `Vivaldi\Application\vivaldi.exe`),
		}
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi",
		}
	default:
		return []string{
			"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
			"microsoft-edge", "brave-browser", "vivaldi-stable",
		}
	}
}

// findChromium returns the first candidate that exists, or "".
func findChromium() string {
	for _, c := range chromiumCandidates() {
		if strings.ContainsRune(c, os.PathSeparator) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// defaultAppPort is deliberately fixed.
//
// The first version picked a free port on every launch, which broke the app in
// a way that only showed up on the second run: the service worker caches the
// shell per origin, so yesterday's port still rendered the whole interface
// from cache and then failed every API call against a socket nobody was
// listening on. The user sees "Failed to fetch" on an app that looks fine.
// A PWA installed from that origin, or any bookmark, breaks the same way.
//
// A stable port keeps the origin stable, which is what makes an install stay
// installed.
const defaultAppPort = 4100

// freePort asks the OS for an unused port by binding and immediately closing.
// Only used when the fixed port is taken by something that is not forge.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// portFree reports whether we can bind the port right now.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// forgeAlreadyServing reports whether the thing holding a port is a forge
// server, so a second launch can join it instead of starting a rival agent
// with the same workspace.
func forgeAlreadyServing(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/bootstrap", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var probe struct {
		Version   string `json:"version"`
		Workspace string `json:"workspace"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&probe) != nil {
		return false
	}
	return probe.Version != ""
}

func cmdApp(args []string) error {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	dir := fs.String("dir", ".", "workspace root the agent may act in")
	port := fs.Int("port", defaultAppPort, "port to serve on (0 picks a free one)")
	width := fs.Int("width", 1440, "window width")
	height := fs.Int("height", 900, "window height")
	browserPath := fs.String("browser", "", "path to a Chromium-family browser (default: autodetect)")
	noWindow := fs.Bool("no-window", false, "start the server without opening a window")
	embedModel := fs.String("embed-model", "", "path to a local embedding model directory")
	embedClass := fs.String("embed-class", "embed", "routing class used for semantic search")
	repoMapTokens := fs.Int("repo-map", 1024, "token budget for the repository map (0 disables it)")
	maxBytes := fs.Int("max-tool-bytes", 30000, "cap on how much any single tool result may add to context")
	verifyCmd := fs.String("verify-cmd", "", "run this instead of the auto-detected checks")
	verifyTimeout := fs.Duration("verify-timeout", 5*time.Minute, "per-check timeout")
	maxSpawns := fs.Int("max-subagents", 8, "total sub-agent delegations allowed per run")
	maxParallel := fs.Int("parallel-subagents", 3, "how many delegations may run at once")
	noSubAgents := fs.Bool("no-subagents", false, "disable delegation entirely")
	noNotes := fs.Bool("no-notes", false, "do not load or write cross-session notes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	e, err := open(*cfgPath, false)
	if err != nil {
		return err
	}
	defer e.Close()

	// A desktop shortcut cannot pass -dir, and its working directory is the
	// user's home. Prefer the workspace they last chose in the interface; fall
	// back to -dir only when they have never chosen one, or asked explicitly.
	explicitDir := *dir != "."
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if !explicitDir {
		if saved := loadWorkspace(e.cfg.Dir()); saved != "" {
			root = saved
		}
	}
	if _, err := tools.NewWorkspace(root); err != nil {
		return err
	}

	p := *port
	switch {
	case p == 0:
		if p, err = freePort(); err != nil {
			return fmt.Errorf("could not find a free port: %w", err)
		}
	case portFree(p):
		// The common case: the fixed port is ours to take.
	case forgeAlreadyServing(p):
		// FORGE is already up. Show that window rather than starting a second
		// agent on the same workspace — two loops editing one tree is a way to
		// lose work, and the user asked to open the app, not to run two.
		url := fmt.Sprintf("http://127.0.0.1:%d", p)
		fmt.Fprintf(os.Stderr, "FORGE is already running at %s\n", url)
		if !*noWindow {
			openAppWindow(*browserPath, url, e.cfg.Dir(), *width, *height)
		}
		return nil
	default:
		// Something else has it. Move rather than fail, and say so, because
		// the app will come up on an origin the user's install does not know.
		if p, err = freePort(); err != nil {
			return fmt.Errorf("could not find a free port: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"port %d is in use by another program; serving on %d instead.\n"+
				"An installed shortcut pointing at %d will not reach this window.\n",
			*port, p, *port)
	}

	em, err := resolveEmbedder(*embedModel, e.cfg.Dir(), nil)
	if err != nil {
		return err
	}

	be := newGUIBackend(e, root, em, guiOptions{
		repoMapTok:   *repoMapTokens,
		embedClass:   *embedClass,
		verifyCmd:    *verifyCmd,
		vTimeout:     *verifyTimeout,
		maxSpawns:    *maxSpawns,
		maxParallel:  *maxParallel,
		noSubAgents:  *noSubAgents,
		noNotes:      *noNotes,
		maxToolBytes: *maxBytes,
	})

	srv := gui.NewServer(be, fmt.Sprintf("127.0.0.1:%d", p))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Closing the window ends the process. A server left running after its
	// only window is gone is a background agent nobody knows is there — with
	// filesystem access.
	windowClosed := make(chan struct{})

	return srv.Run(ctx, func(url string) {
		fmt.Fprintf(os.Stderr, "FORGE  %s\n", url)
		fmt.Fprintf(os.Stderr, "workspace: %s\n", root)
		if *noWindow {
			return
		}
		cmd := openAppWindow(*browserPath, url, e.cfg.Dir(), *width, *height)
		if cmd == nil {
			return // already fell back to the default browser
		}
		fmt.Fprintf(os.Stderr, "close the window to quit\n")
		go func() {
			_ = cmd.Wait()
			close(windowClosed)
			stop() // unwinds srv.Run through its context
		}()
	})
}

// openAppWindow launches a chromeless window at url and returns the running
// command, or nil if it fell back to the default browser.
func openAppWindow(browserPath, url, stateDir string, width, height int) *exec.Cmd {
	bin := browserPath
	if bin == "" {
		bin = findChromium()
	}
	if bin == "" {
		// No Chromium anywhere: fall back to the default browser. The app
		// still works, it just arrives in a tab.
		fmt.Fprintf(os.Stderr,
			"no Chromium-family browser found; opening your default browser instead\n")
		openBrowser(url)
		return nil
	}

	cmd := exec.Command(bin,
		"--app="+url,
		fmt.Sprintf("--window-size=%d,%d", width, height),
		// A dedicated profile keeps the window out of the user's browsing
		// session, and is what makes it a separate taskbar entry rather than
		// another window of their browser.
		"--user-data-dir="+filepath.Join(stateDir, "appwindow"),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,AutofillServerCommunication",
	)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open the app window: %v\n", err)
		openBrowser(url)
		return nil
	}
	fmt.Fprintf(os.Stderr, "window: %s\n", filepath.Base(bin))
	return cmd
}
