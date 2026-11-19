package megarac

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
}

func newFakeBMCServer(t *testing.T) *fakeBMCServer {
	f := &fakeBMCServer{t: t, web: &fakeWeb{password: "pw", cookie: "COOKIE123", csrf: "CSRF456", power: 1}, closing: make(chan struct{})}
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
	defer c.Close()
	// Two request blocks, each ending with a blank line.
	for i := 0; i < 2; i++ {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
	}
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
