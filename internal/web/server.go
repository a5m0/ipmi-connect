package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/config"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

func init() {
	_ = mime.AddExtensionType(".js", "application/javascript")
}

//go:embed static/*
var staticFS embed.FS

const (
	// maxClients bounds simultaneous browser sockets.
	maxClients = 8
	// detachTimeout bounds how long Detach waits for browsers to release held input.
	detachTimeout = 5 * time.Second
)

// Server hosts the local web UI, REST API, and WebSocket streaming bridge.
type Server struct {
	State   bmc.SessionState
	StateMu sync.RWMutex

	// Configuration, set before Run.
	Port        int    // preferred port; 0 asks the OS for a free one
	URLHost     string // host in launch links ("127.0.0.1" by default)
	LaunchTTL   time.Duration
	Version     string
	Desktop     bool          // started without a BMC: the UI offers the connection manager and Quit
	Sessions    Sessions      // connects and disconnects; nil for a console-only server (tests)
	Profiles    *config.Store // saved devices and recents; nil disables them
	Trust       *trust.Store  // trusted certificates, listed and forgotten from the UI
	Media       *MediaRoots   // where virtual media images may come from
	IdleTimeout time.Duration // 0 disables idle cleanup
	OnIdle      func()        // called once when idle for IdleTimeout
	OnShutdown  func()        // called when the UI asks the application to quit
	PickFile    FilePicker    // native file chooser (portal); nil when unavailable

	// After a power action the power state is re-read this often, for this long.
	powerFollowEvery time.Duration
	powerFollowFor   time.Duration
	typeMu           sync.Mutex  // one paste typed at a time
	typing           atomic.Bool // a paste is being typed: live input from every browser is held off
	followMu         sync.Mutex  // guards followUntil and following
	followUntil      time.Time
	following        bool
	typeDelay        time.Duration // between typed key events
	clients          map[*wsClient]struct{}
	clientsMu        sync.Mutex // guards clients
	HTTPServer       *http.Server
	auth             *auth
	sessionMu        sync.Mutex // guards boundPortV
	boundPortV       int
	lastActivity     atomic.Int64 // unix nanoseconds of the last authenticated request or input

	// bindMu guards binding, the console browser input currently goes to.
	bindMu  sync.Mutex
	binding *consoleBinding

	// Sockets are hijacked, so HTTPServer.Shutdown does not stop them: Run cancels connCtx and
	// waits on connWG so each socket releases its held input before the session ends.
	connCtx    context.Context
	connCancel context.CancelFunc
	connWG     sync.WaitGroup

	// The host's current picture (RGBA), kept so new browsers, slow browsers and screenshots
	// get a complete frame without asking the BMC.
	screenMu         sync.Mutex
	screen           []byte
	screenW, screenH int
}

