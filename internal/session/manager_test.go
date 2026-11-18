package session

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/config"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

// fakeDisplay records what the manager does to the web server.
type fakeDisplay struct {
	mu       sync.Mutex
	console  bmc.Console
	state    bmc.SessionState
	events   []string
	attached int
}

func (d *fakeDisplay) UpdateScreen(int, int, []byte) {}
func (d *fakeDisplay) SetStatus(string)              {}
func (d *fakeDisplay) SetFPS(int)                    {}
func (d *fakeDisplay) SetPower(string)               {}
func (d *fakeDisplay) Attach(c bmc.Console, st bmc.SessionState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.console, d.state = c, st
	d.attached++
	d.events = append(d.events, "attach "+st.Host)
}
func (d *fakeDisplay) Detach() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.console = nil
	d.events = append(d.events, "detach")
}
func (d *fakeDisplay) log() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

type nopConsole struct{}

func (nopConsole) SendPointerEvent(byte, uint16, uint16) error { return nil }
func (nopConsole) SendKeyEvent(bool, uint16) error             { return nil }
func (nopConsole) RequestFrameRefresh() error                  { return nil }
func (nopConsole) PowerControl(bmc.PowerAction) error          { return nil }

// fakeBMC is a TLS web server that records passwords posted to /login.
type fakeBMC struct {
	srv       *httptest.Server
	passwords chan string
}

func newFakeBMC(t *testing.T, cfg *tls.Config) *fakeBMC {
	t.Helper()
	f := &fakeBMC{passwords: make(chan string, 8)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			b, _ := io.ReadAll(r.Body)
			f.passwords <- string(b)
		}
	}))
	if cfg != nil {
		f.srv.TLS = cfg
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBMC) addr() string { return f.srv.Listener.Addr().String() }
func (f *fakeBMC) fingerprint() string {
	return trust.Fingerprint(f.srv.Certificate())
}

// fakeVendor logs in by posting the password over the endpoint's (policy-enforcing) client.
type fakeVendor struct {
	insecure []string
	fail     error
	block    chan struct{} // if set, Connect waits for it (or ctx)
	stopped  atomic.Int32
	order    *fakeDisplay
	stopLog  []string
}

