package megarac

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

// oneConnListener hands a single connection to http.Serve.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// peekedConn replays the bytes peeked to tell a tunnel from an HTTP request.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// fakeBMCServer serves the /rpc pages and the KVM tunnel on one TLS port, like the ASMB8.
type fakeBMCServer struct {
	t       *testing.T
	web     *fakeWeb
	ln      net.Listener
	srv     *httptest.Server
	mu      sync.Mutex
	tunnels int
	events  []string         // media state and STOP_SESSION messages on the KVM tunnel
	byKVM   map[int][]string // the same, by KVM tunnel (1 = the first)
	video   net.Conn         // the current KVM tunnel
	served  int              // the last KVM tunnel to have had its first frame
	closing chan struct{}
	media   chan *fakeMediaBMC // media tunnels, after their tunnel request
}

func newFakeBMCServer(t *testing.T) *fakeBMCServer {
	f := &fakeBMCServer{t: t, web: &fakeWeb{password: "pw", cookie: "COOKIE123", csrf: "CSRF456", power: 1}, closing: make(chan struct{}), media: make(chan *fakeMediaBMC, 4)}
	// httptest provides the certificate; connections are accepted here and split by their first bytes.
	f.srv = httptest.NewUnstartedServer(f.web)
	f.srv.StartTLS()
	cert := f.srv.TLS.Certificates[0]
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { close(f.closing); _ = ln.Close(); f.srv.Close() })
	go f.accept()
	return f
}

func (f *fakeBMCServer) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

func (f *fakeBMCServer) serve(c net.Conn) {
	r := bufio.NewReader(c)
	first, err := r.Peek(7)
	if err != nil {
		c.Close()
		return
	}
	if string(first) != "CONNECT" {
		l := &oneConnListener{conn: peekedConn{c, r}, done: f.closing}
		_ = http.Serve(l, f.web)
		return
	}
	// Two request blocks, each ending with a blank line; the second names the service.
	service := ""
	for i := 0; i < 2; i++ {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				c.Close()
				return
			}
			if line == "\r\n" {
				break
			}
			if i == 1 {
				service = strings.Fields(line)[1]
			}
		}
	}
	if service != serviceVideo {
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
		f.media <- &fakeMediaBMC{t: f.t, service: service, conn: c, r: r}
		return
	}
	defer c.Close()
	f.mu.Lock()
	f.tunnels++
	n := f.tunnels
	f.video = c
	f.mu.Unlock()
	_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	b := &fakeBMC{t: f.t, conn: c, r: r}
	b.handshake(validationOK)
	frames := readRecording(f.t)
	b.frame(frames[0])
	f.mu.Lock()
	f.served = n
	f.mu.Unlock()
	// Record the client's media state and STOP_SESSION messages until it leaves.
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		m, err := readMessage(r)
		if err != nil {
			return
		}
		switch m.Type {
		case typeMediaState:
			f.event(n, fmt.Sprintf("media %d", m.Status))
		case typeStopSession:
			f.event(n, "stop")
			return
		}
	}
}

func (f *fakeBMCServer) event(kvm int, e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	if f.byKVM == nil {
		f.byKVM = map[int][]string{}
	}
	f.byKVM[kvm] = append(f.byKVM[kvm], e)
}

// kvmEvents returns the messages seen on KVM tunnel n, joined.
func (f *fakeBMCServer) kvmEvents(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.byKVM[n], ", ")
}

func (f *fakeBMCServer) kvmTunnels() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tunnels
}

// servedKVM returns the last KVM tunnel that got through its handshake and first frame.
func (f *fakeBMCServer) servedKVM() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served
}

// dropKVM closes the current KVM tunnel, as a network failure would.
func (f *fakeBMCServer) dropKVM() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.video != nil {
		_ = f.video.Close()
	}
}

// fastReconnect shortens the client's reconnect delay for one test.
func fastReconnect(t *testing.T, d time.Duration) {
	old := reconnectMin
	reconnectMin = d
	t.Cleanup(func() { reconnectMin = old })
}

