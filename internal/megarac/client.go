package megarac

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/vmedia"
)

// reconnectMin is the first delay before a new KVM session; a variable so tests can shorten it.
var reconnectMin = 2 * time.Second

const (
	dialTimeout       = 5 * time.Second
	reconnectMax      = 30 * time.Second
	stableSession     = 30 * time.Second // a session this long resets the backoff
	logoutWaitTimeout = 5 * time.Second
)

// Client keeps a KVM session up, starting a new one with a fresh launch as needed, and implements
// bmc.Console on whichever session is current.
type Client struct {
	ep   *bmc.Endpoint
	user string
	web  *Web

	mu       sync.Mutex
	session  *Session
	tunnelAt int // the web port the current KVM tunnel was opened with
	running  bool
	stopped  chan struct{} // closed when Run returns

	// Manager provides virtual media (bmc.VirtualMedia) over media's IUSB tunnels, one per
	// drive type, which stay open across ejects until Logout.
	*vmedia.Manager
	media *mediaPool
}

// NewClient prepares a client for the BMC at ep; call Login, then Start. Every connection goes
// through ep.
func NewClient(ep *bmc.Endpoint, user, password string) *Client {
	c := &Client{ep: ep, user: user, web: NewWeb(ep, user, password), stopped: make(chan struct{})}
	c.media = newMediaPool(c.dialMedia, c.setMediaState)
	c.Manager = vmedia.NewManager(c.openVMedia)
	return c
}

// openVMedia starts virtual media on the current KVM session.
func (c *Client) openVMedia(context.Context) (vmedia.Session, error) {
	if _, err := c.current(); err != nil {
		return nil, fmt.Errorf("virtual media needs the KVM session: %w", err)
	}
	return c.media.view(), nil
}

// dialMedia opens a drive's tunnel: it goes through the web port like the KVM and is
// authenticated with the current KVM session's token.
func (c *Client) dialMedia(ctx context.Context, service string) (net.Conn, string, error) {
	s, err := c.current()
	if err != nil {
		return nil, "", fmt.Errorf("virtual media needs the KVM session: %w", err)
	}
	c.mu.Lock()
	webPort := c.tunnelAt
	c.mu.Unlock()
	conn, err := c.tunnel(ctx, webPort, s.cred.webCookie, service)
	if err != nil {
		return nil, "", err
	}
	return conn, s.cred.kvmToken, nil
}

// setMediaState reports media redirection starting or stopping on the current KVM session.
func (c *Client) setMediaState(on bool) {
	if s, err := c.current(); err == nil {
		if err := s.SetMediaState(on); err != nil {
			log.Printf("[MegaRAC] Media state: %v", err)
		}
	}
}

// tunnel opens a TLS connection to the web port and asks it to carry service.
func (c *Client) tunnel(ctx context.Context, webPort int, cookie, service string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := c.ep.DialPort(dctx, c.ep.HTTPSPort)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("tunnel: %w", err)
	}
	conn, err := c.ep.TLSClient(ctx, raw, "https")
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tunnel TLS: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	r := bufio.NewReader(conn)
	if err := openTunnel(conn, r, c.ep.Host, webPort, cookie, service); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return bufferedConn{conn, r}, nil
}

// bufferedConn reads through the reader that consumed the tunnel reply.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Login authenticates to the web interface; call it before Start so bad credentials fail fast.
func (c *Client) Login() error { return c.web.Login() }

// Start runs the client in the background until ctx ends. It marks the client running first,
// so a Logout straight after still waits for the KVM session to close.
func (c *Client) Start(ctx context.Context, sink Sink) {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	go c.run(ctx, sink)
}

func (c *Client) run(ctx context.Context, sink Sink) {
	defer close(c.stopped)
	delay := reconnectMin
	for {
		started := time.Now()
		err := c.serveOnce(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrEncryptionRequired) || errors.Is(err, errUnsupportedLaunch) {
			log.Printf("[MegaRAC] %v", err)
			sink.SetStatus(err.Error())
			return
		}
		if time.Since(started) > stableSession {
			delay = reconnectMin
		}
		log.Printf("[MegaRAC] KVM session ended (%v); reconnecting in %v", err, delay)
		sink.SetStatus("Reconnecting to the KVM…")
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, reconnectMax)
	}
}

var errUnsupportedLaunch = errors.New("unsupported KVM configuration")

