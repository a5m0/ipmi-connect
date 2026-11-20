package session

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

// bannerEndpoint reaches srv with its certificate pinned.
func bannerEndpoint(t *testing.T, srv *httptest.Server) *bmc.Endpoint {
	t.Helper()
	store := trust.NewMemoryStore()
	if err := store.Trust("127.0.0.1", trust.Fingerprint(srv.Certificate()), false); err != nil {
		t.Fatal(err)
	}
	ep, err := bmc.Resolve(context.Background(), srv.Listener.Addr().String(), &trust.Policy{Store: store, Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestDetectByBanner(t *testing.T) {
	megaracPage := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/index.html", http.StatusFound)
			return
		}
		// Like the ASMB8: no Server header, a gzip-encoded page titled "Megarac SP".
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		// The page must be large enough to be truly compressed: gzip stores tiny inputs verbatim.
		_, _ = io.WriteString(gz, "<!--"+strings.Repeat(";** (C) COPYRIGHT American Megatrends Inc. **;\n", 40)+"-->"+
			"<html><head><title>Megarac SP</title></head></html>")
		_ = gz.Close()
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    bmc.TargetType
	}{
		{"dell", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Server", "Mbedthis-Appweb/2.4.2") }, bmc.TargetDell},
		{"supermicro", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Server", "lighttpd/1.4.32") }, bmc.TargetSupermicro},
		{"megarac", megaracPage, bmc.TargetMegaRAC},
		{"unknown", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<title>Router</title>") }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(c.handler)
			defer srv.Close()
			if got := detectByBanner(context.Background(), bannerEndpoint(t, srv)); got != c.want {
				t.Errorf("detectByBanner = %q, want %q", got, c.want)
			}
		})
	}
}
