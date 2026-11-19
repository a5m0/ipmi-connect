package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a5m0/ipmi-connect/internal/trust"
)

// logBuf collects log lines.
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// iusbFrame builds an IUSB frame of total length n with op at the CDB position.
func iusbFrame(n int, op byte) []byte {
	f := make([]byte, n)
	copy(f, "IUSB    ")
	f[8], f[10] = 1, 32
	binary.LittleEndian.PutUint32(f[12:], uint32(n-32))
	f[17], f[18], f[19] = 5, 1, 0x80
	if n > 41 {
		f[41] = op
	}
	return f
}

// payload returns n bytes that do not repeat in short runs.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251 + 3)
	}
	return b
}

// leaksRun reports whether any 8-byte run of data shows up in log, raw or as hex.
func leaksRun(log string, data []byte) bool {
	compact := strings.NewReplacer(" ", "", ":", "").Replace(log)
	for i := 0; i+8 <= len(data); i++ {
		run := data[i : i+8]
		if strings.Contains(log, string(run)) || strings.Contains(compact, hex.EncodeToString(run)) {
			return true
		}
	}
	return false
}

func TestRedactBlanksCookies(t *testing.T) {
	for _, in := range []string{
		"CONNECT 127.0.0.1:8443 HTTPS/1.1\n cookie SECRETCOOKIE\r\n\r\n",
		"JVIEWER CDMEDIA cookie SECRETCOOKIE\r\n\r\n",
		"GET / HTTP/1.1\r\nCookie: SessionCookie=SECRETCOOKIE; lang=EN\r\n",
	} {
		got := redact(in)
		if strings.Contains(got, "SECRETCOOKIE") {
			t.Errorf("redact(%q) = %q", in, got)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("redact(%q) kept line breaks: %q", in, got)
		}
	}
}

func TestRelayTunnelRewritesTargetAndRedacts(t *testing.T) {
	req := "CONNECT 127.0.0.1:8443 HTTPS/1.1\n cookie SECRETCOOKIE\r\n\r\n" +
		"JVIEWER CDMEDIA cookie SECRETCOOKIE\r\n\r\n"
	reply := "HTTP/1.0 200 OK\r\n\r\n"
	var up, down bytes.Buffer
	var log logBuf
	service, err := relayTunnel(bufio.NewReader(strings.NewReader(req)), &down,
		bufio.NewReader(strings.NewReader(reply+"IUSB")), &up, "bmc.example.org:443", log.logf)
	if err != nil {
		t.Fatal(err)
	}
	if service != "CDMEDIA" {
		t.Errorf("service %q", service)
	}
	want := "CONNECT bmc.example.org:443 HTTPS/1.1\n cookie SECRETCOOKIE\r\n\r\nJVIEWER CDMEDIA cookie SECRETCOOKIE\r\n\r\n"
	if up.String() != want {
		t.Errorf("forwarded %q, want %q", up.String(), want)
	}
	if down.String() != reply {
		t.Errorf("relayed %q, want %q", down.String(), reply)
	}
	if l := log.String(); strings.Contains(l, "SECRETCOOKIE") || !strings.Contains(l, "CDMEDIA") || !strings.Contains(l, "200 OK") {
		t.Errorf("log:\n%s", l)
	}
}

func TestPumpIUSBLogsHeadersOnly(t *testing.T) {
	auth := iusbFrame(160, 0xF2)
	copy(auth[63:], "SECRETTOKEN0123456789")
	authReply := iusbFrame(64, 0xF1)
	authReply[62] = 1
	data := payload(4096)
	read := iusbFrame(61, 0x28)
	binary.LittleEndian.PutUint32(read[57:], uint32(len(data)))
	read = append(read, data...)
	binary.LittleEndian.PutUint32(read[12:], uint32(len(read)-32))
	private := iusbFrame(61, 0xF4)
	private = append(private, 0x80)
	binary.LittleEndian.PutUint32(private[12:], uint32(len(private)-32))
	in := bytes.Join([][]byte{auth, authReply, read, private}, nil)

	var out bytes.Buffer
	var log logBuf
	if err := pumpIUSB("C>B", bufio.NewReader(bytes.NewReader(in)), &out, log.logf); err != io.EOF {
		t.Errorf("pump: %v, want EOF", err)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Error("frames were not forwarded unchanged")
	}
	l := log.String()
	for _, want := range []string{"op=f2", "op=f1 authstat=1", "op=28", "dlen=4096", "op=f4", "tail=80"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "SECRETTOKEN") || strings.Contains(l, hex.EncodeToString([]byte("SECRETTO"))) {
		t.Errorf("log shows the token:\n%s", l)
	}
	if leaksRun(l, data) {
		t.Errorf("log shows image data:\n%s", l)
	}
}

