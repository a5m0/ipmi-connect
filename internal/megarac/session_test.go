package megarac

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

type recordingSink struct {
	mu       sync.Mutex
	frames   int
	w, h     int
	statuses []string
}

func (s *recordingSink) UpdateScreen(w, h int, _ []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames++
	s.w, s.h = w, h
}

func (s *recordingSink) SetStatus(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, text)
}

func (s *recordingSink) SetFPS(int) {}

func (s *recordingSink) snapshot() (int, int, int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames, s.w, s.h, append([]string(nil), s.statuses...)
}

// fakeBMC plays the BMC's side of the IVTP session on conn.
type fakeBMC struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func newFakeBMC(t *testing.T) (*fakeBMC, net.Conn) {
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	return &fakeBMC{t: t, conn: server, r: bufio.NewReader(server)}, client
}

func (f *fakeBMC) send(b ...[]byte) {
	f.t.Helper()
	for _, p := range b {
		if _, err := f.conn.Write(p); err != nil {
			f.t.Errorf("fake BMC write: %v", err)
		}
	}
}

func (f *fakeBMC) msg(typ uint16, status uint16, payload []byte) []byte {
	return append(header(typ, uint32(len(payload)), status), payload...)
}

// expect reads one client message and checks its type.
func (f *fakeBMC) expect(typ uint16) message {
	f.t.Helper()
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := readMessage(f.r)
	if err != nil {
		f.t.Fatalf("fake BMC waiting for type %d: %v", typ, err)
	}
	if m.Type != typ {
		f.t.Fatalf("got message type %d (size %d), want %d", m.Type, len(m.Payload), typ)
	}
	return m
}

// handshake plays SESSION_ACCEPTED through the viewer's post-validation requests.
func (f *fakeBMC) handshake(result byte) {
	f.t.Helper()
	f.send(f.msg(typeSessionAccepted, 0, nil))
	if m := f.expect(typeWebToken); string(m.Payload) != "COOKIE123" {
		f.t.Errorf("web token %q", m.Payload)
	}
	// The validation header declares 332 bytes; the BMC reads them, the last 8 being the
	// RESUME_REDIRECTION header JViewer sends straight after its 324.
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var raw [8 + 332]byte
	if _, err := io.ReadFull(f.r, raw[:]); err != nil {
		f.t.Fatalf("read validation: %v", err)
	}
	if binary.LittleEndian.Uint16(raw[0:]) != typeValidateSession {
		f.t.Fatalf("got type %d, want validation", binary.LittleEndian.Uint16(raw[0:]))
	}
	if tok := strings.TrimRight(string(raw[9:9+129]), "\x00"); tok != "KVMTOKEN0123456" {
		f.t.Errorf("KVM token %q", tok)
	}
	if !bytes.Equal(raw[8+324:], header(typeResumeRedirection, 0, 0)) {
		f.t.Errorf("validation not followed by RESUME_REDIRECTION: % x", raw[8+324:])
	}
	f.send(f.msg(typeValidateResponse, 0, []byte{result, 0}))
	if result != validationOK {
		return
	}
	f.send(f.msg(typeMouseMode, 0, []byte{2}))
	f.expect(typePowerStatus)
	if m := f.expect(typeDisplayLock); !bytes.Equal(m.Payload, []byte{2}) {
		f.t.Errorf("display lock payload % x", m.Payload)
	}
	f.expect(typeGetUserMacro)
}

func (f *fakeBMC) frame(fr recordedFrame) {
	data := amiFrame(fr.h, fr.bitstream, false)
	half := len(data) / 2
	f.send(f.msg(typeVideoFragment, 0, fragment(0, false, data[:half])),
		f.msg(typeVideoFragment, 0, fragment(1, true, data[half:])))
}