func (v *fakeVendor) vendor() *Vendor {
	return &Vendor{
		Name: "dell",
		Probe: func(context.Context, *bmc.Endpoint) ([]trust.ProbeResult, []string, error) {
			return nil, v.insecure, nil
		},
		Connect: func(ctx context.Context, ep *bmc.Endpoint, user, password string, d Display) (*Live, error) {
			if v.block != nil {
				select {
				case <-v.block:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if v.fail != nil {
				return nil, v.fail
			}
			resp, err := ep.HTTPClient(5*time.Second).Post(ep.BaseURL()+"/login", "text/plain", strings.NewReader(password))
			if err != nil {
				return nil, err
			}
			resp.Body.Close()
			return &Live{
				Console: nopConsole{},
				Stop: func() {
					v.stopped.Add(1)
					if v.order != nil {
						v.stopLog = append(v.stopLog, fmt.Sprintf("stop after %v", v.order.log()))
					}
				},
				Channels: func() []bmc.ChannelSecurity {
					c, ok := ep.Policy.Connections()["https"]
					return []bmc.ChannelSecurity{tlsChannel("Web", c, ok)}
				},
			}, nil
		},
	}
}

func newManager(v *fakeVendor, store *trust.Store) (*Manager, *fakeDisplay) {
	d := &fakeDisplay{}
	v.order = d
	m := NewManager(Options{Trust: store, Display: d, Vendors: map[bmc.TargetType]*Vendor{bmc.TargetDell: v.vendor()}})
	return m, d
}

func req(addr string) Request {
	return Request{Address: addr, User: "root", Password: "hunter2", Vendor: "dell"}
}

func noPassword(t *testing.T, f *fakeBMC) {
	t.Helper()
	select {
	case p := <-f.passwords:
		t.Fatalf("password %q sent before trust was established", p)
	default:
	}
}

func TestConnectAsksBeforeTrustingAndPinsOnlyWhatWasConfirmed(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	store := trust.NewMemoryStore()
	v := &fakeVendor{}
	m, d := newManager(v, store)

	_, err := m.Connect(context.Background(), req(bmcSrv.addr()))
	var need *ConsentRequired
	if !errors.As(err, &need) || len(need.Certificates) != 1 {
		t.Fatalf("err = %v, want one certificate decision", err)
	}
	c := need.Certificates[0]
	if c.Status != "unknown" || c.Cert.Fingerprint != bmcSrv.fingerprint() || c.Endpoint != "https" {
		t.Errorf("decision = %+v", c)
	}
	noPassword(t, bmcSrv)
	if _, ok := store.Get("127.0.0.1"); ok {
		t.Error("something was stored before the user decided")
	}
	if m.Status().Phase != PhaseIdle {
		t.Errorf("phase = %s", m.Status().Phase)
	}

	// Confirming a fingerprint that was not observed pins nothing.
	r := req(bmcSrv.addr())
	r.TrustFingerprints = []string{strings.Repeat("00", 32)}
	if _, err := m.Connect(context.Background(), r); !errors.As(err, &need) {
		t.Fatalf("err = %v, want ConsentRequired", err)
	}
	noPassword(t, bmcSrv)

	r.TrustFingerprints = []string{bmcSrv.fingerprint()}
	st, err := m.Connect(context.Background(), r)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := <-bmcSrv.passwords; got != "hunter2" {
		t.Errorf("BMC got %q", got)
	}
	if dev, _ := store.Get("127.0.0.1"); len(dev.Fingerprints) != 1 || dev.Fingerprints[0] != bmcSrv.fingerprint() {
		t.Errorf("stored %+v", dev)
	}
	if st.Phase != PhaseConnected || len(st.Channels) != 1 || st.Channels[0].Auth != bmc.AuthPinned {
		t.Errorf("status = %+v", st)
	}
	if d.attached != 1 || d.state.Host != "127.0.0.1" {
		t.Errorf("display = %+v", d.log())
	}
}

func TestChangedCertificateNeedsExplicitReplacement(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	store := trust.NewMemoryStore()
	old := strings.Repeat("AB", 32)
	_ = store.Trust("127.0.0.1", old, false)
	m, _ := newManager(&fakeVendor{}, store)

	r := req(bmcSrv.addr())
	r.TrustFingerprints = []string{bmcSrv.fingerprint()} // confirming is not enough
	_, err := m.Connect(context.Background(), r)
	var need *ConsentRequired
	if !errors.As(err, &need) || need.Certificates[0].Status != "changed" {
		t.Fatalf("err = %v, want a changed-certificate decision", err)
	}
	if len(need.Certificates[0].Pinned) != 1 {
		t.Errorf("pinned = %v", need.Certificates[0].Pinned)
	}
	noPassword(t, bmcSrv)

	r.ReplaceFingerprints = true
	if _, err := m.Connect(context.Background(), r); err != nil {
		t.Fatalf("Connect after replacement: %v", err)
	}
	if dev, _ := store.Get("127.0.0.1"); len(dev.Fingerprints) != 1 || dev.Fingerprints[0] != bmcSrv.fingerprint() {
		t.Errorf("after replacement %+v", dev)
	}
}

func TestLegacyTLSAndInsecureChannelsNeedConsent(t *testing.T) {
	cert := rsaCert(t)
	bmcSrv := newFakeBMC(t, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS10, MaxVersion: tls.VersionTLS10, //nolint:gosec // an old BMC
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_256_CBC_SHA},
	})
	store := trust.NewMemoryStore()
	m, _ := newManager(&fakeVendor{insecure: []string{"KVM: plain"}}, store)
	fp := trust.Fingerprint(bmcSrv.srv.Certificate())

	r := req(bmcSrv.addr())
	r.TrustFingerprints = []string{fp}
	_, err := m.Connect(context.Background(), r)
	var need *ConsentRequired
	if !errors.As(err, &need) || len(need.LegacyTLS) != 1 || len(need.InsecureChannels) != 1 || len(need.Certificates) != 0 {
		t.Fatalf("err = %v, want legacy and insecure decisions only", err)
	}
	if !strings.Contains(need.LegacyTLS[0], "TLS 1.0") {
		t.Errorf("legacy = %v", need.LegacyTLS)
	}
	noPassword(t, bmcSrv)

	r.AllowLegacyTLS = true
	if _, err := m.Connect(context.Background(), r); !errors.As(err, &need) || len(need.InsecureChannels) != 1 {
		t.Fatalf("err = %v, want the insecure-channel decision still", err)
	}
	noPassword(t, bmcSrv)

	r.AllowInsecureChannels = true
	st, err := m.Connect(context.Background(), r)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if <-bmcSrv.passwords != "hunter2" {
		t.Error("wrong password")
	}
	if dev, _ := store.Get("127.0.0.1"); !dev.LegacyTLS || !dev.InsecureChannels {
		t.Errorf("consent not remembered: %+v", dev)
	}
	if st.Channels[0].Encryption != bmc.EncLegacyTLS || st.SecurityLevel != bmc.SecurityLegacy {
		t.Errorf("status = %+v", st)
	}

	// Remembered: the next connection needs no consent.
	m.Disconnect()
	if _, err := m.Connect(context.Background(), req(bmcSrv.addr())); err != nil {
		t.Fatalf("reconnect with remembered consent: %v", err)
	}
}