func TestPumpIUSBFallsBackOnNonIUSB(t *testing.T) {
	data := payload(300)
	in := append(iusbFrame(64, 0x00), data...)
	var out bytes.Buffer
	var log logBuf
	_ = pumpIUSB("B>C", bufio.NewReader(bytes.NewReader(in)), &out, log.logf)
	if !bytes.Equal(out.Bytes(), in) {
		t.Errorf("forwarded %d bytes, want %d", out.Len(), len(in))
	}
	l := log.String()
	if !strings.Contains(l, "non-IUSB") || leaksRun(l, data) {
		t.Errorf("log:\n%s", l)
	}

	text := "JVIEWER DISCONNECT cookie SECRETCOOKIE\r\n\r\n"
	out.Reset()
	log = logBuf{}
	_ = pumpIUSB("C>B", bufio.NewReader(strings.NewReader(text)), &out, log.logf)
	if out.String() != text {
		t.Errorf("forwarded %q", out.String())
	}
	if l := log.String(); !strings.Contains(l, "DISCONNECT") || strings.Contains(l, "SECRETCOOKIE") {
		t.Errorf("log:\n%s", l)
	}
}

// ivtp builds an IVTP message.
func ivtp(typ uint16, status uint16, body []byte) []byte {
	h := make([]byte, 8)
	binary.LittleEndian.PutUint16(h, typ)
	binary.LittleEndian.PutUint32(h[2:], uint32(len(body)))
	binary.LittleEndian.PutUint16(h[6:], status)
	return append(h, body...)
}

func TestPumpIVTPLogsTypesAndCountsVideo(t *testing.T) {
	video := payload(1000)
	token := []byte("SECRETTOKEN0123456789")
	in := bytes.Join([][]byte{
		ivtp(0x13, 0, token),
		ivtp(25, 0, video), ivtp(25, 0, video), ivtp(25, 0, video),
		ivtp(0x22, 1, nil),
	}, nil)
	var out bytes.Buffer
	var log logBuf
	if err := pumpIVTP("B>C", bufio.NewReader(bytes.NewReader(in)), &out, log.logf); err != io.EOF {
		t.Errorf("pump: %v, want EOF", err)
	}
	if !bytes.Equal(out.Bytes(), in) {
		t.Error("messages were not forwarded unchanged")
	}
	l := log.String()
	for _, want := range []string{"type=19 size=21", "type=34 size=0 status=1", "video fragments x3 (3000 bytes)"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "type=25") || strings.Contains(l, "SECRETTOKEN") || leaksRun(l, video) {
		t.Errorf("log:\n%s", l)
	}
}

func TestPumpIVTPFallsBackOnDesync(t *testing.T) {
	in := append(ivtp(0x22, 0, nil), 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	in = append(in, payload(5000)...)
	var out bytes.Buffer
	var log logBuf
	_ = pumpIVTP("B>C", bufio.NewReader(bytes.NewReader(in)), &out, log.logf)
	if !bytes.Equal(out.Bytes(), in) {
		t.Errorf("forwarded %d bytes, want %d", out.Len(), len(in))
	}
	if l := log.String(); !strings.Contains(l, "desync") || !strings.Contains(l, "raw 5000 bytes") {
		t.Errorf("log:\n%s", l)
	}
}

func TestCheckListenRefusesNonLoopback(t *testing.T) {
	for _, a := range []string{"127.0.0.1:8443", "[::1]:8443", "127.0.0.2:1"} {
		if err := checkListen(a); err != nil {
			t.Errorf("checkListen(%q): %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:8443", ":8443", "192.0.2.1:8443", "[::]:8443", "example.org:8443", "127.0.0.1"} {
		if err := checkListen(a); err == nil {
			t.Errorf("checkListen(%q) accepted", a)
		}
	}
}

func TestParseBMC(t *testing.T) {
	for in, want := range map[string]string{
		"bmc.example.org": "bmc.example.org:443", "192.0.2.5:8443": "192.0.2.5:8443",
		"[2001:db8::1]": "[2001:db8::1]:443", "2001:db8::1": "[2001:db8::1]:443",
	} {
		host, addr, err := parseBMC(in)
		if err != nil || addr != want || strings.Contains(host, ":443") {
			t.Errorf("parseBMC(%q) = %q, %q, %v; want %q", in, host, addr, err, want)
		}
	}
	if _, _, err := parseBMC(""); err == nil {
		t.Error("empty -bmc accepted")
	}
}

// fakeBMC accepts one TLS connection, reads the tunnel request, answers it, then echoes the
// client's IUSB auth frame back with a reply and closes.
type fakeBMC struct {
	ln      net.Listener
	cert    *x509.Certificate
	request chan string
	got     chan []byte
}

func newFakeBMC(t *testing.T, reply []byte) *fakeBMC {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: srv.TLS.Certificates})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeBMC{ln: ln, cert: srv.Certificate(), request: make(chan string, 1), got: make(chan []byte, 1)}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		var req strings.Builder
		for blocks := 0; blocks < 2; {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			req.WriteString(line)
			if line == "\r\n" {
				blocks++
			}
		}
		f.request <- req.String()
		_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\n\r\n")
		frame := make([]byte, 160)
		if _, err := io.ReadFull(r, frame); err != nil {
			return
		}
		f.got <- frame
		_, _ = c.Write(reply)
	}()
	return f
}

