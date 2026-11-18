package dell

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/vmedia"
)

// defaultKVMPort is used for both channels until viewer.jnlp says otherwise.
const defaultKVMPort = 7578

// Timeouts and intervals. The 5s heartbeat keeps well inside the BMC's 30s control-channel watchdog.
//
// socketWriteTimeout is generous on purpose: a timed-out write leaves a TLS connection unusable.
const (
	httpTimeout        = 10 * time.Second
	socketWriteTimeout = 5 * time.Second
	heartbeatInterval  = 5 * time.Second
	controlDrainPoll   = 2 * time.Second
	reconnectSettle    = 1 * time.Second
)

// Variables rather than constants so tests can shorten them.
var (
	// apcpHandshakeTimeout bounds the APCP greeting, TLS handshake and channel login.
	apcpHandshakeTimeout = 10 * time.Second
	// packetBodyTimeout bounds the rest of a packet once its first byte has arrived.
	packetBodyTimeout = 3 * time.Second
)

// Client manages authentication, KVM sessions, and power controls for Dell C6220 II BMCs.
type Client struct {
	Host       string // the BMC's host name or address, for logs and the JNLP request
	ep         *bmc.Endpoint
	User       string
	Password   string
	HTTPClient *http.Client
	ST1Token   string
	ST2Token   string
	SessionMu  sync.Mutex
	IsLoggedIn bool

	// JNLP KVM credentials
	UserTokenStr string
	PassTokenStr string
	KMPort       int
	VPort        int

	// Active Channels
	CtrlConn              net.Conn
	VideoConn             net.Conn
	ClientRandom          uint32
	ApplianceSessionToken uint32
	AckCount              uint32
	heartbeatStop         chan struct{}
	ConnMu                sync.Mutex // guards CtrlConn, VideoConn and heartbeatStop
	ctrlWriteMu           sync.Mutex // serialises writes on CtrlConn
	videoWriteMu          sync.Mutex // serialises writes on VideoConn
	loggingOut            atomic.Bool
	// plainChannels records that the BMC refused TLS on an APCP channel and the user had
	// authorised unencrypted channels, for the security indicator.
	plainChannels atomic.Bool

	vmCreds chan []byte // 0x8434 payloads, handed from the control reader to openVMedia

	// Manager provides virtual media (bmc.VirtualMedia) over AVMP sessions that openVMedia opens.
	*vmedia.Manager
}

// NewClient creates a client for the BMC at ep. Every connection it makes goes through ep, under
// ep's trust policy.
func NewClient(ep *bmc.Endpoint, user, pass string) *Client {
	_ = ep.AllowPort(defaultKVMPort)
	httpClient := ep.HTTPClient(httpTimeout)
	c := &Client{
		Host:         ep.Host,
		ep:           ep,
		User:         user,
		Password:     pass,
		HTTPClient:   httpClient,
		KMPort:       defaultKVMPort,
		VPort:        defaultKVMPort,
		ClientRandom: randomUint31(),
		vmCreds:      make(chan []byte, 1),
	}
	c.Manager = vmedia.NewManager(c.openVMedia)
	return c
}

// Close closes any open control and video connections and stops the heartbeat.
func (c *Client) Close() {
	c.StopHeartbeat()

	c.ConnMu.Lock()
	defer c.ConnMu.Unlock()

	if c.VideoConn != nil {
		c.VideoConn.Close()
		c.VideoConn = nil
	}
	if c.CtrlConn != nil {
		c.CtrlConn.Close()
		c.CtrlConn = nil
	}
}

// Reconnect resets and re-establishes the Control and Video channels after a disconnect.
func (c *Client) Reconnect() error {
	log.Printf("[Dell] Reconnecting Control and Video channels...")
	c.Close()

	// Wait briefly to allow BMC socket cleanup
	time.Sleep(reconnectSettle)

	// One-time KVM tokens are single use; get fresh ones, logging in again if the web session expired.
	if err := c.RefreshKVMTokens(); err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}

	if err := c.ConnectControl(); err != nil {
		return fmt.Errorf("reconnect control channel failed: %w", err)
	}

	if err := c.ConnectVideo(); err != nil {
		return fmt.Errorf("reconnect video channel failed: %w", err)
	}

	if err := c.EnableVideo(); err != nil {
		log.Printf("[Dell] Reconnect EnableVideo warning: %v", err)
	}

	log.Printf("[Dell] Reconnection successful!")
	return nil
}

// PlainChannels reports whether an APCP channel ran without TLS.
func (c *Client) PlainChannels() bool { return c.plainChannels.Load() }

// randomUint31 is the session's client random: unpredictable, so another client cannot guess
// it to bind a video channel to this session.
func randomUint31() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return binary.BigEndian.Uint32(b[:]) & 0x7FFFFFFF
}
