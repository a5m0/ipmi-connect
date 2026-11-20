package megarac

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestHeaderIsLittleEndian(t *testing.T) {
	got := header(typePowerControl, 0x01020304, 5)
	want := []byte{35, 0, 4, 3, 2, 1, 5, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("header = % x, want % x", got, want)
	}
}

func TestReadMessage(t *testing.T) {
	var b bytes.Buffer
	b.Write(header(typeValidateResponse, 2, 0))
	b.Write([]byte{1, 0})
	b.Write(header(typePowerStatus, 0, 1))
	r := bufio.NewReader(&b)

	m, err := readMessage(r)
	if err != nil || m.Type != typeValidateResponse || !bytes.Equal(m.Payload, []byte{1, 0}) {
		t.Fatalf("first message %+v, %v", m, err)
	}
	m, err = readMessage(r)
	if err != nil || m.Type != typePowerStatus || m.Status != 1 || len(m.Payload) != 0 {
		t.Fatalf("second message %+v, %v", m, err)
	}
}

func TestReadMessageRejectsImplausibleSize(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader(header(typeVideoFragment, maxPayload+1, 0)))
	if _, err := readMessage(r); err == nil {
		t.Fatal("want an error for an implausible payload size")
	}
}

// The validation message has JViewer's layout: a header declaring 332 bytes, a 324-byte
// payload, and straight after it a RESUME_REDIRECTION header (the BMC needs both).
func TestValidationMessage(t *testing.T) {
	msg := validationMessage("ABCDEFGHIJKLMNOP", "192.0.2.50", "admin")
	if len(msg) != 8+324+8 {
		t.Fatalf("length %d, want %d", len(msg), 8+324+8)
	}
	if !bytes.Equal(msg[:8], header(typeValidateSession, 332, 0)) {
		t.Errorf("header % x", msg[:8])
	}
	p := msg[8 : 8+324]
	field := func(off, n int) string { return strings.TrimRight(string(p[off:off+n]), "\x00") }
	if p[0] != 0 {
		t.Errorf("token type %d, want 0 (web session token)", p[0])
	}
	if got := field(1, 129); got != "ABCDEFGHIJKLMNOP" {
		t.Errorf("token %q", got)
	}
	if got := field(130, 65); got != "192.0.2.50" {
		t.Errorf("client IP %q", got)
	}
	if got := field(195, 129); got != "admin" {
		t.Errorf("user %q", got)
	}
	if !bytes.Equal(msg[8+324:], header(typeResumeRedirection, 0, 0)) {
		t.Errorf("trailer % x, want RESUME_REDIRECTION", msg[8+324:])
	}
}

func TestValidationMessageTruncatesLongFields(t *testing.T) {
	long := strings.Repeat("x", 500)
	msg := validationMessage(long, long, long)
	if len(msg) != 8+324+8 {
		t.Fatalf("length %d", len(msg))
	}
	p := msg[8 : 8+324]
	// Each field keeps a terminating zero.
	for _, end := range []int{129, 194, 323} {
		if p[end] != 0 {
			t.Errorf("byte %d = %#x, want a terminating zero", end, p[end])
		}
	}
}

func checkIUSB(t *testing.T, pkt []byte, wantSize, wantDev, wantProto, wantLen byte) {
	t.Helper()
	if int(binary.LittleEndian.Uint32(pkt[2:6])) != int(wantSize) || binary.LittleEndian.Uint16(pkt[0:2]) != typeHID {
		t.Errorf("IVTP header % x", pkt[:8])
	}
	if len(pkt) != 8+int(wantSize) {
		t.Errorf("length %d, want %d", len(pkt), 8+int(wantSize))
	}
	if string(pkt[8:16]) != "IUSB    " {
		t.Errorf("signature %q", pkt[8:16])
	}
	var sum byte
	for _, b := range pkt[8:40] {
		sum += b
	}
	if sum != 0 {
		t.Errorf("IUSB header sums to %#x, want 0", sum)
	}
	if pkt[25] != wantDev || pkt[26] != wantProto || pkt[40] != wantLen {
		t.Errorf("device %#x protocol %#x length %d", pkt[25], pkt[26], pkt[40])
	}
}

func TestKeyboardPacket(t *testing.T) {
	var h hidState
	report := [8]byte{0x02, 0, 0x04}
	a := h.keyboard(report)
	b := h.keyboard(report)
	checkIUSB(t, a, 41, 0x30, 0x10, 8)
	if !bytes.Equal(a[41:49], report[:]) {
		t.Errorf("report % x", a[41:49])
	}
	if sa, sb := binary.LittleEndian.Uint32(a[32:36]), binary.LittleEndian.Uint32(b[32:36]); sb != sa+1 {
		t.Errorf("sequence %d then %d, want consecutive", sa, sb)
	}
}

func TestMousePacket(t *testing.T) {
	var h hidState
	pkt := h.mouse(0x02, 1000, 32767)
	checkIUSB(t, pkt, 39, 0x31, 0x20, 6)
	if pkt[41] != 0x02 || binary.LittleEndian.Uint16(pkt[42:]) != 1000 || binary.LittleEndian.Uint16(pkt[44:]) != 32767 || pkt[46] != 0 {
		t.Errorf("report % x", pkt[41:47])
	}
}

func TestKeyboardReportTracksHeldKeys(t *testing.T) {
	var k keyboardReport
	k.set(true, 0xE1)  // left shift
	k.set(true, 0x04)  // a
	k.set(true, 0x05)  // b
	k.set(false, 0x04) // release a
	got := k.bytes()
	want := [8]byte{0x02, 0, 0x05}
	if got != want {
		t.Fatalf("report % x, want % x", got, want)
	}
	k.set(false, 0xE1)
	k.set(false, 0x05)
	if k.bytes() != ([8]byte{}) {
		t.Fatalf("report % x after releasing everything", k.bytes())
	}
}

func TestKeyboardReportKeepsSixKeys(t *testing.T) {
	var k keyboardReport
	for u := uint16(0x04); u < 0x04+8; u++ {
		k.set(true, u)
	}
	got := k.bytes()
	if got[2] != 0x04 || got[7] != 0x09 {
		t.Fatalf("report % x, want the first six keys", got)
	}
	k.set(true, 0x04) // pressing a held key again changes nothing
	if k.bytes() != got {
		t.Fatalf("repeat press changed the report to % x", k.bytes())
	}
}

func TestHIDButtons(t *testing.T) {
	// Browser order: left 1, middle 2, right 4. HID order: left 1, right 2, middle 4.
	cases := map[byte]byte{0: 0, 1: 1, 2: 4, 4: 2, 7: 7}
	for in, want := range cases {
		if got := hidButtons(in); got != want {
			t.Errorf("hidButtons(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestScaleAbsolute(t *testing.T) {
	cases := []struct {
		v, size int
		want    uint16
	}{
		{0, 1024, 0}, {1023, 1024, 32767}, {512, 1024, 16400}, {2000, 1024, 32767}, {5, 0, 0},
	}
	for _, c := range cases {
		if got := scaleAbsolute(c.v, c.size); got != c.want {
			t.Errorf("scaleAbsolute(%d, %d) = %d, want %d", c.v, c.size, got, c.want)
		}
	}
}
