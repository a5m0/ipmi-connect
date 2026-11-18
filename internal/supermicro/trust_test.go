package supermicro

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

func TestLoginSendsNoPasswordToUntrustedServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request %s reached a server whose certificate is not trusted", r.URL.Path)
	}))
	defer srv.Close()
	ep, err := bmc.Resolve(context.Background(), srv.Listener.Addr().String(),
		&trust.Policy{Store: trust.NewMemoryStore(), Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	err = NewWebSession(ep, "ADMIN", "secret").Login()
	var unknown *trust.UnknownCertificateError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownCertificateError", err)
	}
}

func TestInsecureChannelsNeedConsent(t *testing.T) {
	ep, err := bmc.Resolve(context.Background(), "127.0.0.1", &trust.Policy{Store: trust.NewMemoryStore(), Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(ep, "ADMIN", "pw")
	dialled := false
	c.dial = func(context.Context, int) (net.Conn, error) { dialled = true; return nil, errors.New("no") }

	sink := &recordingSink{}
	c.Run(context.Background(), sink) // returns instead of retrying
	if dialled {
		t.Error("KVM port dialled without consent")
	}
	if _, err := c.openVMedia(context.Background()); !errors.Is(err, ErrInsecureChannels) {
		t.Errorf("openVMedia err = %v, want ErrInsecureChannels", err)
	}
}
