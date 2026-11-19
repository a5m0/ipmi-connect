package megarac

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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
	disc    []string
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
	t.Cleanup(func() { close(f.closing); ln.Close(); f.srv.Close() })
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
	f.mu.Unlock()
	_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	b := &fakeBMC{t: f.t, conn: c, r: r}
	b.handshake(validationOK)
	frames := readRecording(f.t)
	b.frame(frames[0])
	// Wait for the client to leave and record its disconnect line.
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "JVIEWER DISCONNECT") {
			f.mu.Lock()
			f.disc = append(f.disc, strings.TrimSpace(line))
			f.mu.Unlock()
			return
		}
	}
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
	if len(f.disc) != 1 || f.disc[0] != "JVIEWER DISCONNECT Cookie COOKIE123" {
		t.Errorf("disconnects %q", f.disc)
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
	cancel()
	loggedOut = true
	c.Logout()
	_ = m.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := m.r.ReadByte(); err == nil {
		t.Error("media tunnel still open after Logout")
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
