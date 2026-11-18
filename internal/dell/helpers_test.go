package dell

import (
	"context"
	"crypto/x509"
	"net"
	"strconv"
	"testing"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

// pinnedEndpoint reaches addr (host:port) with cert pinned, as after the user trusted it.
func pinnedEndpoint(t *testing.T, addr string, cert *x509.Certificate) *bmc.Endpoint {
	t.Helper()
	store := trust.NewMemoryStore()
	if cert != nil {
		if err := store.Trust("127.0.0.1", trust.Fingerprint(cert), false); err != nil {
			t.Fatal(err)
		}
	}
	ep, err := bmc.Resolve(context.Background(), addr, &trust.Policy{Store: store, Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// plainEndpoint reaches a fake APCP server without TLS, with unencrypted channels authorised.
func plainEndpoint(t *testing.T, host string, port int) *bmc.Endpoint {
	t.Helper()
	ep := pinnedEndpoint(t, net.JoinHostPort(host, strconv.Itoa(port)), nil)
	ep.Policy.AllowInsecureChannels = true
	return ep
}
