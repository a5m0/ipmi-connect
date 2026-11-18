package session

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

type netConn = net.Conn

// Vendor is one BMC family's implementation.
type Vendor struct {
	Name string
	// DetectPort, if open, identifies the vendor.
	DetectPort int
	// Probe learns the certificates of TLS channels besides the web server, and lists the
	// channels that are not effectively encrypted. It must not authenticate.
	Probe func(ctx context.Context, ep *bmc.Endpoint) (tls []trust.ProbeResult, insecure []string, err error)
	// Connect logs in and starts the session. Every connection must go through ep.
	Connect func(ctx context.Context, ep *bmc.Endpoint, user, password string, d Display) (*Live, error)
}

// Live is a running vendor session.
type Live struct {
	Host     string
	Console  bmc.Console
	Model    string
	Firmware string
	// Stop ends the session: stops the background loops, ejects virtual media and logs out.
	// It must be safe to call once and must not return before the BMC session is closed (or
	// has failed to close).
	Stop func()
	// Channels describes the security of each channel.
	Channels func() []bmc.ChannelSecurity
}

const (
	probeDialTimeout = 1500 * time.Millisecond
	probeHTTPTimeout = 5 * time.Second
)

// detectByPort identifies the vendor from its open KVM port. It sends nothing.
func detectByPort(ctx context.Context, ep *bmc.Endpoint, vendors map[bmc.TargetType]*Vendor) bmc.TargetType {
	for _, t := range []bmc.TargetType{bmc.TargetDell, bmc.TargetSupermicro} {
		v := vendors[t]
		if v == nil || v.DetectPort == 0 {
			continue
		}
		if err := ep.AllowPort(v.DetectPort); err != nil {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, probeDialTimeout)
		conn, err := ep.DialPort(dctx, v.DetectPort)
		cancel()
		if err == nil {
			conn.Close()
			return t
		}
	}
	return ""
}

// detectByBanner reads the web server's Server header. Call it only once the web server's
// certificate is trusted: ep's client enforces the policy.
func detectByBanner(ctx context.Context, ep *bmc.Endpoint) bmc.TargetType {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL()+"/", nil)
	if err != nil {
		return ""
	}
	resp, err := ep.HTTPClient(probeHTTPTimeout).Do(req)
	if err != nil {
		log.Printf("[Session] Vendor detection request: %v", err)
		return ""
	}
	defer resp.Body.Close()
	server := strings.ToLower(resp.Header.Get("Server"))
	switch {
	case strings.Contains(server, "appweb"):
		return bmc.TargetDell
	case strings.Contains(server, "lighttpd"):
		return bmc.TargetSupermicro
	}
	return ""
}

// tlsChannel describes a channel carried by an accepted TLS connection.
func tlsChannel(name string, c trust.Connection, ok bool) bmc.ChannelSecurity {
	if !ok {
		return bmc.ChannelSecurity{Channel: name, Encryption: bmc.EncTLS, Auth: bmc.AuthNone, Note: "not connected yet"}
	}
	ch := bmc.ChannelSecurity{
		Channel: name, Encryption: bmc.EncTLS, Protocol: c.Version, Cipher: c.Cipher,
		Auth: c.Auth, Fingerprint: c.Fingerprint,
	}
	if c.Legacy {
		ch.Encryption = bmc.EncLegacyTLS
		ch.Note = "obsolete TLS, allowed for this device"
	}
	return ch
}
