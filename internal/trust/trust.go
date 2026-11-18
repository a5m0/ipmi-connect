// Package trust decides whether a BMC's TLS certificate is accepted, and remembers the decisions
// the user made.
//
// A certificate is accepted when it chains to a trusted root for the name dialled (normal
// verification), or when its SHA-256 fingerprint is pinned for the device. Anything else fails
// the handshake with an error that carries the certificate's details, so the caller can show
// the fingerprint and ask the user. Nothing here ever accepts an unknown certificate silently:
// pins are added only through Store.Trust or Store.Replace, which the caller invokes after the
// user has explicitly confirmed.
//
// Certificate authentication (who the peer is) is separate from transport encryption (how the
// bytes are protected): a pinned certificate on TLS 1.0 authenticates the BMC but still runs over
// an obsolete protocol, and the caller reports both.
package trust

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Authentication methods recorded for an accepted connection.
const (
	AuthCA     = "ca"     // chains to a trusted root for the dialled name
	AuthPinned = "pinned" // fingerprint pinned for the device
)

// CertInfo describes a certificate for display and for the user's decision.
type CertInfo struct {
	Fingerprint string    `json:"fingerprint"` // SHA-256 of the DER certificate, see FormatFingerprint
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	SelfSigned  bool      `json:"self_signed"`
	Key         string    `json:"key"` // e.g. "RSA 2048"
	Names       []string  `json:"names,omitempty"`
}

// Describe summarises a certificate.
func Describe(cert *x509.Certificate) CertInfo {
	info := CertInfo{
		Fingerprint: Fingerprint(cert),
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		SelfSigned:  isSelfSigned(cert),
		Key:         keyDescription(cert),
	}
	info.Names = append(info.Names, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		info.Names = append(info.Names, ip.String())
	}
	return info
}

// isSelfSigned reports whether the certificate names itself as issuer and carries a valid
// signature by its own key (CheckSignatureFrom would also demand the CA flag, which BMC
// certificates rarely set).
func isSelfSigned(cert *x509.Certificate) bool {
	return bytes.Equal(cert.RawIssuer, cert.RawSubject) &&
		cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}

func keyDescription(cert *x509.Certificate) string {
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "ECDSA " + k.Curve.Params().Name
	case ed25519.PublicKey:
		return "Ed25519"
	}
	return cert.PublicKeyAlgorithm.String()
}

// Fingerprint returns the certificate's SHA-256 fingerprint as colon-separated upper-case hex,
// the form browsers and openssl show.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return FormatFingerprint(sum[:])
}

// FormatFingerprint formats a 32-byte digest.
func FormatFingerprint(sum []byte) string {
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// ParseFingerprint accepts a SHA-256 fingerprint with or without a "SHA256:" prefix, colons or
// spaces, in either case, and returns it in canonical form.
func ParseFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	if p, ok := strings.CutPrefix(strings.ToUpper(s), "SHA256:"); ok {
		s = p
	} else if p, ok := strings.CutPrefix(strings.ToUpper(s), "SHA-256:"); ok {
		s = p
	}
	s = strings.NewReplacer(":", "", " ", "", "-", "").Replace(s)
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("a SHA-256 fingerprint is 32 bytes of hex (64 hex digits)")
	}
	return FormatFingerprint(b), nil
}

// DeviceKey names a device in the trust store: the host without port, lower-cased, with IP
// addresses in canonical form.
func DeviceKey(host string) string {
	h := strings.TrimSpace(host)
	if addr, err := netip.ParseAddrPort(h); err == nil {
		return addr.Addr().WithZone("").String()
	}
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			h = h[1:i]
		}
	} else if strings.Count(h, ":") == 1 {
		h = h[:strings.Index(h, ":")]
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.WithZone("").String()
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// UnknownCertificateError means the certificate is neither CA-verified nor pinned, and no
// certificate is pinned for the device yet: the user may choose to trust it (trust on first use).
type UnknownCertificateError struct {
	Device   string
	Endpoint string
	Cert     CertInfo
	Reason   string // why normal verification failed
}

func (e *UnknownCertificateError) Error() string {
	return fmt.Sprintf("the certificate of %s (%s) is not trusted: %s; SHA-256 fingerprint %s",
		e.Device, e.Endpoint, e.Reason, e.Cert.Fingerprint)
}

// CertificateChangedError means the device presented a certificate other than the pinned one.
// This is what an interception attack looks like; it is also what a BMC firmware update or
// certificate regeneration looks like. Only an explicit replacement accepts it.
type CertificateChangedError struct {
	Device   string
	Endpoint string
	Cert     CertInfo
	Pinned   []string
}

func (e *CertificateChangedError) Error() string {
	return fmt.Sprintf("the certificate of %s (%s) has CHANGED: it now has SHA-256 fingerprint %s, "+
		"but %s is pinned; refusing to connect", e.Device, e.Endpoint, e.Cert.Fingerprint, strings.Join(e.Pinned, ", "))
}

// LegacyTLSRequiredError means the device only completes a handshake with obsolete TLS
// settings (TLS 1.0/1.1 or CBC/3DES suites), which the user has not authorised for it.
type LegacyTLSRequiredError struct {
	Device   string
	Endpoint string
	Version  string
	Cipher   string
}

func (e *LegacyTLSRequiredError) Error() string {
	return fmt.Sprintf("%s (%s) only supports obsolete TLS (%s, %s); allow legacy TLS for this device to connect",
		e.Device, e.Endpoint, e.Version, e.Cipher)
}

// IsTrustError reports whether err needs a trust decision from the user.
func IsTrustError(err error) bool {
	var u *UnknownCertificateError
	var c *CertificateChangedError
	var l *LegacyTLSRequiredError
	return errors.As(err, &u) || errors.As(err, &c) || errors.As(err, &l)
}

// legacyCipherSuites are the suites old BMCs need, offered only when legacy TLS is authorised.
// All are CBC-mode with SHA-1 MACs; the 3DES ones are also 64-bit block ciphers.
var legacyCipherSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

// modernCipherSuites are the TLS 1.2 suites offered in every configuration (TLS 1.3 suites are
// not configurable and always offered).
var modernCipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// IsLegacy reports whether a negotiated version and suite are obsolete.
func IsLegacy(version, suite uint16) bool {
	return version < tls.VersionTLS12 || slices.Contains(legacyCipherSuites, suite)
}

// baseConfig returns a client configuration with verification left to VerifyConnection.
//
// InsecureSkipVerify only disables crypto/tls's built-in chain check; VerifyConnection always
// runs and performs the CA-or-pin check itself, so no certificate is accepted unchecked.
func baseConfig(serverName string, legacy bool) *tls.Config {
	cfg := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // verification is done in VerifyConnection
		MinVersion:         tls.VersionTLS12,
		CipherSuites:       modernCipherSuites,
	}
	if legacy {
		cfg.MinVersion = tls.VersionTLS10 //nolint:gosec // only with the user's explicit authorisation
		cfg.CipherSuites = append(slices.Clone(modernCipherSuites), legacyCipherSuites...)
	}
	return cfg
}

// verifyChain performs normal WebPKI verification of the presented chain for name.
func verifyChain(certs []*x509.Certificate, name string, roots *x509.CertPool) error {
	if len(certs) == 0 {
		return errors.New("no certificate presented")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	host := name
	if addr, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		host = addr.String()
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, Roots: roots})
	return err
}
