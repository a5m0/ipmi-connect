package dell

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a5m0/ipmi-connect/internal/trust"
)

// fakeTLSAPCP answers the APCP greeting offering TLS, upgrades with cert, and reports whatever
// application bytes arrive afterwards.
func fakeTLSAPCP(t *testing.T, cert tls.Certificate) (host string, port int, got chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan []byte, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.ReadFull(conn, make([]byte, 53)); err != nil {
					return
				}
				reply := make([]byte, 53)
				copy(reply, "APCP")
				binary.BigEndian.PutUint32(reply[14:18], 5) // plain | TLS
				if _, err := conn.Write(reply); err != nil {
					return
				}
				tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				buf := make([]byte, controlAuthLen)
				if n, _ := io.ReadFull(tc, buf); n > 0 {
					got <- buf[:n]
					_, _ = tc.Write(loginResponse(0, 0x1234))
				}
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, got
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS() // generates a self-signed certificate for 127.0.0.1
	t.Cleanup(srv.Close)
	return srv.TLS.Certificates[0]
}

func TestConnectControlRefusesUnknownKVMCertificateBeforeSendingTokens(t *testing.T) {
	cert := selfSigned(t)
	host, port, got := fakeTLSAPCP(t, cert)
	ep := pinnedEndpoint(t, host, nil) // nothing pinned
	_ = ep.AllowPort(port)
	c := &Client{Host: host, ep: ep, VPort: port, UserTokenStr: "secret-user", PassTokenStr: "secret-pass"}

	err := c.ConnectControl()
	var unknown *trust.UnknownCertificateError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownCertificateError", err)
	}
	select {
	case b := <-got:
		t.Fatalf("tokens reached an untrusted KVM port: %q", b)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestConnectControlOverPinnedTLS(t *testing.T) {
	cert := selfSigned(t)
	host, port, got := fakeTLSAPCP(t, cert)
	cert.Leaf, _ = parseLeaf(cert)
	ep := pinnedEndpoint(t, host, cert.Leaf)
	_ = ep.AllowPort(port)
	c := &Client{Host: host, ep: ep, VPort: port, UserTokenStr: "u", PassTokenStr: "p"}
	defer c.Close()

	if err := c.ConnectControl(); err != nil {
		t.Fatalf("ConnectControl: %v", err)
	}
	if b := <-got; len(b) != controlAuthLen {
		t.Errorf("cb packet %d bytes", len(b))
	}
	if conn := ep.Policy.Connections()["kvm"]; conn.Auth != trust.AuthPinned {
		t.Errorf("kvm connection = %+v", conn)
	}
	if c.PlainChannels() {
		t.Error("TLS channel reported as plain")
	}

	res, offered, err := ProbeKVM(context.Background(), ep, port)
	if err != nil || !offered || res.Cert.Fingerprint != trust.Fingerprint(cert.Leaf) {
		t.Errorf("ProbeKVM = %+v, %v, %v", res, offered, err)
	}
}

func parseLeaf(c tls.Certificate) (*x509.Certificate, error) {
	return x509.ParseCertificate(c.Certificate[0])
}

func TestPlainAPCPNeedsConsent(t *testing.T) {
	host, port := fakeAPCP(t, func(conn net.Conn) {
		buf := make([]byte, controlAuthLen)
		if n, _ := io.ReadFull(conn, buf); n > 0 {
			t.Errorf("tokens sent over an unauthorised plain channel")
		}
	})
	ep := pinnedEndpoint(t, host, nil)
	_ = ep.AllowPort(port)
	c := &Client{Host: host, ep: ep, VPort: port, UserTokenStr: "u", PassTokenStr: "p"}
	if err := c.ConnectControl(); !errors.Is(err, ErrPlainAPCP) {
		t.Fatalf("err = %v, want ErrPlainAPCP", err)
	}
}

func TestConnectControlRejectsOversizedTokens(t *testing.T) {
	host, port := fakeAPCP(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	c := &Client{Host: host, ep: plainEndpoint(t, host, port), VPort: port,
		UserTokenStr: strings.Repeat("u", 97), PassTokenStr: strings.Repeat("p", 300)}
	if err := c.ConnectControl(); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchKVMParametersValidatesJNLP(t *testing.T) {
	for name, args := range map[string]string{
		"long token": `<argument>user=` + strings.Repeat("1", 97) + `</argument><argument>passwd=2</argument>`,
		"bad port":   `<argument>user=1</argument><argument>passwd=2</argument><argument>vport=70000</argument>`,
		"zero port":  `<argument>user=1</argument><argument>passwd=2</argument><argument>kmport=0</argument>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reply(w, `<jnlp><application-desc>`+args+`</application-desc></jnlp>`)
			}))
			defer srv.Close()
			c := NewClient(pinnedEndpoint(t, srv.Listener.Addr().String(), srv.Certificate()), "root", "pw")
			c.IsLoggedIn, c.ST1Token = true, "abc"
			if err := c.FetchKVMParameters(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestReadBodyIsBounded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxBody+10))
	}))
	defer srv.Close()
	c := NewClient(pinnedEndpoint(t, srv.Listener.Addr().String(), srv.Certificate()), "root", "pw")
	c.IsLoggedIn, c.ST1Token = true, "abc"
	if _, err := c.GetPowerState(); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}
