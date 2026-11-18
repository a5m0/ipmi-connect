// Command fakeapp serves the real console UI and API with a scripted session manager and a fake
// console, for the browser tests in test/e2e. It contacts no BMC. Never shipped.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/config"
	"github.com/a5m0/ipmi-connect/internal/session"
	"github.com/a5m0/ipmi-connect/internal/trust"
	"github.com/a5m0/ipmi-connect/internal/web"
)

// The values the scripted BMC presents. The defaults probe the UI's escaping of server text;
// FAKEAPP_DOCS=1 swaps in plain, realistic placeholders for documentation screenshots.
var (
	fakeFingerprint = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
	fakeSubject     = "CN=<b>bmc</b>"
	fakeInsecure    = []string{"KVM: plain RFB <script>alert(1)</script>"}
	docsMode        = false
	fakeNotBefore   = time.Now().Add(-time.Hour)
	fakeNotAfter    = time.Now().Add(time.Hour)
	fakeVersion     = "e2e"
)

func useDocsValues() {
	docsMode = true
	sum := sha256.Sum256([]byte("ipmi-connect documentation screenshot"))
	fakeFingerprint = trust.FormatFingerprint(sum[:])
	fakeSubject = "CN=192.0.2.10, O=ATEN, C=TW"
	// Old BMCs ship long-lived self-signed certificates generated at manufacture.
	fakeNotBefore = time.Date(2014, 1, 2, 0, 0, 0, 0, time.UTC)
	fakeNotAfter = time.Date(2034, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeVersion = "0.3.0"
	fakeInsecure = []string{
		"KVM: plain RFB, so the screen and every keystroke cross the network unencrypted",
		"Virtual media: RC4 under a key published in the vendor's client, so disk data is effectively unencrypted",
	}
}

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
			{Channel: "KVM", Encryption: bmc.EncNone, Auth: bmc.AuthNone, Note: "plain RFB: screen and keystrokes are unencrypted"},
			{Channel: "Virtual media", Encryption: bmc.EncWeak, Auth: bmc.AuthNone, Note: "RC4 under a publicly known key: effectively unencrypted"},
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
				Cert: trust.CertInfo{Fingerprint: fakeFingerprint, Subject: fakeSubject, Issuer: fakeSubject, Key: "RSA 1024",
					NotBefore: fakeNotBefore, NotAfter: fakeNotAfter},
			}},
			LegacyTLS:        []string{"https (TLS 1.0, TLS_RSA_WITH_AES_256_CBC_SHA)"},
			InsecureChannels: fakeInsecure,
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
	screen := loadScreen()
	go func() {
		w, h, interval := 320, 200, 200*time.Millisecond
		// FAKEAPP_STRESS=1024x768@30 streams a full-screen change at that rate instead, to
		// measure how fast a browser engine can draw the console.
		var fps int
		if _, err := fmt.Sscanf(os.Getenv("FAKEAPP_STRESS"), "%dx%d@%d", &w, &h, &fps); err == nil && fps > 0 {
			interval = time.Second / time.Duration(fps)
		} else {
			w, h = 320, 200
		}
		pix := make([]byte, w*h*4)
		for n := 0; ; n++ {
			for i := 0; i < len(pix); i += 4 {
				pix[i], pix[i+1], pix[i+2], pix[i+3] = 200, byte(n), 40, 255
				if fps > 0 { // a moving gradient, so every tile changes every frame
					pix[i+2] = byte(i/4%w + n)
				}
				if docsMode { // a dark screen, like a host at a text console
					pix[i], pix[i+1], pix[i+2] = 18, 18, 18
				}
			}
			if screen != nil { // a fixed picture instead of the generated one
				s.server.UpdateScreen(screen.Rect.Dx(), screen.Rect.Dy(), screen.Pix)
			} else {
				s.server.UpdateScreen(w, h, pix)
			}
			select {
			case <-runCtx.Done():
				return
			case <-time.After(interval):
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

// loadScreen reads FAKEAPP_SCREEN, a PNG shown as the host's screen in documentation shots.
func loadScreen() *image.RGBA {
	path := os.Getenv("FAKEAPP_SCREEN")
	if path == "" {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // test tool; the path comes from its operator
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil
	}
	rgba := image.NewRGBA(img.Bounds().Sub(img.Bounds().Min))
	draw.Draw(rgba, rgba.Rect, img, img.Bounds().Min, draw.Src)
	return rgba
}

func main() {
	if os.Getenv("FAKEAPP_DOCS") == "1" {
		useDocsValues()
	}
	srv := web.NewServer(0, bmc.SessionState{PowerState: bmc.PowerStateUnknown})
	srv.Version = fakeVersion
	srv.Desktop = true
	srv.Trust = trust.NewMemoryStore()
	srv.Profiles = config.NewMemoryStore()
	srv.Media = web.NewMediaRoots(os.TempDir())
	sess := &sessions{server: srv, phase: session.PhaseIdle}
	srv.Sessions = sess
	if os.Getenv("FAKEAPP_CONNECTED") == "1" { // start on the console, past the consent dialog
		if _, err := sess.Connect(context.Background(), session.Request{Password: "test-password",
			TrustFingerprints: []string{fakeFingerprint}, AllowLegacyTLS: true, AllowInsecureChannels: true}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv.OnShutdown = stop
	if os.Getenv("FAKEAPP_WINDOW") != "1" {
		_ = srv.Run(ctx, func(url string) { fmt.Println(url) })
		return
	}
	// The server in the background and the window on the main goroutine, as in the desktop build.
	links := make(chan string, 1)
	go func() {
		_ = srv.Run(ctx, func(url string) { fmt.Println(url); links <- url })
		os.Exit(0)
	}()
	if err := openWindow(<-links); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
