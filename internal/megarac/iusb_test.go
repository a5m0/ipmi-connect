package megarac

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// iusbFrame builds a BMC-side IUSB frame: a header whose data length covers body.
func iusbFrame(seq uint32, body []byte) []byte {
	h := make([]byte, iusbHeaderLen)
	copy(h, iusbSignature)
	h[8], h[10] = 1, iusbHeaderLen
	binary.LittleEndian.PutUint32(h[12:], uint32(len(body)))
	h[17], h[18] = mediaDevice, mediaProtocol
	binary.LittleEndian.PutUint32(h[24:], seq)
	setIUSBChecksum(h)
	return append(h, body...)
}

// scsiRequest builds a SCSI request frame as the BMC sends it: CDB at 41, data from 61.
func scsiRequest(seq uint32, cdb []byte, data []byte) []byte {
	body := make([]byte, scsiDataAt-iusbHeaderLen, scsiDataAt-iusbHeaderLen+len(data))
	binary.LittleEndian.PutUint32(body[0:], 0x11223344) // bytes 32..40 are echoed unread
	body[8] = 0x5A
	copy(body[scsiCDBAt-iusbHeaderLen:], cdb)
	return iusbFrame(seq, append(body, data...))
}

func TestReadIUSBFrame(t *testing.T) {
	want := scsiRequest(7, []byte{0x00}, []byte("payload"))
	got, err := readIUSB(bytes.NewReader(append(want, 0xEE)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("frame\n% x\nwant\n% x", got, want)
	}
}

func TestReadIUSBRejectsBadFrames(t *testing.T) {
	good := scsiRequest(1, []byte{0x00}, nil)
	badSig := append([]byte(nil), good...)
	copy(badSig, "IVTP    ")
	huge := append([]byte(nil), good[:iusbHeaderLen]...)
	binary.LittleEndian.PutUint32(huge[12:], maxIUSBFrame)
	for name, b := range map[string][]byte{
		"signature": badSig, "oversize": huge,
		"truncated header": good[:10], "truncated body": good[:40],
	} {
		if _, err := readIUSB(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := readIUSB(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: %v, want EOF", err)
	}
}

// The ASMB8 leaves the version and header length bytes zero in what it sends (seen on hardware).
func TestReadIUSBAcceptsZeroVersionAndHeaderLength(t *testing.T) {
	frame := scsiRequest(1, []byte{0x00}, nil)
	frame[8], frame[9], frame[10] = 0, 0, 0
	if _, err := readIUSB(bytes.NewReader(frame)); err != nil {
		t.Errorf("readIUSB: %v", err)
	}
}

func TestIUSBChecksum(t *testing.T) {
	h := iusbFrame(3, nil)
	var sum byte
	for _, b := range h[:iusbHeaderLen] {
		sum += b
	}
	if sum != 0 {
		t.Errorf("header bytes sum to %#x, want 0", sum)
	}
}

func TestMediaAuthPacket(t *testing.T) {
	p, err := mediaAuthPacket("TOKEN-abc", 0x80)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != mediaAuthLen {
		t.Fatalf("auth packet is %d bytes, want %d", len(p), mediaAuthLen)
	}
	if string(p[:8]) != "IUSB    " || p[8] != 1 || p[10] != 32 {
		t.Errorf("header start % x", p[:11])
	}
	if n := binary.LittleEndian.Uint32(p[12:]); n != mediaAuthLen-iusbHeaderLen {
		t.Errorf("data length %d", n)
	}
	if p[17] != 5 || p[18] != 1 || p[19] != 0x80 || p[22] != 0x80 || p[23] != 0 {
		t.Errorf("device/protocol/direction/client data/instance = % x", p[17:24])
	}
	if p[41] != opMediaAuth || p[62] != 0 {
		t.Errorf("opcode %#x, token type %d", p[41], p[62])
	}
	if got := string(bytes.TrimRight(p[63:], "\x00")); got != "TOKEN-abc" {
		t.Errorf("token %q", got)
	}
	var sum byte
	for _, b := range p[:iusbHeaderLen] {
		sum += b
	}
	if sum != 0 {
		t.Errorf("checksum: header sums to %#x", sum)
	}
	if _, err := mediaAuthPacket(string(bytes.Repeat([]byte{'a'}, mediaAuthLen)), 0); err == nil {
		t.Error("want an error for a token that does not fit")
	}
}

func TestSCSIReplyEchoesRequest(t *testing.T) {
	req := scsiRequest(0xA1B2C3D4, []byte{0x28, 0, 0, 0, 0, 2, 0, 0, 1}, nil)
	data := []byte("block")
	rep := scsiReply(req, 1, 0x05, 0x24, 0x01, data)
	if len(rep) != scsiDataAt+len(data) {
		t.Fatalf("reply is %d bytes", len(rep))
	}
	if !bytes.Equal(rep[16:19], req[16:19]) || !bytes.Equal(rep[20:53], req[20:53]) {
		t.Error("reply does not echo the request's header fields, bytes 32..40 and CDB")
	}
	if rep[19] != 0x80 {
		t.Errorf("direction %#x", rep[19])
	}
	if n := binary.LittleEndian.Uint32(rep[12:]); n != uint32(len(rep)-iusbHeaderLen) {
		t.Errorf("header data length %d", n)
	}
	if !bytes.Equal(rep[53:57], []byte{1, 0x05, 0x24, 0x01}) {
		t.Errorf("status/sense % x", rep[53:57])
	}
	if n := binary.LittleEndian.Uint32(rep[57:]); n != uint32(len(data)) {
		t.Errorf("data length field %d", n)
	}
	if !bytes.Equal(rep[61:], data) {
		t.Errorf("data %q", rep[61:])
	}
	var sum byte
	for _, b := range rep[:iusbHeaderLen] {
		sum += b
	}
	if sum != 0 {
		t.Errorf("checksum: header sums to %#x", sum)
	}
	// A short request (no data byte) still gives a full-size reply prefix.
	if rep := scsiReply(req[:scsiStatusAt], 0, 0, 0, 0, nil); len(rep) != scsiDataAt {
		t.Errorf("reply to a short request is %d bytes", len(rep))
	}
}

func FuzzReadIUSB(f *testing.F) {
	f.Add(scsiRequest(1, []byte{0x28, 0, 0, 0, 0, 0, 0, 0, 1}, nil))
	f.Add(iusbFrame(0, []byte{1, 2, 3}))
	f.Add([]byte("IUSB    \x01\x00\x20\x00\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		frame, err := readIUSB(bytes.NewReader(b))
		if err != nil {
			return
		}
		if len(frame) < iusbHeaderLen || len(frame) > maxIUSBFrame {
			t.Fatalf("frame of %d bytes accepted", len(frame))
		}
		if int(binary.LittleEndian.Uint32(frame[12:]))+iusbHeaderLen != len(frame) {
			t.Fatal("frame length does not match its header")
		}
		_ = scsiReply(frame, 0, 0, 0, 0, nil)
	})
}
