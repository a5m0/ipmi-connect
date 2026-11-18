package trust

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert issues a certificate for names, signed by parent (self-signed when parent is nil).
func testCert(t *testing.T, cn string, parent *tls.Certificate, isCA bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return issue(t, cn, parent, isCA, key, &key.PublicKey)
}

// testRSACert is a self-signed RSA certificate, as old BMCs have (RSA key exchange suites need one).
func testRSACert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return issue(t, cn, nil, false, key, &key.PublicKey)
}

func issue(t *testing.T, cn string, parent *tls.Certificate, isCA bool, key crypto.Signer, pub any) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         isCA, BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(cn); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{cn}
	}
	signer, signerKey := tmpl, any(key)
	if parent != nil {
		signer = parent.Leaf
		signerKey = parent.PrivateKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, pub, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// tlsServer accepts connections with cfg and echoes a greeting, recording whether any
// application data arrived.
func tlsServer(t *testing.T, cfg *tls.Config) (addr string, gotData chan []byte) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gotData = make(chan []byte, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 64)
				n, _ := io.ReadAtLeast(c, buf, 1)
				if n > 0 {
					gotData <- buf[:n]
					_, _ = c.Write([]byte("ok"))
				}
			}()
		}
	}()
	return ln.Addr().String(), gotData
}

func dialer(addr string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// roundTrip connects with the policy and sends one message.
func roundTrip(t *testing.T, p *Policy, addr, serverName string) error {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, p.ClientConfig(serverName, "https"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("hello"))
	return err
}

func TestPolicyAcceptsCAVerifiedCertificate(t *testing.T) {
	ca := testCert(t, "Test CA", nil, true)
	leaf := testCert(t, "bmc.test", &ca, false)
	addr, _ := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{leaf}})
	roots := x509.NewCertPool()
	roots.AddCert(ca.Leaf)

	p := &Policy{Store: NewMemoryStore(), Device: "bmc.test", Roots: roots}
	if err := roundTrip(t, p, addr, "bmc.test"); err != nil {
		t.Fatalf("CA-verified certificate refused: %v", err)
	}
	if got := p.Connections()["https"].Auth; got != AuthCA {
		t.Errorf("auth = %q, want %q", got, AuthCA)
	}
	// The right CA but the wrong name is not CA-verified.
	p2 := &Policy{Store: NewMemoryStore(), Device: "other.test", Roots: roots}
	var unknown *UnknownCertificateError
	if err := roundTrip(t, p2, addr, "other.test"); !errors.As(err, &unknown) {
		t.Fatalf("wrong name: err = %v, want UnknownCertificateError", err)
	}
}

func TestPolicyRejectsUnknownSelfSignedCertificate(t *testing.T) {
	cert := testCert(t, "127.0.0.1", nil, false)
	addr, got := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{cert}})
	p := &Policy{Store: NewMemoryStore(), Device: "127.0.0.1"}

	err := roundTrip(t, p, addr, "127.0.0.1")
	var unknown *UnknownCertificateError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownCertificateError", err)
	}
	if unknown.Cert.Fingerprint != Fingerprint(cert.Leaf) {
		t.Errorf("fingerprint = %s, want %s", unknown.Cert.Fingerprint, Fingerprint(cert.Leaf))
	}
	if !unknown.Cert.SelfSigned {
		t.Error("self-signed certificate not reported as such")
	}
	select {
	case d := <-got:
		t.Fatalf("application data %q reached an untrusted server", d)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPolicyAcceptsPinnedAndRejectsChangedCertificate(t *testing.T) {
	pinned := testCert(t, "127.0.0.1", nil, false)
	other := testCert(t, "127.0.0.1", nil, false)
	store := NewMemoryStore()
	if err := store.Trust("127.0.0.1", Fingerprint(pinned.Leaf), false); err != nil {
		t.Fatal(err)
	}
	p := &Policy{Store: store, Device: "127.0.0.1"}

	addr, got := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{pinned}})
	if err := roundTrip(t, p, addr, "127.0.0.1"); err != nil {
		t.Fatalf("pinned certificate refused: %v", err)
	}
	if d := <-got; string(d) != "hello" {
		t.Errorf("server got %q", d)
	}
	if c := p.Connections()["https"]; c.Auth != AuthPinned || c.Fingerprint != Fingerprint(pinned.Leaf) {
		t.Errorf("connection = %+v", c)
	}

	addr2, got2 := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{other}})
	err := roundTrip(t, p, addr2, "127.0.0.1")
	var changed *CertificateChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want CertificateChangedError", err)
	}
	if changed.Cert.Fingerprint != Fingerprint(other.Leaf) || len(changed.Pinned) != 1 {
		t.Errorf("changed = %+v", changed)
	}
	if !IsTrustError(err) {
		t.Error("IsTrustError = false")
	}
	select {
	case d := <-got2:
		t.Fatalf("application data %q reached a server with a changed certificate", d)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLegacyTLSNeedsAuthorisation(t *testing.T) {
	cert := testRSACert(t, "127.0.0.1")
	legacy := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS10, MaxVersion: tls.VersionTLS10, //nolint:gosec // emulating an old BMC
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_256_CBC_SHA}, // the Supermicro's only suite
	}
	addr, _ := tlsServer(t, legacy)
	store := NewMemoryStore()
	_ = store.Trust("127.0.0.1", Fingerprint(cert.Leaf), false)

	p := &Policy{Store: store, Device: "127.0.0.1"}
	if err := roundTrip(t, p, addr, "127.0.0.1"); err == nil {
		t.Fatal("TLS 1.0 accepted without authorisation")
	}

	res, err := ProbeWithFallback(context.Background(), dialer(addr), "127.0.0.1", "https")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Legacy || res.Version != "TLS 1.0" {
		t.Errorf("probe = %+v, want legacy TLS 1.0", res)
	}
	if res.Cert.Fingerprint != Fingerprint(cert.Leaf) {
		t.Error("probe reported the wrong certificate")
	}

	if err := store.SetConsent("127.0.0.1", true, false); err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(t, p, addr, "127.0.0.1"); err != nil {
		t.Fatalf("authorised legacy TLS refused: %v", err)
	}
	if c := p.Connections()["https"]; !c.Legacy {
		t.Errorf("connection not marked legacy: %+v", c)
	}
}