func startSession(t *testing.T, conn net.Conn, sink Sink) (*Session, <-chan error) {
	t.Helper()
	s := newSession(conn, bufio.NewReader(conn), credentials{kvmToken: "KVMTOKEN0123456", webCookie: "COOKIE123", user: "admin", clientIP: "192.0.2.50"})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), sink) }()
	return s, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionHandshakeVideoInputPower(t *testing.T) {
	bmcSide, conn := newFakeBMC(t)
	sink := &recordingSink{}
	s, done := startSession(t, conn, sink)
	bmcSide.handshake(validationOK)

	frames := readRecording(t)
	bmcSide.frame(frames[0])
	waitFor(t, "a decoded frame", func() bool { n, _, _, _ := sink.snapshot(); return n == 1 })
	if _, w, h, _ := sink.snapshot(); w != frames[0].h.Width || h != frames[0].h.Height {
		t.Errorf("screen %dx%d", w, h)
	}

	// Power state from the BMC.
	bmcSide.send(bmcSide.msg(typePowerStatus, 1, nil))
	waitFor(t, "power state", func() bool { return s.PowerState() == bmc.PowerStateOn })

	// Keyboard: press shift+a, release a.
	go func() {
		_ = s.SendKeyEvent(true, 0xE1)
		_ = s.SendKeyEvent(true, 0x04)
		_ = s.SendKeyEvent(false, 0x04)
	}()
	want := [][8]byte{{0x02}, {0x02, 0, 0x04}, {0x02}}
	for i, w := range want {
		m := bmcSide.expect(typeHID)
		var got [8]byte
		copy(got[:], m.Payload[33:41])
		if got != w {
			t.Errorf("key report %d = % x, want % x", i, got, w)
		}
	}

	// Mouse: the last pixel of the 1024-wide screen is the far edge; right button.
	go func() { _ = s.SendPointerEvent(4, 1023, 0) }()
	m := bmcSide.expect(typeHID)
	if m.Payload[33] != 2 || binary.LittleEndian.Uint16(m.Payload[34:]) != 32767 {
		t.Errorf("mouse report % x", m.Payload[33:39])
	}

	go func() { _ = s.RequestFrameRefresh() }()
	bmcSide.expect(typeFullScreen)

	go func() { _ = s.PowerControl(bmc.PowerSoftOff) }()
	if m := bmcSide.expect(typePowerControl); m.Status != 5 {
		t.Errorf("power code %d, want 5 (soft off)", m.Status)
	}
	if err := s.PowerControl("power_explode"); err == nil {
		t.Error("want an error for an unknown power action")
	}

	// The BMC ends the session.
	bmcSide.send(bmcSide.msg(typeStopSession, 7, nil))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "web session") {
			t.Errorf("Run = %v, want the web session logout reason", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after STOP_SESSION")
	}
}

func TestSessionRejectedValidation(t *testing.T) {
	bmcSide, conn := newFakeBMC(t)
	_, done := startSession(t, conn, &recordingSink{})
	bmcSide.handshake(0)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Errorf("Run = %v, want a rejected session", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Keyboard and mouse encryption is not implemented: a BMC that requires it must fail loudly
// rather than receive input it would drop.
func TestSessionRequiringEncryptionFails(t *testing.T) {
	bmcSide, conn := newFakeBMC(t)
	_, done := startSession(t, conn, &recordingSink{})
	bmcSide.handshake(validationOK)
	bmcSide.send(bmcSide.msg(typeInitialEncryption, 0, nil))
	select {
	case err := <-done:
		if !errors.Is(err, ErrEncryptionRequired) {
			t.Errorf("Run = %v, want ErrEncryptionRequired", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestSessionStopsOnContextAndSendsDisconnect(t *testing.T) {
	bmcSide, conn := newFakeBMC(t)
	s := newSession(conn, bufio.NewReader(conn), credentials{kvmToken: "KVMTOKEN0123456", webCookie: "COOKIE123", user: "admin"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, &recordingSink{}) }()
	bmcSide.handshake(validationOK)
	cancel()
	_ = bmcSide.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bmcSide.r.ReadString('\n')
	if err != nil || line != "JVIEWER DISCONNECT Cookie COOKIE123\r\n" {
		t.Errorf("disconnect line %q, %v", line, err)
	}
	go io.Copy(io.Discard, bmcSide.r)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err := s.SendKeyEvent(true, 4); err == nil {
		t.Error("want an error sending to a closed session")
	}
}

func TestSessionReportsBlankScreen(t *testing.T) {
	bmcSide, conn := newFakeBMC(t)
	sink := &recordingSink{}
	startSession(t, conn, sink)
	bmcSide.handshake(validationOK)
	bmcSide.send(bmcSide.msg(typeBlankScreen, 0, nil))
	waitFor(t, "no signal status", func() bool {
		_, _, _, st := sink.snapshot()
		return len(st) > 0 && st[len(st)-1] == statusNoSignal
	})
}
