// Command ipmi-connect is a browser-based replacement for the Java Web Start KVM consoles
// shipped with legacy Dell (Avocent) and Supermicro (ATEN) BMCs.
//
// Without a host it runs the desktop connection manager: a local console server on a free
// loopback port that the user's browser opens, where they choose a BMC. With a host it connects
// at once, as earlier versions did.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/config"
	"github.com/a5m0/ipmi-connect/internal/portal"
	"github.com/a5m0/ipmi-connect/internal/session"
	"github.com/a5m0/ipmi-connect/internal/trust"
	"github.com/a5m0/ipmi-connect/internal/web"
)

var version = "dev"

func getVersion() string {
	if version != "dev" && version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// defaultDesktopIdle ends an unattended desktop session.
const defaultDesktopIdle = 30 * time.Minute

func main() {
	opts := parseArgs(os.Args[1:])
	if opts.ShowVersion {
		fmt.Printf("ipmi-connect %s\n", getVersion())
		os.Exit(0)
	}
	if opts.ShowHelp {
		printUsage()
		os.Exit(0)
	}
	if opts.PortIgnored {
		log.Printf("Ignoring --port: BMC ports are fixed by each vendor's protocol. Use --web-port for the local console.")
	}
	desktop := opts.Desktop || opts.Host == ""
	if err := run(opts, desktop); err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

// app is everything both modes share.
type app struct {
	server  *web.Server
	manager *session.Manager
	trust   *trust.Store
}

func newApp(opts options, desktop bool) (*app, error) {
	trustPath, err := config.TrustPath()
	if err != nil {
		return nil, fmt.Errorf("find the configuration folder: %w", err)
	}
	trustStore, err := trust.OpenStore(trustPath)
	if err != nil {
		return nil, fmt.Errorf("trusted certificates: %w", err)
	}
	var profiles *config.Store
	if p, err := config.ProfilesPath(); err == nil {
		if profiles, err = config.Open(p); err != nil {
			log.Printf("Device profiles are unavailable: %v", err)
			profiles = nil
		}
	}

	port := opts.WebPort
	if desktop && !opts.WebPortSet {
		port = desktopPort()
	}
	server := web.NewServer(port, bmc.SessionState{PowerState: bmc.PowerStateUnknown, ScreenWidth: 1024, ScreenHeight: 768})
	server.Version = getVersion()
	server.Desktop = desktop
	server.Trust = trustStore
	server.Profiles = profiles
	server.Media = mediaRoots(opts.MediaDirs)
	if inCrostini() {
		server.URLHost = "localhost" // ChromeOS forwards localhost ports into the container
		server.LaunchTTL = 10 * time.Minute
	}
	if opts.NoBrowser {
		server.LaunchTTL = 10 * time.Minute // time to copy the link by hand
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if portal.Available(ctx) {
		server.PickFile = portal.PickFiles
	}
	cancel()

	manager := session.NewManager(session.Options{Trust: trustStore, Profiles: profiles, Display: server})
	server.Sessions = manager
	server.IdleTimeout = opts.IdleTimeout
	if desktop && !opts.IdleSet {
		server.IdleTimeout = defaultDesktopIdle
	}
	return &app{server: server, manager: manager, trust: trustStore}, nil
}

// mediaRoots are the folders virtual media images may come from: the home folder (outside a
// Flatpak, where it is not visible anyway), the application's private media folder, and any
// --media-dir.
func mediaRoots(extra []string) *web.MediaRoots {
	roots := web.NewMediaRoots()
	if !portal.InFlatpak() {
		if home, err := os.UserHomeDir(); err == nil {
			roots.AddRoot(home)
		}
	}
	if d, err := config.MediaDir(); err == nil {
		roots.AddRoot(d)
	}
	for _, d := range extra {
		if abs, err := filepath.Abs(d); err == nil {
			roots.AddRoot(abs)
		}
	}
	return roots
}

// desktopPort is the local port for desktop mode: any free port, except on ChromeOS, whose
// browser reaches the Linux container only through forwarded ports.
func desktopPort() int {
	if p, err := strconv.Atoi(os.Getenv("IPMI_CONNECT_WEB_PORT")); err == nil && p > 0 && p < 65536 {
		return p
	}
	if inCrostini() {
		return defaultWebPort
	}
	return 0
}

// inCrostini reports whether this is the Linux container on ChromeOS.
func inCrostini() bool {
	if _, err := os.Stat("/dev/.cros_milestone"); err == nil {
		return true
	}
	return os.Getenv("SOMMELIER_VERSION") != ""
}

func run(opts options, desktop bool) error {
	a, err := newApp(opts, desktop)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first Ctrl+C, restore default handling so a second one exits immediately, even
	// while a BMC is slow to log out.
	go func() {
		<-ctx.Done()
		log.Printf("[Signal] Shutting down, logging out of the BMC first (Ctrl+C again to force quit)...")
		stop()
	}()
	var quit sync.Once
	shutdown := func() { quit.Do(stop) }
	a.server.OnShutdown = shutdown
	a.server.OnIdle = func() {
		log.Printf("Idle timeout: disconnecting and quitting")
		shutdown()
	}
	// Whatever happens, end the BMC session on the way out: the Dell allows only one, and an
	// unclean exit locks the user out until the BMC times it out.
	defer a.manager.Disconnect()

	if !desktop {
		if err := connectCLI(ctx, a, opts); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}

	listener, err := a.server.Listen(desktop && inCrostini() && !opts.WebPortSet)
	if err != nil {
		return fmt.Errorf("start the local console: %w", err)
	}
	if desktop && !opts.NoBrowser {
		if ui := newNativeUI(); ui != nil {
			return serveInWindow(ctx, a, ui, listener, shutdown)
		}
	}
	opener := func(url string) { showLink(url, opts.NoBrowser) }
	go offerNewLinks(ctx, a.server, opts.NoBrowser)
	if err := a.server.Serve(ctx, listener, opener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// serveInWindow runs the server in the background and the native window on this goroutine.
// Closing the window quits; quitting any other way (Quit button, idle timeout, Ctrl+C) closes
// the window.
func serveInWindow(ctx context.Context, a *app, ui nativeUI, listener net.Listener, shutdown func()) error {
	a.server.SaveFile = ui.SaveFile
	if a.server.PickFile == nil { // the FileChooser portal is preferred where there is one
		a.server.PickFile = ui.PickFiles
	}
	links := make(chan string, 1)
	served := make(chan error, 1)
	go func() {
		served <- a.server.Serve(ctx, listener, func(url string) { links <- url })
	}()
	winErr := ui.Run(ctx, links)
	shutdown()
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return winErr
}
