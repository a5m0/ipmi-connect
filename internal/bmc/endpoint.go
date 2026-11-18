package bmc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

// DefaultHTTPSPort is the BMC web server's port unless the address names another.
const DefaultHTTPSPort = 443

const (
	endpointDialTimeout = 5 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	maxResponseHeaders  = 64 << 10
	maxRedirects        = 5
)

// Endpoint is one BMC as the application may reach it: one address, resolved once, a set of
// allowed ports, and the device's TLS trust policy. Every connection to the BMC goes through it,
// so a hostname that later resolves elsewhere (DNS rebinding), a redirect, or a port named by the
// BMC itself cannot point the application at another machine.
//
// The Flatpak sandbox (or any OS sandbox sharing the host network) cannot restrict destinations
// per BMC; this is the application-level substitute, not a security boundary against a
// compromised process.
type Endpoint struct {
	// Host is the BMC as the user named it, without a port: the TLS server name and the HTTP
	// Host header.
	Host string
	// HTTPSPort is the web server port.
	HTTPSPort int
	// Policy decides which certificates are accepted.
	Policy *trust.Policy

	addr netip.Addr

	mu    sync.Mutex
	ports map[int]bool
}

// ParseAddress splits a BMC address typed by a user ("192.0.2.10", "bmc.example:8443",
// "[2001:db8::1]") into host and HTTPS port. It rejects URLs, paths, user info and anything that
// is not a hostname or IP literal.
func ParseAddress(address string) (host string, port int, err error) {
	a := strings.TrimSpace(address)
	if a == "" {
		return "", 0, errors.New("enter the BMC's address")
	}
	if strings.ContainsAny(a, "/\\@?#\x00 \t\r\n") || strings.Contains(a, "://") {
		return "", 0, errors.New("enter only the BMC's host name or IP address (no https://, path or user name)")
	}
	port = DefaultHTTPSPort
	host = a
	if h, p, splitErr := net.SplitHostPort(a); splitErr == nil {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid port %q", p)
		}
		host, port = h, n
	} else if strings.HasPrefix(a, "[") && strings.HasSuffix(a, "]") {
		host = a[1 : len(a)-1]
	}
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if addr.IsUnspecified() || addr.IsMulticast() {
			return "", 0, fmt.Errorf("%s is not a usable BMC address", host)
		}
		return addr.String(), port, nil
	}
	if !validHostname(host) {
		return "", 0, fmt.Errorf("%q is not a valid host name or IP address", host)
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")), port, nil
}

func validHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
			if !alnum && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// Resolve looks up the BMC once and returns an endpoint pinned to the first address, with only
// the HTTPS port allowed. Allow further ports before dialling them.
func Resolve(ctx context.Context, address string, policy *trust.Policy) (*Endpoint, error) {
	if policy == nil {
		return nil, errors.New("no trust policy")
	}
	host, port, err := ParseAddress(address)
	if err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		addrs, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if lerr != nil {
			return nil, fmt.Errorf("look up %s: %w", host, lerr)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("look up %s: no addresses", host)
		}
		addr = addrs[0].Unmap()
		if addr.IsUnspecified() || addr.IsMulticast() {
			return nil, fmt.Errorf("%s resolves to %s, which is not a usable BMC address", host, addr)
		}
	}
	return &Endpoint{Host: host, HTTPSPort: port, Policy: policy, addr: addr, ports: map[int]bool{port: true}}, nil
}

// Addr is the address every connection goes to.
func (e *Endpoint) Addr() netip.Addr { return e.addr }

// AllowPort permits connections to port, e.g. a KVM port named in the BMC's JNLP.
func (e *Endpoint) AllowPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is out of range", port)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ports[port] = true
	return nil
}

func (e *Endpoint) portAllowed(port int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ports[port]
}

// Authority is host[:port] for URLs, with IPv6 bracketed.
func (e *Endpoint) Authority() string {
	if e.HTTPSPort == DefaultHTTPSPort {
		if e.addrIsHost() && e.addr.Is6() {
			return "[" + e.Host + "]"
		}
		return e.Host
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.HTTPSPort))
}

