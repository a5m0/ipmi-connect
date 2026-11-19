package megarac

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/a5m0/ipmi-connect/internal/aspeed"
)

// frameHeaderLen is the BMC's video header before each compressed frame.
const frameHeaderLen = 86

// Offsets in the video header (little-endian).
const (
	offHeaderLen     = 2
	offDestWidth     = 13
	offDestHeight    = 15
	offTableSelector = 44
	offTableMapping  = 45 // 1: chroma uses the luma tables (not supported by the decoder)
	offAdvanceTable  = 47
	offRC4           = 53
	offMode420       = 55
	offCompressSize  = 69
)

// maxFrame bounds a reassembled frame; an uncompressed 1920x1200 picture is under 9 MB.
const maxFrame = 16 << 20

var errRC4Video = errors.New("the BMC encrypts its video with RC4, which is not supported")

// parseFrame splits a frame into the decoder's header and the compressed bitstream.
func parseFrame(f []byte) (aspeed.Header, []byte, error) {
	if len(f) < frameHeaderLen {
		return aspeed.Header{}, nil, fmt.Errorf("video frame of %d bytes is shorter than its header", len(f))
	}
	le := binary.LittleEndian
	if n := le.Uint16(f[offHeaderLen:]); n != frameHeaderLen {
		return aspeed.Header{}, nil, fmt.Errorf("video header length %d, want %d", n, frameHeaderLen)
	}
	if f[offRC4] != 0 {
		return aspeed.Header{}, nil, errRC4Video
	}
	if f[offTableMapping] != 0 {
		return aspeed.Header{}, nil, fmt.Errorf("video table mapping %d is not supported", f[offTableMapping])
	}
	size := int(le.Uint32(f[offCompressSize:]))
	if size > len(f)-frameHeaderLen {
		return aspeed.Header{}, nil, fmt.Errorf("video frame claims %d bytes but carries %d", size, len(f)-frameHeaderLen)
	}
	h := aspeed.Header{
		Width:           int(le.Uint16(f[offDestWidth:])),
		Height:          int(le.Uint16(f[offDestHeight:])),
		Mode420:         f[offMode420],
		LumaSelector:    f[offTableSelector],
		ChromaSelector:  f[offTableSelector],
		AdvanceSelector: f[offAdvanceTable],
	}
	return h, f[frameHeaderLen : frameHeaderLen+size], nil
}

// assembler joins VIDEO_FRAGMENT payloads: a u16 fragment number (index in bits 0-14, 0 starts a
// frame; bit 15 marks the last fragment), then data.
type assembler struct {
	buf     []byte
	started bool
}

// add appends one fragment and returns the frame when it is complete. The frame aliases the
// assembler's buffer until the next call.
func (a *assembler) add(payload []byte) ([]byte, error) {
	if len(payload) < 2 {
		return nil, errors.New("video fragment shorter than its number")
	}
	n := binary.LittleEndian.Uint16(payload)
	if n&0x7FFF == 0 {
		a.buf, a.started = a.buf[:0], true
	} else if !a.started {
		return nil, nil // joined mid-frame: wait for the next start
	}
	if len(a.buf)+len(payload)-2 > maxFrame {
		a.buf, a.started = a.buf[:0], false
		return nil, fmt.Errorf("video frame larger than %d bytes", maxFrame)
	}
	a.buf = append(a.buf, payload[2:]...)
	if n&0x8000 == 0 {
		return nil, nil
	}
	a.started = false
	return a.buf, nil
}

// video decodes frames. The decoder keeps the picture between frames: each one only updates the
// blocks that changed.
type video struct {
	dec *aspeed.Decoder
}

// decode returns the picture after applying frame f; pix aliases the decoder's buffer.
func (v *video) decode(f []byte) (w, h int, pix []byte, err error) {
	hdr, bits, err := parseFrame(f)
	if err != nil {
		return 0, 0, nil, err
	}
	if v.dec == nil {
		v.dec = aspeed.NewDecoder()
	}
	if err := v.dec.Decode(hdr, bits); err != nil {
		return 0, 0, nil, err
	}
	return hdr.Width, hdr.Height, v.dec.Frame(), nil
}
