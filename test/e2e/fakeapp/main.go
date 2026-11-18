// Command fakeapp serves the real console UI and API with a scripted session manager and a fake
// console, for the browser tests in test/e2e. It contacts no BMC. Never shipped.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/config"
	"github.com/mhamzahkhan/ipmi-connect/internal/session"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
	"github.com/mhamzahkhan/ipmi-connect/internal/web"
)

const fakeFingerprint = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

type console struct {
	mu   sync.Mutex
	keys int
}

func (c *console) SendPointerEvent(byte, uint16, uint16) error { return nil }
func (c *console) SendKeyEvent(bool, uint16) error {
	c.mu.Lock()
	c.keys++
	c.mu.Unlock()
	return nil
}
func (c *console) RequestFrameRefresh() error         { return nil }
func (c *console) PowerControl(bmc.PowerAction) error { return nil }

// sessions asks for consent once (unknown certificate and unencrypted channels), then
// "connects" and streams a test picture.
type sessions struct {
	server *web.Server
	mu     sync.Mutex
	phase  string
	stop   context.CancelFunc
}

func (s *sessions) Status() session.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := session.Status{Phase: s.phase, Channels: []bmc.ChannelSecurity{}}
	if s.phase == session.PhaseConnected {
		st.Host, st.Vendor = "192.0.2.10", "supermicro"
		st.Channels = []bmc.ChannelSecurity{
			{Channel: "Web interface", Encryption: bmc.EncLegacyTLS, Protocol: "TLS 1.0", Cipher: "TLS_RSA_WITH_AES_256_CBC_SHA", Auth: bmc.AuthPinned, Fingerprint: fakeFingerprint},
			{Channel: "KVM", Encryption: bmc.EncNone, Auth: bmc.AuthNone, Note: "plain RFB"},
		}
		st.SecurityLevel = bmc.SecurityLevelOf(st.Channels)
	}
	return st
}

func (s *sessions) Connect(ctx context.Context, req session.Request) (session.Status, error) {
	if req.Password == "" {
		return session.Status{}, &session.InputError{Err: fmt.Errorf("enter the BMC password")}
	}
	if len(req.TrustFingerprints) == 0 || !req.AllowInsecureChannels || !req.AllowLegacyTLS {
		return session.Status{}, &session.ConsentRequired{
			Device: "192.0.2.10", Vendor: "supermicro",
			Certificates: []session.CertDecision{{
				Endpoint: "https", Status: "unknown", Protocol: "TLS 1.0", Cipher: "TLS_RSA_WITH_AES_256_CBC_SHA",
				Reason: "it is not signed by a trusted certificate authority (self-signed?)",
				Cert: trust.CertInfo{Fingerprint: fakeFingerprint, Subject: "CN=<b>bmc</b>", Issuer: "CN=<b>bmc</b>", Key: "RSA 1024",
					NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)},
			}},
			LegacyTLS:        []string{"https (TLS 1.0, TLS_RSA_WITH_AES_256_CBC_SHA)"},
			InsecureChannels: []string{"KVM: plain RFB <script>alert(1)</script>"},
		}
	}
	if !strings.EqualFold(req.TrustFingerprints[0], fakeFingerprint) || req.Password != "test-password" {
		return session.Status{}, fmt.Errorf("unexpected request")
	}
	c := &console{}
	s.server.Attach(c, bmc.SessionState{Host: "192.0.2.10", TargetType: bmc.TargetSupermicro, Model: "Fake", Connected: true, PowerState: "ON"})
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.phase, s.stop = session.PhaseConnected, cancel
	s.mu.Unlock()
	go func() {
		w, h := 320, 200
		pix := make([]byte, w*h*4)
		for n := 0; ; n++ {
			for i := 0; i < len(pix); i += 4 {
				pix[i], pix[i+1], pix[i+2], pix[i+3] = 200, byte(n), 40, 255
			}
			s.server.UpdateScreen(w, h, pix)
			select {
			case <-runCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return s.Status(), nil
}

func (s *sessions) Disconnect() {
	s.mu.Lock()
	stop := s.stop
	s.phase, s.stop = session.PhaseIdle, nil
	s.mu.Unlock()
	if stop != nil {
		stop()
		s.server.Detach()
	}
}

func main() {
	srv := web.NewServer(0, bmc.SessionState{PowerState: bmc.PowerStateUnknown})
	srv.Version = "e2e"
	srv.Desktop = true
	srv.Trust = trust.NewMemoryStore()
	srv.Profiles = config.NewMemoryStore()
	srv.Media = web.NewMediaRoots(os.TempDir())
	sess := &sessions{server: srv, phase: session.PhaseIdle}
	srv.Sessions = sess
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv.OnShutdown = stop
	_ = srv.Run(ctx, func(url string) { fmt.Println(url) })
}
