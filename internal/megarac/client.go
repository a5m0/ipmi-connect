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
)

const (
	dialTimeout       = 5 * time.Second
	reconnectMin      = 2 * time.Second
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

	mu      sync.Mutex
	session *Session
	running bool
	stopped chan struct{} // closed when Run returns
}

// NewClient prepares a client for the BMC at ep; call Login, then Start. Every connection goes
// through ep.
func NewClient(ep *bmc.Endpoint, user, password string) *Client {
	return &Client{ep: ep, user: user, web: NewWeb(ep, user, password), stopped: make(chan struct{})}
}

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
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := c.ep.DialPort(dctx, c.ep.HTTPSPort)
	cancel()
	if err != nil {
		return fmt.Errorf("KVM tunnel: %w", err)
	}
	conn, err := c.ep.TLSClient(ctx, raw, "https")
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("KVM tunnel TLS: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	r := bufio.NewReader(conn)
	if err := openTunnel(conn, r, c.ep.Host, l.WebPort, l.WebCookie); err != nil {
		_ = conn.Close()
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	clientIP := ""
	if a, ok := raw.LocalAddr().(*net.TCPAddr); ok {
		clientIP = a.IP.String()
	}
	s := newSession(conn, r, credentials{kvmToken: l.KVMToken, webCookie: l.WebCookie, user: c.user, clientIP: clientIP})
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.session = nil
		c.mu.Unlock()
	}()
	sink.SetStatus("")
	return s.Run(ctx, sink)
}

// Logout ends the web session once the KVM session has closed. Cancel Start's context first.
func (c *Client) Logout() {
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