// startMappedCD starts a client on f, maps a CD image and returns the client, the BMC end of
// the media tunnel, the image content and a teardown that leaves as the app does.
func startMappedCD(t *testing.T, f *fakeBMCServer) (*Client, *fakeMediaBMC, []byte, func()) {
	t.Helper()
	ep := testEndpoint(t, f.ln.Addr().String(), f.srv.Certificate())
	c := NewClient(ep, "admin", "pw")
	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	stop := func() { once.Do(func() { c.CloseMedia(); cancel(); c.Logout() }) }
	t.Cleanup(stop)
	c.Start(ctx, sink)
	waitFor(t, "a frame through the tunnel", func() bool { n, _, _, _ := sink.snapshot(); return n > 0 })
	iso := filepath.Join(t.TempDir(), "test.iso")
	content := bytes.Repeat([]byte("reconnect"), 4*2048/9+1)[:4*2048]
	if err := os.WriteFile(iso, content, 0o600); err != nil {
		t.Fatal(err)
	}
	im, err := c.AddImage(iso, true)
	if err != nil {
		t.Fatal(err)
	}
	mapped := make(chan error, 1)
	go func() { mapped <- c.MapImage(im.ID, bmc.TargetCD) }()
	var m *fakeMediaBMC
	select {
	case m = <-f.media:
	case <-time.After(5 * time.Second):
		t.Fatal("no media tunnel")
	}
	m.accept(1)
	if err := <-mapped; err != nil {
		t.Fatalf("MapImage: %v", err)
	}
	waitFor(t, "MEDIA_STATE 1 on the first KVM session", func() bool { return f.kvmEvents(1) == "media 1" })
	return c, m, content, stop
}

func (f *fakeBMCServer) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestClientConnectsThroughTheTunnel(t *testing.T) {
	f := newFakeBMCServer(t)
	ep := testEndpoint(t, f.ln.Addr().String(), f.srv.Certificate())
	c := NewClient(ep, "admin", "pw")
	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx, sink)
	waitFor(t, "a frame through the tunnel", func() bool { n, _, _, _ := sink.snapshot(); return n > 0 })
	if st, err := c.PowerState(); err != nil || st != "ON" {
		t.Errorf("PowerState = %q, %v", st, err)
	}
	cancel()
	c.Logout()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tunnels != 1 {
		t.Errorf("%d tunnels, want 1", f.tunnels)
	}
	if len(f.events) != 1 || f.events[0] != "stop" {
		t.Errorf("KVM teardown %q, want STOP_SESSION", f.events)
	}
	if f.web.logouts != 1 {
		t.Errorf("web logouts %d, want 1", f.web.logouts)
	}
	if err := c.SendKeyEvent(true, 4); err == nil {
		t.Error("want an error for input after logout")
	}
}

