package supermicro

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/vmedia"
)

const (
	dialTimeout       = 5 * time.Second
	handshakeTimeout  = 15 * time.Second
	reconnectMin      = 2 * time.Second
	reconnectMax      = 30 * time.Second
	stableSession     = 30 * time.Second // a session this long resets the backoff
	logoutWaitTimeout = 5 * time.Second
)

// inBandPower maps power actions to the KVM socket's 0x1a codes. Power cycle has none; it goes
// through the web CGI.
var inBandPower = map[bmc.PowerAction]byte{
	bmc.PowerOff: powerOff, bmc.PowerOn: powerOn, bmc.PowerReset: powerReset, bmc.PowerSoftOff: powerSoftOff,
}

// Client keeps a Supermicro KVM session up, reconnecting with a fresh one-time ticket as needed,
// and implements bmc.Console on whichever session is current.
type Client struct {
	host string
	ep   *bmc.Endpoint
	web  *WebSession
	dial func(ctx context.Context, port int) (net.Conn, error)

	mu      sync.Mutex
	session *Session
	running bool
	stopped chan struct{} // closed when Run returns

	// Manager provides virtual media (bmc.VirtualMedia) over ATEN Virtual Storage sessions.
	*vmedia.Manager
}

// NewClient prepares a client for the BMC at ep; call Login, then Run. Every connection goes
// through ep.
func NewClient(ep *bmc.Endpoint, user, password string) *Client {
	_ = ep.AllowPort(defaultVMediaPort)
	c := &Client{
		host:    ep.Host,
		ep:      ep,
		web:     NewWebSession(ep, user, password),
		stopped: make(chan struct{}),
		dial: func(ctx context.Context, port int) (net.Conn, error) {
			if err := ep.AllowPort(port); err != nil {
				return nil, err
			}
			dctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return ep.DialPort(dctx, port)
		},
	}
	c.Manager = vmedia.NewManager(c.openVMedia)
	return c
}

// Login authenticates to the web interface; call it before Run so bad credentials fail fast.
func (c *Client) Login() error { return c.web.Login() }

func nextDelay(d time.Duration) time.Duration { return min(2*d, reconnectMax) }

// Start runs the client in the background. It marks the client running before returning, so a
// Logout straight after still waits for the KVM socket to close.
func (c *Client) Start(ctx context.Context, sink Sink) {
	c.markRunning()
	go c.Run(ctx, sink)
}

func (c *Client) markRunning() {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
}

// Run keeps a KVM session up until ctx ends. Call it once.
func (c *Client) Run(ctx context.Context, sink Sink) {
	c.markRunning()
	defer close(c.stopped)

	delay := reconnectMin
	for {
		started := time.Now()
		err := c.serveOnce(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrInsecureChannels) {
			log.Printf("[Supermicro] %v", err)
			sink.SetStatus(err.Error())
			return
		}
		if time.Since(started) > stableSession {
			delay = reconnectMin
		}
		log.Printf("[Supermicro] KVM session ended (%v); reconnecting in %v", err, delay)
		sink.SetStatus("Reconnecting to the KVM…")
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = nextDelay(delay)
	}
}

// ErrInsecureChannels means the KVM (plain RFB) or virtual media (RC4 under a published key)
// channel would be used without the user's authorisation for this device.
var ErrInsecureChannels = errors.New("this BMC's KVM and virtual media channels are not encrypted; " +
	"allow unencrypted channels for this device to connect")

// serveOnce fetches a ticket, connects, authenticates and serves one session.
func (c *Client) serveOnce(ctx context.Context, sink Sink) error {
	if !c.ep.InsecureChannelsAllowed() {
		return ErrInsecureChannels
	}
	ticket, err := c.web.FetchTicket(c.host)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutting down: never open a new web session
		}
		// The web session may have expired: log in again once.
		if lerr := c.web.Login(); lerr != nil {
			return fmt.Errorf("re-login: %w", lerr)
		}
		if ticket, err = c.web.FetchTicket(c.host); err != nil {
			return err
		}
	}
	if ticket.TLS {
		return errors.New(`the BMC has "KVM SSL" enabled, which is not supported; disable it in the BMC web UI`)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	conn, err := c.dial(ctx, ticket.Port)
	if err != nil {
		return fmt.Errorf("KVM port %d: %w", ticket.Port, err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	r := bufio.NewReader(conn)
	info, err := handshake(r, conn, ticket)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("KVM handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	log.Printf("[Supermicro] KVM connected to %q (video %v, keyboard/mouse %v)", info.Name, info.Video, info.KeyboardMouse)
	sink.SetStatus("")

	s := newSession(conn, r, info)
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.session = nil
		c.mu.Unlock()
	}()
	return s.Run(ctx, sink)
}

// Logout ends the web session once the KVM socket has closed (a web logout would kick it).
func (c *Client) Logout() {
	if c.Manager != nil {
		c.Close()
	}
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if running {
		select {
		case <-c.stopped:
		case <-time.After(logoutWaitTimeout):
			log.Printf("[Supermicro] KVM session did not close in time; logging out anyway")
		}
	}
	c.web.Logout()
}

func (c *Client) current() (*Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil, errors.New("supermicro KVM not connected")
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

// PowerControl implements bmc.Console: in-band when a KVM session is up, otherwise via the web CGI.
func (c *Client) PowerControl(action bmc.PowerAction) error {
	if code, ok := inBandPower[action]; ok {
		if s, err := c.current(); err == nil {
			log.Printf("[Supermicro] Power %s sent over the KVM connection", action)
			return s.power(code)
		}
	}
	return c.web.PowerCGI(action)
}

// PowerStatus reports "ON" or "OFF" from the web interface.
func (c *Client) PowerStatus() (string, error) { return c.web.PowerStatus() }

// PowerState implements bmc.PowerReader.
func (c *Client) PowerState() (string, error) { return c.web.PowerStatus() }