// consoleBinding is one attachment of a console. Its context ends when the console is
// detached; every socket and paste using it is counted in wg, so Detach can wait for them to
// release held keys and buttons before the BMC session ends.
type consoleBinding struct {
	console bmc.Console
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// FilePicker asks the user to choose files with the desktop's native chooser. It returns the
// chosen paths (empty if the user cancelled).
type FilePicker func(ctx context.Context, title string, directory bool) ([]string, error)

// NewServer initializes a new Web UI server.
func NewServer(port int, state bmc.SessionState) *Server {
	connCtx, connCancel := context.WithCancel(context.Background())
	s := &Server{
		connCtx:    connCtx,
		connCancel: connCancel,
		auth:       newAuth(),
		State:      state,
		Port:       port,
		clients:    make(map[*wsClient]struct{}),

		powerFollowEvery: powerFollowInterval,
		powerFollowFor:   powerFollowDuration,
		typeDelay:        typeKeyDelay,
	}
	s.touch()
	return s
}

// SetConsole attaches console without changing the session description (tests, simple use).
func (s *Server) SetConsole(console bmc.Console) {
	s.StateMu.RLock()
	st := s.State
	s.StateMu.RUnlock()
	s.Attach(console, st)
}

// Attach implements session.Display: browser input now goes to console.
func (s *Server) Attach(console bmc.Console, state bmc.SessionState) {
	ctx, cancel := context.WithCancel(s.connCtx)
	b := &consoleBinding{console: console, ctx: ctx, cancel: cancel}
	s.bindMu.Lock()
	old := s.binding
	s.binding = b
	s.bindMu.Unlock()
	if old != nil {
		old.cancel()
	}
	s.StateMu.Lock()
	keepW, keepH := s.State.ScreenWidth, s.State.ScreenHeight
	s.State = state
	if s.State.ScreenWidth == 0 {
		s.State.ScreenWidth, s.State.ScreenHeight = keepW, keepH
	}
	if s.State.ScreenWidth == 0 {
		s.State.ScreenWidth, s.State.ScreenHeight = 1024, 768
	}
	s.StateMu.Unlock()
}

// Detach implements session.Display: it closes every console socket, waits (bounded) until
// each has released the keys and buttons it held, stops pastes, and clears the picture and the
// session description.
func (s *Server) Detach() {
	s.bindMu.Lock()
	b := s.binding
	s.binding = nil
	s.bindMu.Unlock()
	if b != nil {
		b.cancel()
		done := make(chan struct{})
		go func() { b.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(detachTimeout):
			log.Printf("[Server] Timed out waiting for browsers to release input")
		}
	}
	s.screenMu.Lock()
	s.screen, s.screenW, s.screenH = nil, 0, 0
	s.screenMu.Unlock()
	s.StateMu.Lock()
	s.State = bmc.SessionState{PowerState: bmc.PowerStateUnknown, ScreenWidth: 1024, ScreenHeight: 768}
	s.StateMu.Unlock()
}

// bound returns the current console binding, or nil when no BMC is attached.
func (s *Server) bound() *consoleBinding {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	return s.binding
}

// console returns the attached console, or nil.
func (s *Server) console() bmc.Console {
	if b := s.bound(); b != nil {
		return b.console
	}
	return nil
}

// SetPower implements session.Display.
func (s *Server) SetPower(state string) {
	s.StateMu.Lock()
	defer s.StateMu.Unlock()
	s.State.PowerState = state
}

// touch records activity for the idle timer.
func (s *Server) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

func (s *Server) clientCount() int {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	return len(s.clients)
}

// watchIdle calls OnIdle once nothing has used the console for IdleTimeout: no browser socket
// is open and no authenticated request has arrived. Closing a tab is not a reliable signal (the
// browser may be suspended, or killed), so inactivity is what counts.
func (s *Server) watchIdle(ctx context.Context) {
	if s.IdleTimeout <= 0 || s.OnIdle == nil {
		return
	}
	tick := time.NewTicker(min(s.IdleTimeout/4, 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.clientCount() > 0 {
				s.touch()
				continue
			}
			if time.Since(time.Unix(0, s.lastActivity.Load())) >= s.IdleTimeout {
				log.Printf("[Server] No browser has used the console for %v", s.IdleTimeout)
				s.OnIdle()
				return
			}
		}
	}
}

// routes wires the embedded UI, REST API, and WebSocket endpoint.
func (s *Server) routes() (http.Handler, error) {
	subFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	// Static assets are all top-level files. Matching only single-segment paths keeps API paths
	// out of the file server, so a wrong method on an API route is a 405, not a 404.
	files := http.FileServer(http.FS(subFS))
	mux.Handle("GET /{$}", files)
	mux.Handle("GET /{file}", files)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/power", s.handlePower)
	mux.HandleFunc("POST /api/ws-ticket", s.handleWSTicket)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.sessionRoutes(mux)
	s.mediaRoutes(mux)
	mux.HandleFunc("GET /api/screenshot.png", s.handleScreenshot)
	mux.HandleFunc("GET /ws", s.handleWS)
	return s.guard(mux), nil
}

// Listen binds the console to loopback: the preferred port, else (unless the port is required)
// a free one chosen by the OS.
func (s *Server) Listen(requirePort bool) (net.Listener, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port))
	if err != nil && s.Port != 0 && !requirePort {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.sessionMu.Lock()
	s.boundPortV = listener.Addr().(*net.TCPAddr).Port
	s.sessionMu.Unlock()
	return listener, nil
}

// Run serves the console until ctx is cancelled, then shuts the HTTP server down. It binds a
// listener first if Listen was not called. openBrowser, if set, is given a launch link once the
// server is ready.
func (s *Server) Run(ctx context.Context, openBrowser func(url string)) error {
	return s.Serve(ctx, nil, openBrowser)
}

// Serve is Run on an existing listener (nil: bind one).
func (s *Server) Serve(ctx context.Context, listener net.Listener, openBrowser func(url string)) error {
	mux, err := s.routes()
	if err != nil {
		return err
	}
	if listener == nil {
		if listener, err = s.Listen(false); err != nil {
			return err
		}
	}

	s.HTTPServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second, // the connect handler extends its own
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          log.New(discardHTTPNoise{}, "", 0),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.HTTPServer.Serve(listener) }()

	if openBrowser != nil {
		go openBrowser(s.LaunchURL())
	}
	idleCtx, stopIdle := context.WithCancel(ctx)
	defer stopIdle()
	go s.watchIdle(idleCtx)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = s.HTTPServer.Shutdown(shutdownCtx)

	s.connCancel()
	sockets := make(chan struct{})
	go func() { s.connWG.Wait(); close(sockets) }()
	select {
	case <-sockets:
	case <-shutdownCtx.Done():
		log.Printf("[Server] Timed out waiting for browser connections to close")
	}

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// discardHTTPNoise drops net/http's own log lines (TLS-less probes, aborted connections): they
// carry no secrets, but they can carry request lines with query strings.
type discardHTTPNoise struct{}

func (discardHTTPNoise) Write(p []byte) (int, error) { return len(p), nil }

// boundPort returns the listening port once bound (0 before).
func (s *Server) boundPort() int {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	return s.boundPortV
}

// shutdownTimeout bounds how long Run waits for in-flight HTTP requests on exit.
const shutdownTimeout = 3 * time.Second

// closeSockets ends every browser socket (logout): each releases its held input as it closes.
func (s *Server) closeSockets() {
	for _, c := range s.clientList() {
		c.close(websocket.StatusPolicyViolation, "signed out")
	}
}

// setBoundPort records the port when a listener is bound elsewhere (tests).
func (s *Server) setBoundPort(port int) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.boundPortV = port
}