func TestClientMapsCDThroughATunnel(t *testing.T) {
	f := newFakeBMCServer(t)
	ep := testEndpoint(t, f.ln.Addr().String(), f.srv.Certificate())
	c := NewClient(ep, "admin", "pw")
	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, ok := any(c).(bmc.VirtualMedia); !ok {
		t.Fatal("Client does not implement bmc.VirtualMedia")
	}
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	loggedOut := false
	defer func() {
		if !loggedOut {
			cancel()
			c.Logout()
		}
	}()
	c.Start(ctx, sink)
	waitFor(t, "a frame through the tunnel", func() bool { n, _, _, _ := sink.snapshot(); return n > 0 })

	iso := filepath.Join(t.TempDir(), "test.iso")
	content := bytes.Repeat([]byte("ipmi-connect"), 4*2048/12+1)[:4*2048]
	if err := os.WriteFile(iso, content, 0o600); err != nil {
		t.Fatal(err)
	}
	im, err := c.AddImage(iso, true)
	if err != nil {
		t.Fatal(err)
	}
	mapped := make(chan error, 1)
	go func() { mapped <- c.MapImage(im.ID, bmc.TargetCD) }()
	var m *fakeMediaBMC
	select {
	case m = <-f.media:
	case err := <-mapped:
		t.Fatalf("MapImage returned without a tunnel: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no media tunnel")
	}
	if m.service != serviceCD {
		t.Errorf("service %q", m.service)
	}
	auth := m.accept(1)
	if tok := string(bytes.TrimRight(auth[mediaAuthTokenAt:], "\x00")); tok != "KVMTOKEN0123456" {
		t.Errorf("media token %q", tok)
	}
	if err := <-mapped; err != nil {
		t.Fatalf("MapImage: %v", err)
	}
	r := m.scsi([]byte{0x28, 0, 0, 0, 0, 1, 0, 0, 2, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, content[2048:3*2048]) {
		t.Errorf("READ(10): status %d, %d bytes", r.status, len(r.data))
	}
	if st := c.MediaStatus(); len(st.Drives) != 1 || st.Drives[0].Target != bmc.TargetCD {
		t.Errorf("media status %+v", st.Drives)
	}
	if err := c.UnmapDrive(bmc.TargetCD); err != nil {
		t.Fatal(err)
	}
	// The tunnel stays up with an empty drive, and the next image goes over it.
	sense(t, "TEST UNIT READY after eject", m.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x02, 0x3A, 0)
	iso2 := filepath.Join(t.TempDir(), "second.iso")
	content2 := bytes.Repeat([]byte{0xC3}, 2*2048)
	if err := os.WriteFile(iso2, content2, 0o600); err != nil {
		t.Fatal(err)
	}
	im2, err := c.AddImage(iso2, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MapImage(im2.ID, bmc.TargetCD); err != nil {
		t.Fatalf("MapImage after eject: %v", err)
	}
	select {
	case <-f.media:
		t.Fatal("a second media tunnel was opened")
	default:
	}
	sense(t, "TEST UNIT READY after the swap", m.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x06, 0x28, 0)
	if r := m.scsi([]byte{0x28, 0, 0, 0, 0, 1, 0, 0, 1, 0}, nil); r.status != 0 || !bytes.Equal(r.data, content2[2048:]) {
		t.Errorf("READ of the second image: status %d, %d bytes", r.status, len(r.data))
	}
	// As JViewer leaves: media first, so MEDIA_STATE 0 goes out on the live KVM session, then
	// STOP_SESSION.
	c.CloseMedia()
	_ = m.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := m.r.ReadByte(); err == nil {
		t.Error("media tunnel still open after CloseMedia")
	}
	cancel()
	loggedOut = true
	c.Logout()
	if got := strings.Join(f.eventLog(), ", "); got != "media 1, media 0, stop" {
		t.Errorf("KVM messages %q, want media 1, media 0, stop", got)
	}
}

func TestClientReconnectKeepsMediaTunnel(t *testing.T) {
	fastReconnect(t, 50*time.Millisecond)
	f := newFakeBMCServer(t)
	_, m, content, stop := startMappedCD(t, f)

	f.dropKVM()
	waitFor(t, "a second KVM tunnel", func() bool { return f.kvmTunnels() == 2 })
	// The drive is still connected, so the new KVM session must hear so, as the old one did.
	waitFor(t, "MEDIA_STATE 1 on the new KVM session", func() bool { return f.kvmEvents(2) == "media 1" })
	if r := m.scsi([]byte{0x28, 0, 0, 0, 0, 2, 0, 0, 1, 0}, nil); r.status != 0 || !bytes.Equal(r.data, content[2*2048:3*2048]) {
		t.Errorf("READ after the reconnect: status %d, %d bytes", r.status, len(r.data))
	}
	select {
	case <-f.media:
		t.Error("the reconnect opened another media tunnel")
	default:
	}
	stop()
	if got := f.kvmEvents(2); got != "media 1, media 0, stop" {
		t.Errorf("new KVM session messages %q, want media 1, media 0, stop", got)
	}
}

func TestClientMediaNeedsKVMSession(t *testing.T) {
	c := &Client{}
	if _, err := c.openVMedia(context.Background()); err == nil {
		t.Error("want an error opening virtual media without a KVM session")
	}
}

func TestClientRefusesLaunchWithoutTLS(t *testing.T) {
	if err := checkLaunch(Launch{KVMToken: "t", WebCookie: "c", SinglePort: true}); err == nil {
		t.Error("want an error for a tunnel without TLS")
	}
	if err := checkLaunch(Launch{KVMToken: "t", WebCookie: "c", Secure: true}); err == nil {
		t.Error("want an error for a dedicated KVM port")
	}
	if err := checkLaunch(Launch{KVMToken: "t", WebCookie: "c", Secure: true, SinglePort: true}); err != nil {
		t.Errorf("single-port TLS launch: %v", err)
	}
}

func TestClientKeyboardLEDsWithoutSession(t *testing.T) {
	if got := (&Client{}).KeyboardLEDs(); got.Known {
		t.Errorf("no session: %+v", got)
	}
}
