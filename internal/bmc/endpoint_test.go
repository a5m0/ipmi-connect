package bmc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/trust"
)

func TestParseAddress(t *testing.T) {
	good := []struct {
		in   string
		host string
		port int
	}{
		{"192.0.2.10", "192.0.2.10", 443},
		{" 192.0.2.10:8443 ", "192.0.2.10", 8443},
		{"BMC.Example.", "bmc.example", 443},
		{"bmc-1.example:443", "bmc-1.example", 443},
		{"2001:db8::10", "2001:db8::10", 443},
		{"[2001:db8::10]", "2001:db8::10", 443},
		{"[2001:db8::10]:8443", "2001:db8::10", 8443},
	}
	for _, tt := range good {
		h, p, err := ParseAddress(tt.in)
		if err != nil || h != tt.host || p != tt.port {
			t.Errorf("ParseAddress(%q) = %q, %d, %v; want %q, %d", tt.in, h, p, err, tt.host, tt.port)
		}
	}
	for _, bad := range []string{
		"", "https://192.0.2.10", "192.0.2.10/login", "root@192.0.2.10", "bmc example", "bmc.example:0",
		"bmc.example:70000", "0.0.0.0", "224.0.0.1", "-bad.example", "bmc..example", "a\x00b", "bmc?x", "evil.example#",
	} {
		if _, _, err := ParseAddress(bad); err == nil {
			t.Errorf("ParseAddress(%q) accepted", bad)
		}
	}
}

func testEndpoint(t *testing.T, srv *httptest.Server) *Endpoint {
	t.Helper()
	store := trust.NewMemoryStore()
	if srv.TLS != nil {
		if err := store.Trust("127.0.0.1", trust.Fingerprint(srv.Certificate()), false); err != nil {
			t.Fatal(err)
		}
	}
	ep, err := Resolve(context.Background(), srv.Listener.Addr().String(), &trust.Policy{Store: store, Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestEndpointRefusesOtherHostsAndPorts(t *testing.T) {
	ep, err := Resolve(context.Background(), "127.0.0.1", &trust.Policy{Store: trust.NewMemoryStore(), Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ep.DialContext(ctx, "tcp", "192.0.2.99:443"); err == nil || !strings.Contains(err.Error(), "only 127.0.0.1") {
		t.Errorf("foreign host: %v", err)
	}
	if _, err := ep.DialContext(ctx, "tcp", "evil.example:443"); err == nil {
		t.Error("foreign name accepted")
	}
	if _, err := ep.DialPort(ctx, 22); err == nil || !strings.Contains(err.Error(), "port 22") {
		t.Errorf("unlisted port: %v", err)
	}
	if _, err := ep.DialContext(ctx, "udp", "127.0.0.1:443"); err == nil {
		t.Error("udp accepted")
	}
	if err := ep.AllowPort(0); err == nil {
		t.Error("port 0 allowed")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ep.AllowPort(port); err != nil {
		t.Fatal(err)
	}
	conn, err := ep.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("allowed port: %v", err)
	}
	conn.Close()
}

func TestEndpointHTTPClientPinsAndRefusesForeignRedirects(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("fine"))
		case "/same":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/plain":
			http.Redirect(w, r, "http://"+r.Host+"/ok", http.StatusFound)
		case "/away":
			http.Redirect(w, r, other.URL+"/ok", http.StatusFound)
		case "/name":
			http.Redirect(w, r, "https://evil.example/ok", http.StatusFound)
		}
	}))
	defer srv.Close()
	ep := testEndpoint(t, srv)
	client := ep.HTTPClient(0)

	for path, wantOK := range map[string]bool{"/ok": true, "/same": true, "/plain": false, "/away": false, "/name": false} {
		resp, err := client.Get(ep.BaseURL() + path)
		if wantOK {
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			resp.Body.Close()
		} else if err == nil || !strings.Contains(err.Error(), "refusing") {
			if resp != nil {
				resp.Body.Close()
			}
			t.Errorf("%s: redirect followed (err %v)", path, err)
		}
	}
}

func TestEndpointHTTPClientRejectsUnpinnedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached a server whose certificate is not trusted")
	}))
	defer srv.Close()
	ep, err := Resolve(context.Background(), srv.Listener.Addr().String(), &trust.Policy{Store: trust.NewMemoryStore(), Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ep.BaseURL()+"/login", strings.NewReader("password=secret"))
	_, err = ep.HTTPClient(0).Do(req)
	var unknown *trust.UnknownCertificateError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownCertificateError", err)
	}
}

func TestEndpointAuthority(t *testing.T) {
	p := &trust.Policy{Store: trust.NewMemoryStore()}
	for in, want := range map[string]string{
		"192.0.2.10":          "https://192.0.2.10",
		"192.0.2.10:8443":     "https://192.0.2.10:8443",
		"2001:db8::10":        "https://[2001:db8::10]",
		"[2001:db8::10]:8443": "https://[2001:db8::10]:8443",
	} {
		ep, err := Resolve(context.Background(), in, p)
		if err != nil {
			t.Fatal(err)
		}
		if got := ep.BaseURL(); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