// startProxy runs the proxy for the BMC at addr, trusting fp.
func startProxy(t *testing.T, addr, fp string) (string, *tls.Config, *logBuf) {
	t.Helper()
	host, target, err := parseBMC(addr)
	if err != nil {
		t.Fatal(err)
	}
	dial, err := upstreamDialer(host, target, fp, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, leaf, err := listenerConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	log := &logBuf{}
	p := &proxy{target: target, dial: dial, logf: log.logf}
	go func() { _ = p.serve(ln) }()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, log
}

func dialProxy(t *testing.T, addr string, cfg *tls.Config) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func TestProxyEndToEnd(t *testing.T) {
	reply := iusbFrame(64, 0xF1)
	reply[62] = 1
	f := newFakeBMC(t, reply)
	addr, cfg, log := startProxy(t, f.ln.Addr().String(), trust.Fingerprint(f.cert))
	conn := dialProxy(t, addr, cfg)

	req := "CONNECT 127.0.0.1:8443 HTTPS/1.1\n cookie SECRETCOOKIE\r\n\r\nJVIEWER CDMEDIA cookie SECRETCOOKIE\r\n\r\n"
	auth := iusbFrame(160, 0xF2)
	copy(auth[63:], "SECRETTOKEN0123456789")
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil || status != "HTTP/1.0 200 OK\r\n" {
		t.Fatalf("tunnel reply %q, %v", status, err)
	}
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(auth); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Errorf("client got %d bytes, want the BMC's %d unchanged", len(got), len(reply))
	}
	if fwd := <-f.got; !bytes.Equal(fwd, auth) {
		t.Error("the BMC did not get the auth frame unchanged")
	}
	if rq := <-f.request; !strings.HasPrefix(rq, "CONNECT "+f.ln.Addr().String()+" HTTPS/1.1\n") {
		t.Errorf("BMC got request %q", rq)
	}
	waitLog(t, log, "connection done")
	l := log.String()
	for _, want := range []string{"service CDMEDIA", "op=f2", "op=f1 authstat=1", "BMC closed first"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "SECRET") {
		t.Errorf("log shows a secret:\n%s", l)
	}
}

func TestProxyRefusesWrongFingerprint(t *testing.T) {
	f := newFakeBMC(t, nil)
	wrong := trust.FormatFingerprint(bytes.Repeat([]byte{0xAB}, 32))
	addr, cfg, log := startProxy(t, f.ln.Addr().String(), wrong)
	conn := dialProxy(t, addr, cfg)
	if _, err := io.WriteString(conn, "CONNECT 127.0.0.1:8443 HTTPS/1.1\n cookie x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(conn); len(b) != 0 {
		t.Errorf("client got %q (%v), want the connection closed", b, err)
	}
	waitLog(t, log, "dial BMC")
	select {
	case rq := <-f.request:
		t.Errorf("the BMC got a request through an untrusted connection: %q", rq)
	default:
	}
}

func waitLog(t *testing.T, log *logBuf, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !strings.Contains(log.String(), want) {
		select {
		case <-ctx.Done():
			t.Fatalf("log lacks %q:\n%s", want, log.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