func trusted(t *testing.T, f *fakeBMC) *trust.Store {
	t.Helper()
	s := trust.NewMemoryStore()
	if err := s.Trust("127.0.0.1", f.fingerprint(), false); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDisconnectReleasesInputBeforeLoggingOut(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	v := &fakeVendor{}
	m, d := newManager(v, trusted(t, bmcSrv))
	if _, err := m.Connect(context.Background(), req(bmcSrv.addr())); err != nil {
		t.Fatal(err)
	}
	m.Disconnect()
	if v.stopped.Load() != 1 {
		t.Fatalf("stopped %d times", v.stopped.Load())
	}
	if len(v.stopLog) != 1 || !strings.Contains(v.stopLog[0], "detach") {
		t.Errorf("Stop ran before the display detached: %v", v.stopLog)
	}
	if st := m.Status(); st.Phase != PhaseIdle || st.Host != "" {
		t.Errorf("status after disconnect = %+v", st)
	}
	if d.console != nil {
		t.Error("console still attached")
	}
	m.Disconnect() // idempotent
}

func TestConnectingToAnotherBMCEndsTheFirst(t *testing.T) {
	a, b := newFakeBMC(t, nil), newFakeBMC(t, nil)
	store := trusted(t, a)
	_ = store.Trust("127.0.0.1", b.fingerprint(), true)
	v := &fakeVendor{}
	m, d := newManager(v, store)
	if _, err := m.Connect(context.Background(), req(a.addr())); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Connect(context.Background(), req(b.addr())); err != nil {
		t.Fatal(err)
	}
	if v.stopped.Load() != 1 {
		t.Errorf("first session stopped %d times", v.stopped.Load())
	}
	if got := d.log(); len(got) != 3 || got[1] != "detach" {
		t.Errorf("display events = %v", got)
	}
}

func TestConcurrentConnectIsRefusedAndCancelStopsTheAttempt(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	v := &fakeVendor{block: make(chan struct{})}
	m, d := newManager(v, trusted(t, bmcSrv))

	done := make(chan error, 1)
	go func() {
		_, err := m.Connect(context.Background(), req(bmcSrv.addr()))
		done <- err
	}()
	waitPhase(t, m, PhaseConnecting)
	if _, err := m.Connect(context.Background(), req(bmcSrv.addr())); !errors.Is(err, ErrBusy) {
		t.Errorf("second Connect err = %v, want ErrBusy", err)
	}
	m.Disconnect()
	if err := <-done; err == nil {
		t.Fatal("cancelled attempt succeeded")
	}
	if m.Status().Phase != PhaseIdle || d.attached != 0 {
		t.Errorf("after cancel: %+v, attached %d", m.Status(), d.attached)
	}
}

func TestFailedConnectLeavesNothingBehind(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	m, d := newManager(&fakeVendor{fail: errors.New("login failed: check the password")}, trusted(t, bmcSrv))
	_, err := m.Connect(context.Background(), req(bmcSrv.addr()))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v", err)
	}
	st := m.Status()
	if st.Phase != PhaseIdle || st.LastError == "" || d.attached != 0 {
		t.Errorf("status = %+v, attached %d", st, d.attached)
	}
}

func TestInputValidation(t *testing.T) {
	m, _ := newManager(&fakeVendor{}, trust.NewMemoryStore())
	for _, r := range []Request{
		{Address: "https://bmc", User: "u", Password: "p"},
		{Address: "192.0.2.1", User: "", Password: "p"},
		{Address: "192.0.2.1", User: "u", Password: ""},
		{Address: "192.0.2.1", User: "u", Password: "p", Vendor: "hp"},
	} {
		var in *InputError
		if _, err := m.Connect(context.Background(), r); !errors.As(err, &in) {
			t.Errorf("Connect(%v) err = %v, want InputError", r, err)
		}
	}
}

func TestRequestNeverPrintsPassword(t *testing.T) {
	r := Request{Address: "192.0.2.1", User: "root", Password: "hunter2"}
	for _, s := range []string{fmt.Sprint(r), fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r), fmt.Sprintf("%s", r)} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("formatted request leaks the password: %s", s)
		}
	}
}

func TestRecentsRecordedWithoutPassword(t *testing.T) {
	bmcSrv := newFakeBMC(t, nil)
	profiles := config.NewMemoryStore()
	d := &fakeDisplay{}
	m := NewManager(Options{Trust: trusted(t, bmcSrv), Display: d, Profiles: profiles,
		Vendors: map[bmc.TargetType]*Vendor{bmc.TargetDell: (&fakeVendor{}).vendor()}})
	if _, err := m.Connect(context.Background(), req(bmcSrv.addr())); err != nil {
		t.Fatal(err)
	}
	if r := profiles.Recents(); len(r) != 1 || r[0].User != "root" || r[0].Address != bmcSrv.addr() {
		t.Errorf("recents = %+v", r)
	}
}

func waitPhase(t *testing.T, m *Manager, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.Status().Phase != phase {
		if time.Now().After(deadline) {
			t.Fatalf("phase %s never reached (now %s)", phase, m.Status().Phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