// checkLaunch accepts only the single-port KVM over TLS: it reuses the web server's trusted
// certificate and keeps the screen and keystrokes encrypted.
func checkLaunch(l Launch) error {
	if !l.SinglePort {
		return fmt.Errorf("%w: the BMC uses a dedicated KVM port, which is not supported", errUnsupportedLaunch)
	}
	if !l.Secure {
		return fmt.Errorf("%w: the BMC offers the KVM without TLS", errUnsupportedLaunch)
	}
	return nil
}

// launch fetches KVM arguments, logging in again once if the web session has expired.
func (c *Client) launch(ctx context.Context) (Launch, error) {
	l, err := c.web.Launch(c.ep.Host)
	if err == nil || ctx.Err() != nil {
		return l, err
	}
	if lerr := c.web.Login(); lerr != nil {
		return Launch{}, fmt.Errorf("re-login: %w", lerr)
	}
	return c.web.Launch(c.ep.Host)
}

// serveOnce launches, tunnels and serves one KVM session.
func (c *Client) serveOnce(ctx context.Context, sink Sink) error {
	l, err := c.launch(ctx)
	if err != nil {
		return err
	}
	if err := checkLaunch(l); err != nil {
		return err
	}
	// The tunnel shares the web server's port, so it is the port the user connects to.
	conn, err := c.tunnel(ctx, l.WebPort, l.WebCookie, serviceVideo)
	if err != nil {
		return fmt.Errorf("KVM %w", err)
	}
	bc := conn.(bufferedConn)
	clientIP := ""
	if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		clientIP = a.IP.String()
	}
	s := newSession(bc.Conn, bc.r, credentials{kvmToken: l.KVMToken, webCookie: l.WebCookie, user: c.user, clientIP: clientIP})
	s.onValidated = func() {
		// A drive that outlived the previous KVM session is still connected: the pool only
		// reports changes, so tell this session, as the old one was told when the drive came up.
		if c.media != nil && c.media.active() {
			if err := s.SetMediaState(true); err != nil {
				log.Printf("[MegaRAC] Media state on the new KVM session: %v", err)
			}
		}
	}
	c.mu.Lock()
	c.session, c.tunnelAt = s, l.WebPort
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.session = nil
		c.mu.Unlock()
	}()
	sink.SetStatus("")
	return s.Run(ctx, sink)
}

// Logout ends the web session once the KVM session has closed. Call CloseMedia, then cancel
// Start's context, first; Logout closes the media too if that was skipped.
func (c *Client) Logout() {
	c.CloseMedia()
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if running {
		select {
		case <-c.stopped:
		case <-time.After(logoutWaitTimeout):
			log.Printf("[MegaRAC] KVM session did not close in time; logging out anyway")
		}
	}
	c.web.Logout()
}

func (c *Client) current() (*Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil, errors.New("MegaRAC KVM not connected")
	}
	return c.session, nil
}

// SendKeyEvent implements bmc.Console.
func (c *Client) SendKeyEvent(down bool, usage uint16) error {
	s, err := c.current()
	if err != nil {
		return err
	}
	return s.SendKeyEvent(down, usage)
}

// SendPointerEvent implements bmc.Console.
func (c *Client) SendPointerEvent(buttons byte, x, y uint16) error {
	s, err := c.current()
	if err != nil {
		return err
	}
	return s.SendPointerEvent(buttons, x, y)
}

// RequestFrameRefresh implements bmc.Console.
func (c *Client) RequestFrameRefresh() error {
	s, err := c.current()
	if err != nil {
		return err
	}
	return s.RequestFrameRefresh()
}

// PowerControl implements bmc.Console. Power commands go over the KVM session.
func (c *Client) PowerControl(action bmc.PowerAction) error {
	s, err := c.current()
	if err != nil {
		return fmt.Errorf("power control needs the KVM session: %w", err)
	}
	return s.PowerControl(action)
}

// PowerState implements bmc.PowerReader from the web interface, which works without a KVM
// session.
func (c *Client) PowerState() (string, error) { return c.web.PowerStatus() }

// KeyboardLEDs returns the host's keyboard LED state from the current KVM session; Known is
// false while there is no session or the BMC has not reported it.
func (c *Client) KeyboardLEDs() KeyboardLEDs {
	s, err := c.current()
	if err != nil {
		return KeyboardLEDs{}
	}
	return s.KeyboardLEDs()
}

// CloseMedia ejects every drive and closes the media tunnels, which also sends MEDIA_STATE 0.
// Call it before cancelling Start's context, so that goes out on the live KVM session, as
// JViewer does when it exits.
func (c *Client) CloseMedia() {
	if c.Manager != nil {
		c.Close() // eject every drive while the session token is still valid
	}
	if c.media != nil {
		c.media.Close() // the tunnels outlive ejects; close them before the web logout
	}
}
