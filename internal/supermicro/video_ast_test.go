package supermicro

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/aspeed"
)

// atenASTRects wraps the frames of the Dell's recorded ASPEED stream the way ATEN firmware sends
// them in 0x57 rects, and returns them with the headers the decoder should see.
func atenASTRects(t *testing.T) ([]Rect, []aspeed.Header, [][]byte) {
	t.Helper()
	data, err := os.ReadFile("../aspeed/testdata/c6220-pattern-1024x768.bin")
	if err != nil {
		t.Fatal(err)
	}
	var rects []Rect
	var headers []aspeed.Header
	var streams [][]byte
	for len(data) > 0 {
		n := int(binary.BigEndian.Uint32(data))
		msg := data[4 : 4+n]
		data = data[4+n:]
		h := aspeed.Header{
			Mode420: msg[1], LumaSelector: msg[2], ChromaSelector: msg[3],
			Width: int(binary.BigEndian.Uint16(msg[8:10])), Height: int(binary.BigEndian.Uint16(msg[10:12])),
		}
		h.AdvanceSelector = h.LumaSelector
		sub := uint16(astSubsampling444)
		if h.Mode420 == 1 {
			sub = astSubsampling420
		}
		payload := append([]byte{h.LumaSelector, h.ChromaSelector, byte(sub >> 8), byte(sub)}, msg[12:]...)
		rects = append(rects, Rect{W: uint16(h.Width), H: uint16(h.Height), Encoding: encodingAST, Data: payload})
		headers = append(headers, h)
		streams = append(streams, msg[12:])
	}
	return rects, headers, streams
}

func TestSessionDecodesASPEEDRects(t *testing.T) {
	rects, headers, streams := atenASTRects(t)
	s := newSession(nil, nil, ServerInfo{Video: true, KeyboardMouse: true})
	sink := &recordingSink{}
	ref := aspeed.NewDecoder()
	for i, r := range rects {
		s.handleUpdate(FramebufferUpdate{Rects: []Rect{r}}, sink)
		if err := ref.Decode(headers[i], streams[i]); err != nil {
			t.Fatal(err)
		}
		if sink.w != headers[i].Width || sink.h != headers[i].Height {
			t.Fatalf("frame %d: screen %dx%d", i, sink.w, sink.h)
		}
		if !bytes.Equal(sink.pix, ref.Frame()) {
			t.Fatalf("frame %d differs from the ASPEED decoder's own output", i)
		}
	}
	if sink.frames != len(rects) {
		t.Errorf("%d screen updates for %d rects", sink.frames, len(rects))
	}
}

func TestASPEEDRejectsUnknownSubsampling(t *testing.T) {
	err := (&astVideo{}).decode([]byte{4, 7, 0x12, 0x34, 0, 0, 0, 0, 0, 0, 0, 0}, newFrame(64, 64))
	if err == nil || !strings.Contains(err.Error(), "subsampling") {
		t.Fatalf("decode = %v, want an unknown subsampling error", err)
	}
}

func TestUnsupportedEncodingIsReported(t *testing.T) {
	s := newSession(nil, nil, ServerInfo{Video: true, KeyboardMouse: true})
	sink := &recordingSink{}
	s.handleUpdate(FramebufferUpdate{Rects: []Rect{{W: 800, H: 600, Encoding: 0x42, Data: []byte{1}}}}, sink)
	if len(sink.statuses) == 0 || !strings.Contains(sink.statuses[len(sink.statuses)-1], "0x42") {
		t.Errorf("statuses %q, want the unsupported encoding named", sink.statuses)
	}
}
