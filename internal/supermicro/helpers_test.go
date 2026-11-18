package supermicro

import (
	"context"
	"crypto/x509"
	"testing"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

// testEndpoint reaches addr with cert pinned (nil: nothing pinned) and the unencrypted KVM and
// virtual media channels authorised, as after the user agreed to both.
func testEndpoint(t *testing.T, addr string, cert *x509.Certificate) *bmc.Endpoint {
	t.Helper()
	store := trust.NewMemoryStore()
	if cert != nil {
		if err := store.Trust("127.0.0.1", trust.Fingerprint(cert), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetConsent("127.0.0.1", false, true); err != nil {
		t.Fatal(err)
	}
	ep, err := bmc.Resolve(context.Background(), addr, &trust.Policy{Store: store, Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}
