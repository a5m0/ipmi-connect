package trust

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"sync"
)

// Connection describes one accepted TLS connection, for the security indicator.
type Connection struct {
	Endpoint    string `json:"endpoint"`
	Version     string `json:"version"`
	Cipher      string `json:"cipher"`
	Legacy      bool   `json:"legacy"`
	Auth        string `json:"auth"` // AuthCA or AuthPinned
	Fingerprint string `json:"fingerprint"`
}

// Policy is the trust decision procedure for one device. It is safe for concurrent use.
type Policy struct {
	Store  *Store
	Device string // DeviceKey of the BMC
	// AllowLegacyTLS permits obsolete TLS for this policy even without stored consent (used
	// for an explicit authorisation that the caller has not persisted).
	AllowLegacyTLS bool
	// Roots overrides the system roots (tests).
	Roots *x509.CertPool

	mu   sync.Mutex
	seen map[string]Connection // by endpoint
}

// Legacy reports whether obsolete TLS is authorised for the device.
func (p *Policy) Legacy() bool {
	if p.AllowLegacyTLS {
		return true
	}
	d, _ := p.Store.Get(p.Device)
	return d.LegacyTLS
}

// ClientConfig returns the TLS configuration for connecting to endpoint ("https", "kvm", ...)
// of the device, presenting serverName for SNI and name verification. Every handshake made
// with it runs Check.
func (p *Policy) ClientConfig(serverName, endpoint string) *tls.Config {
	cfg := baseConfig(serverName, p.Legacy())
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		conn, err := p.check(serverName, endpoint, cs.PeerCertificates, cs.Version, cs.CipherSuite, true)
		if err != nil {
			return err
		}
		p.record(conn)
		return nil
	}
	return cfg
}

// Connections returns what has been accepted under this policy, by endpoint.
func (p *Policy) Connections() map[string]Connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Connection, len(p.seen))
	for k, v := range p.seen {
		out[k] = v
	}
	return out
}

func (p *Policy) record(c Connection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]Connection{}
	}
	p.seen[c.Endpoint] = c
}

// check applies the policy to a presented chain.
func (p *Policy) check(serverName, endpoint string, certs []*x509.Certificate, version, suite uint16, enforceLegacy bool) (Connection, error) {
	if len(certs) == 0 {
		return Connection{}, errors.New("the BMC presented no certificate")
	}
	leaf := certs[0]
	fp := Fingerprint(leaf)
	conn := Connection{
		Endpoint: endpoint, Version: tls.VersionName(version), Cipher: tls.CipherSuiteName(suite),
		Legacy: IsLegacy(version, suite), Fingerprint: fp,
	}
	if enforceLegacy && conn.Legacy && !p.Legacy() {
		// Not reachable through ClientConfig, which offers legacy settings only when
		// authorised; a second line of defence.
		return Connection{}, &LegacyTLSRequiredError{Device: p.Device, Endpoint: endpoint, Version: conn.Version, Cipher: conn.Cipher}
	}
	dev, _ := p.Store.Get(p.Device)
	if slices.Contains(dev.Fingerprints, fp) {
		conn.Auth = AuthPinned
		return conn, nil
	}
	verr := verifyChain(certs, serverName, p.Roots)
	if verr == nil {
		conn.Auth = AuthCA
		return conn, nil
	}
	if len(dev.Fingerprints) > 0 {
		return Connection{}, &CertificateChangedError{Device: p.Device, Endpoint: endpoint, Cert: Describe(leaf), Pinned: dev.Fingerprints}
	}
	return Connection{}, &UnknownCertificateError{Device: p.Device, Endpoint: endpoint, Cert: Describe(leaf), Reason: shortReason(verr)}
}

// CheckCertificate applies the certificate part of the policy to a probe result: it returns the
// *UnknownCertificateError or *CertificateChangedError the real connection would fail with, or
// nil. Whether obsolete TLS is needed is r.Legacy, for the caller to weigh against Legacy().
func (p *Policy) CheckCertificate(serverName string, r ProbeResult) error {
	_, err := p.check(serverName, r.Endpoint, r.Chain, r.version, r.suite, false)
	return err
}

func shortReason(err error) string {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknown):
		return "it is not signed by a trusted certificate authority (self-signed?)"
	case errors.As(err, &hostname):
		return "it was issued for a different name"
	case errors.As(err, &invalid):
		return "it is invalid (" + invalid.Error() + ")"
	}
	return err.Error()
}

// ProbeResult is what a handshake revealed, without any application data being exchanged.
type ProbeResult struct {
	Endpoint string              `json:"endpoint"`
	Cert     CertInfo            `json:"cert"`
	Version  string              `json:"version"`
	Cipher   string              `json:"cipher"`
	Legacy   bool                `json:"legacy"`
	Chain    []*x509.Certificate `json:"-"`

	version, suite uint16
}

// errProbeDone stops a probe handshake once the certificate has been seen.
var errProbeDone = errors.New("probe complete")

// Probe performs a TLS handshake over conn to learn the peer's certificate and negotiated
// parameters, then closes conn. It never sends application data, so it is safe to run against
// an untrusted peer before any credential is sent. The handshake is abandoned as soon as the
// certificate has been received, so the peer never sees a completed session.
//
// legacy selects the obsolete-TLS offer; try modern first and only fall back after the user
// may be asked about it.
func Probe(ctx context.Context, conn net.Conn, serverName, endpoint string, legacy bool) (ProbeResult, error) {
	defer conn.Close()
	var res ProbeResult
	cfg := baseConfig(serverName, legacy)
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("the BMC presented no certificate")
		}
		res = ProbeResult{
			Endpoint: endpoint, Cert: Describe(cs.PeerCertificates[0]),
			Version: tls.VersionName(cs.Version), Cipher: tls.CipherSuiteName(cs.CipherSuite),
			Legacy: IsLegacy(cs.Version, cs.CipherSuite), Chain: cs.PeerCertificates,
			version: cs.Version, suite: cs.CipherSuite,
		}
		return errProbeDone
	}
	err := tls.Client(conn, cfg).HandshakeContext(ctx)
	if res.Chain != nil {
		return res, nil
	}
	if err == nil {
		err = errors.New("handshake finished without a certificate")
	}
	return ProbeResult{}, err
}

// ProbeWithFallback probes with modern settings and, if that handshake fails, with legacy
// settings on a fresh connection from dial. The result's Legacy field says which one worked.
func ProbeWithFallback(ctx context.Context, dial func(context.Context) (net.Conn, error), serverName, endpoint string) (ProbeResult, error) {
	conn, err := dial(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	res, modernErr := Probe(ctx, conn, serverName, endpoint, false)
	if modernErr == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return ProbeResult{}, ctx.Err()
	}
	conn, err = dial(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	res, legacyErr := Probe(ctx, conn, serverName, endpoint, true)
	if legacyErr != nil {
		return ProbeResult{}, errors.Join(modernErr, legacyErr)
	}
	return res, nil
}
