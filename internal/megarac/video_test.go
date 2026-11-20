package megarac

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/aspeed"
)

// recordedFrame is one frame of the Dell's recorded ASPEED test pattern (the same video engine)
// with its header, so tests use no picture taken from a real host.
type recordedFrame struct {
	h         aspeed.Header
	bitstream []byte
}

func readRecording(t *testing.T) []recordedFrame {
	t.Helper()
	data, err := os.ReadFile("../aspeed/testdata/c6220-pattern-1024x768.bin")
	if err != nil {
		t.Fatal(err)
	}
	var out []recordedFrame
	for len(data) > 0 {
		n := int(binary.BigEndian.Uint32(data))
		msg := data[4 : 4+n]
		data = data[4+n:]
		out = append(out, recordedFrame{
			h: aspeed.Header{
				Mode420: msg[1], LumaSelector: msg[2], ChromaSelector: msg[2], AdvanceSelector: msg[2],
				Width: int(binary.BigEndian.Uint16(msg[8:10])), Height: int(binary.BigEndian.Uint16(msg[10:12])),
			},
			bitstream: msg[12:],
		})
	}
	return out
}

// amiFrame wraps a bitstream in the BMC's 86-byte video header.
func amiFrame(h aspeed.Header, bitstream []byte, rc4 bool) []byte {
	f := make([]byte, frameHeaderLen, frameHeaderLen+len(bitstream))
	le := binary.LittleEndian
	le.PutUint16(f[0:], 1)
	le.PutUint16(f[2:], frameHeaderLen)
	le.PutUint16(f[4:], uint16(h.Width))
	le.PutUint16(f[6:], uint16(h.Height))
	le.PutUint16(f[13:], uint16(h.Width))
	le.PutUint16(f[15:], uint16(h.Height))
	f[42] = 3
	f[43] = 16
	f[44] = h.LumaSelector
	f[47] = h.AdvanceSelector
	if rc4 {
		f[53] = 1
	}
	f[55] = h.Mode420
	le.PutUint32(f[69:], uint32(len(bitstream)))
	return append(f, bitstream...)
}

func TestParseFrame(t *testing.T) {
	h := aspeed.Header{Width: 1024, Height: 768, Mode420: 0, LumaSelector: 4, ChromaSelector: 4, AdvanceSelector: 7}
	got, bits, err := parseFrame(amiFrame(h, []byte{1, 2, 3, 4, 5, 6, 7, 8}, false))
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Errorf("header %+v, want %+v", got, h)
	}
	if !bytes.Equal(bits, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("bitstream % x", bits)
	}
}

func TestParseFrameRejects(t *testing.T) {
	h := aspeed.Header{Width: 1024, Height: 768}
	good := amiFrame(h, make([]byte, 16), false)
	short := good[:40]
	rc4 := amiFrame(h, make([]byte, 16), true)
	overlong := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(overlong[69:], 1<<20) // claims more data than it carries
	badLen := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(badLen[2:], 40)
	for name, f := range map[string][]byte{"short": short, "rc4": rc4, "overlong": overlong, "header length": badLen} {
		if _, _, err := parseFrame(f); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func fragment(index int, last bool, data []byte) []byte {
	n := uint16(index)
	if last {
		n |= 0x8000
	}
	return append(binary.LittleEndian.AppendUint16(nil, n), data...)
}

func TestAssemblerJoinsFragments(t *testing.T) {
	var a assembler
	if f, err := a.add(fragment(0, false, []byte("abc"))); f != nil || err != nil {
		t.Fatalf("first fragment returned %q, %v", f, err)
	}
	f, err := a.add(fragment(1, true, []byte("def")))
	if err != nil || string(f) != "abcdef" {
		t.Fatalf("frame %q, %v", f, err)
	}
	// Index 0 starts afresh, dropping an unfinished frame.
	if _, err := a.add(fragment(0, false, []byte("xx"))); err != nil {
		t.Fatal(err)
	}
	if f, _ := a.add(fragment(0, true, []byte("yz"))); string(f) != "yz" {
		t.Fatalf("restarted frame %q", f)
	}
}

func TestAssemblerDropsFramesWithoutAStart(t *testing.T) {
	var a assembler
	if f, _ := a.add(fragment(3, true, []byte("orphan"))); f != nil {
		t.Fatalf("frame %q from a fragment with no start", f)
	}
	if _, err := a.add([]byte{1}); err == nil {
		t.Fatal("want an error for a fragment shorter than its number")
	}
}

func TestAssemblerBoundsFrameSize(t *testing.T) {
	var a assembler
	chunk := make([]byte, 1<<20)
	if _, err := a.add(fragment(0, false, chunk)); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 1; i < 64 && err == nil; i++ {
		_, err = a.add(fragment(i, false, chunk))
	}
	if err == nil {
		t.Fatal("want an error once a frame grows past the limit")
	}
}

// Frames from the recording, wrapped as this BMC sends them, decode exactly like the ASPEED
// decoder's own output.
func TestVideoDecodesRecordedFrames(t *testing.T) {
	frames := readRecording(t)
	var v video
	ref := aspeed.NewDecoder()
	for i, fr := range frames {
		w, h, pix, err := v.decode(amiFrame(fr.h, fr.bitstream, false))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if err := ref.Decode(fr.h, fr.bitstream); err != nil {
			t.Fatal(err)
		}
		if w != fr.h.Width || h != fr.h.Height || !bytes.Equal(pix, ref.Frame()) {
			t.Fatalf("frame %d differs from the reference decoder", i)
		}
	}
}