func (e *Endpoint) addrIsHost() bool {
	a, err := netip.ParseAddr(e.Host)
	return err == nil && a == e.addr
}

// BaseURL is the BMC's https:// origin.
func (e *Endpoint) BaseURL() string { return (&url.URL{Scheme: "https", Host: e.Authority()}).String() }

// String names the endpoint in logs.
func (e *Endpoint) String() string { return e.Host }

// DialPort connects to port on the BMC's address.
func (e *Endpoint) DialPort(ctx context.Context, port int) (net.Conn, error) {
	if !e.portAllowed(port) {
		return nil, fmt.Errorf("refusing to connect to %s port %d: not a port this BMC uses", e.Host, port)
	}
	if port < 1 || port > 65535 { // allowed ports are range-checked too; keep the conversion safe here
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	d := net.Dialer{Timeout: endpointDialTimeout}
	return d.DialContext(ctx, "tcp", netip.AddrPortFrom(e.addr, uint16(port)).String())
}

// DialContext is a net.Dialer-style function for http.Transport: it accepts only this BMC's
// host (or its resolved address) and allowed ports, and always dials the resolved address.
func (e *Endpoint) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("refusing %s connection", network)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad port in %q", address)
	}
	if !e.isSelf(host) {
		return nil, fmt.Errorf("refusing to connect to %s: only %s may be contacted", host, e.Host)
	}
	return e.DialPort(ctx, port)
}

func (e *Endpoint) isSelf(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), e.Host) {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && a.WithZone("") == e.addr.WithZone("")
}

// TLSConfig returns the client TLS configuration for one of the BMC's TLS endpoints ("https",
// "kvm", "vmedia"), enforcing the trust policy.
func (e *Endpoint) TLSConfig(endpoint string) *tls.Config {
	return e.Policy.ClientConfig(e.Host, endpoint)
}

// TLSClient upgrades conn to TLS under the trust policy and completes the handshake.
func (e *Endpoint) TLSClient(ctx context.Context, conn net.Conn, endpoint string) (*tls.Conn, error) {
	tc := tls.Client(conn, e.TLSConfig(endpoint))
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		return nil, err
	}
	return tc, nil
}

// HTTPClient returns a client for the BMC's web server: no proxy (BMC traffic and its
// credentials never go to a proxy from the environment), the trust policy on every TLS
// connection, redirects only within the same https origin, and a cookie jar.
func (e *Endpoint) HTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            e.DialContext,
		TLSClientConfig:        e.TLSConfig("https"),
		TLSHandshakeTimeout:    tlsHandshakeTimeout,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: maxResponseHeaders,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: transport, Timeout: timeout, Jar: jar, CheckRedirect: e.checkRedirect}
}

// checkRedirect allows only same-origin https redirects: never to another host, another port,
// or plain http.
func (e *Endpoint) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	u := req.URL
	if u.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s: the BMC may only be reached over https", u.Scheme)
	}
	port := DefaultHTTPSPort
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if !e.isSelf(u.Hostname()) || port != e.HTTPSPort {
		return fmt.Errorf("refusing redirect to another host (%s)", u.Host)
	}
	return nil
}

// PlainKVMAllowed reports whether a KVM channel without TLS may be used: unencrypted channels
// are authorised and the KVM port has not been seen offering TLS before (which would make a
// plain offer look like a downgrade attack).
func (e *Endpoint) PlainKVMAllowed() bool {
	if e.Policy.AllowInsecureChannels {
		return true
	}
	d, _ := e.Policy.Store.Get(e.Policy.Device)
	return d.InsecureChannels && !d.KVMTLS
}

// InsecureChannelsAllowed reports whether the user has authorised channels without effective
// encryption (plain RFB, fixed-key RC4, APCP without TLS) for this device.
func (e *Endpoint) InsecureChannelsAllowed() bool {
	d, _ := e.Policy.Store.Get(e.Policy.Device)
	return d.InsecureChannels || e.Policy.AllowInsecureChannels
}