func TestProbeReportsCertificateWithoutSendingData(t *testing.T) {
	cert := testCert(t, "127.0.0.1", nil, false)
	addr, got := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{cert}})
	res, err := ProbeWithFallback(context.Background(), dialer(addr), "127.0.0.1", "https")
	if err != nil {
		t.Fatal(err)
	}
	if res.Legacy || res.Version != "TLS 1.3" {
		t.Errorf("probe = %+v", res)
	}
	p := &Policy{Store: NewMemoryStore(), Device: "127.0.0.1"}
	var unknown *UnknownCertificateError
	if err := p.CheckCertificate("127.0.0.1", res); !errors.As(err, &unknown) {
		t.Errorf("CheckCertificate = %v, want unknown", err)
	}
	_ = p.Store.Trust("127.0.0.1", res.Cert.Fingerprint, false)
	if err := p.CheckCertificate("127.0.0.1", res); err != nil {
		t.Errorf("CheckCertificate after trust = %v", err)
	}
	select {
	case d := <-got:
		t.Fatalf("probe sent application data %q", d)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStorePersistsAndGuardsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "trust.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Repeat("ab", 32)
	b := "SHA256:" + strings.Repeat("CD:", 31) + "CD"
	if err := s.Trust("BMC.Example.", a, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust("bmc.example", b, false); err == nil {
		t.Fatal("second pin accepted without an explicit replacement")
	}
	if err := s.Trust("bmc.example:443", b, true); err != nil {
		t.Fatalf("additional pin: %v", err)
	}
	if err := s.SetConsent("bmc.example", true, false); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust file mode %o, want 600", perm)
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Errorf("trust dir mode %o, want 700", dir.Mode().Perm())
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := s2.Get("bmc.example")
	if !ok || len(d.Fingerprints) != 2 || !d.LegacyTLS || d.InsecureChannels {
		t.Fatalf("reloaded = %+v, %v", d, ok)
	}

	if err := s2.Replace("bmc.example", []string{b}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s2.Get("bmc.example"); len(d.Fingerprints) != 1 || d.Fingerprints[0] != FormatFingerprint(mustHex(t, strings.Repeat("cd", 32))) {
		t.Errorf("after replace = %+v", d)
	}
	if err := s2.Forget("bmc.example"); err != nil {
		t.Fatal(err)
	}
	s3, _ := OpenStore(path)
	if _, ok := s3.Get("bmc.example"); ok {
		t.Error("forgotten device still present after reload")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	fp, err := ParseFingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 0, 32)
	for _, part := range strings.Split(fp, ":") {
		var v byte
		for _, c := range part {
			v = v<<4 | byte(strings.IndexRune("0123456789ABCDEF", c))
		}
		b = append(b, v)
	}
	return b
}

func TestOpenStoreRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Error("corrupt trust store loaded without error")
	}
}

func TestParseFingerprint(t *testing.T) {
	want := strings.TrimSuffix(strings.Repeat("0A:", 32), ":")
	for _, in := range []string{
		strings.Repeat("0a", 32),
		strings.Repeat("0A", 32),
		"SHA256:" + want,
		"sha256:" + strings.ToLower(want),
		" " + strings.Repeat("0a ", 32),
	} {
		if got, err := ParseFingerprint(in); err != nil || got != want {
			t.Errorf("ParseFingerprint(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "zz", strings.Repeat("0a", 31), strings.Repeat("0a", 33), "SHA1:" + strings.Repeat("0a", 20)} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q) accepted", bad)
		}
	}
}

func TestDeviceKey(t *testing.T) {
	tests := map[string]string{
		"192.0.2.10":          "192.0.2.10",
		"192.0.2.10:443":      "192.0.2.10",
		"BMC.Example.":        "bmc.example",
		"bmc.example:8443":    "bmc.example",
		"2001:DB8::10":        "2001:db8::10",
		"[2001:db8::10]":      "2001:db8::10",
		"[2001:db8::10]:8443": "2001:db8::10",
		"fe80::1%eth0":        "fe80::1",
	}
	for in, want := range tests {
		if got := DeviceKey(in); got != want {
			t.Errorf("DeviceKey(%q) = %q, want %q", in, got, want)
		}
	}
}
