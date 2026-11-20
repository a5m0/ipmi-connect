package supermicro

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/a5m0/ipmi-connect/internal/aspeed"
)

// Encodings in ATEN framebuffer updates.
const (
	encodingHermonOld = 0x00 // WPCM450 on older firmware
	encodingAST       = 0x57 // ASPEED AST2050-AST2400 video engine
	encodingHermon    = 0x59 // WPCM450
)

// astPrefix is the 4 bytes ATEN puts before an ASPEED bitstream: the luma and chroma
// quantisation table selectors and the chroma subsampling as a big-endian u16.
const astPrefix = 4

// ATEN's subsampling codes.
const (
	astSubsampling444 = 0x0444
	astSubsampling420 = 0x0422
)

// astVideo decodes ASPEED rects. The decoder keeps the picture between updates: each one only
// redraws the blocks that changed.
type astVideo struct {
	dec *aspeed.Decoder
}

// decode applies one 0x57 payload for a w x h screen to f.
func (v *astVideo) decode(data []byte, f *frame) error {
	if len(data) < astPrefix {
		return errors.New("aspeed: payload shorter than its header")
	}
	h := aspeed.Header{Width: f.w, Height: f.h, LumaSelector: data[0], ChromaSelector: data[1]}
	h.AdvanceSelector = h.LumaSelector
	switch sub := binary.BigEndian.Uint16(data[2:4]); sub {
	case astSubsampling444:
	case astSubsampling420:
		h.Mode420 = 1
	default:
		return fmt.Errorf("aspeed: unknown subsampling %#04x (header % x)", sub, data[:astPrefix])
	}
	if v.dec == nil {
		v.dec = aspeed.NewDecoder()
	}
	if err := v.dec.Decode(h, data[astPrefix:]); err != nil {
		return err
	}
	copy(f.pix, v.dec.Frame())
	return nil
}
