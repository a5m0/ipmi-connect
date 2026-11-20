package megarac

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// IVTP message types (see docs/asus-asmb8-ikvm.md).
const (
	typeHID               = 1
	typeResumeRedirection = 6
	typeStopSession       = 8
	typeBlankScreen       = 9
	typeMouseMode         = 10
	typeFullScreen        = 11
	typeInitialEncryption = 15
	typeValidateSession   = 18
	typeValidateResponse  = 19
	typeKeyboardLEDs      = 20
	typeWebToken          = 21
	typeSessionAccepted   = 23
	typeMediaState        = 24
	typeVideoFragment     = 25
	typePowerStatus       = 34
	typePowerControl      = 35
	typePowerResponse     = 36
	typeGetUserMacro      = 40
	typeDisplayLock       = 51
)

const (
	headerLen = 8
	// maxPayload bounds a message; video fragments are about 30 KB.
	maxPayload = 1 << 20
	// validationOK is the VALIDATE_VIDEO_SESSION_RESPONSE result for an accepted session.
	validationOK = 1
)

// The validation payload's zero-padded fields, as JViewer lays them out.
const (
	validationTokenLen = 129
	validationIPLen    = 65
	validationUserLen  = 129
	validationPayload  = 1 + validationTokenLen + validationIPLen + validationUserLen // 324
	validationDeclared = 332                                                          // what the header claims
)

// message is one IVTP message.
type message struct {
	Type    uint16
	Status  uint16
	Payload []byte
}

// header encodes an IVTP header: u16 type, u32 payload size, u16 status, little-endian.
func header(typ uint16, size uint32, status uint16) []byte {
	b := make([]byte, headerLen)
	binary.LittleEndian.PutUint16(b[0:], typ)
	binary.LittleEndian.PutUint32(b[2:], size)
	binary.LittleEndian.PutUint16(b[6:], status)
	return b
}

// readMessage reads one IVTP message.
func readMessage(r *bufio.Reader) (message, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return message{}, err
	}
	size := binary.LittleEndian.Uint32(h[2:])
	if size > maxPayload {
		return message{}, fmt.Errorf("IVTP message of %d bytes is not plausible", size)
	}
	m := message{Type: binary.LittleEndian.Uint16(h[0:]), Status: binary.LittleEndian.Uint16(h[6:])}
	if size > 0 {
		m.Payload = make([]byte, size)
		if _, err := io.ReadFull(r, m.Payload); err != nil {
			return message{}, err
		}
	}
	return m, nil
}

// putField copies s into a zero-padded field, keeping at least one terminating zero.
func putField(dst []byte, s string) {
	copy(dst[:len(dst)-1], s)
}

// validationMessage is VALIDATE_VIDEO_SESSION followed by RESUME_REDIRECTION, byte for byte as
// JViewer sends them: the header declares 332 bytes but only 324 follow, and the BMC reads the
// RESUME_REDIRECTION header as the rest. The BMC keeps the session only with this exact sequence.
func validationMessage(kvmToken, clientIP, user string) []byte {
	p := make([]byte, validationPayload)
	// p[0] = 0: the token is a web session token.
	putField(p[1:1+validationTokenLen], kvmToken)
	putField(p[1+validationTokenLen:1+validationTokenLen+validationIPLen], clientIP)
	putField(p[1+validationTokenLen+validationIPLen:], user)
	msg := header(typeValidateSession, validationDeclared, 0)
	msg = append(msg, p...)
	return append(msg, header(typeResumeRedirection, 0, 0)...)
}

// AMI IUSB packets carry USB HID reports inside IVTP HID messages.
const (
	iusbHeaderLen    = 32
	iusbChecksumAt   = 11 // within the IUSB header
	keyboardDevice   = 0x30
	keyboardProtocol = 0x10
	mouseDevice      = 0x31
	mouseProtocol    = 0x20
	keyboardDataLen  = 8
	mouseDataLen     = 6
)

// hidState numbers HID packets; keyboard and mouse count separately, as in JViewer.
type hidState struct {
	keyboardSeq, mouseSeq uint32
}

// iusbPacket wraps a report in an IVTP HID message with an IUSB header.
func iusbPacket(device, protocol, mouseFlag byte, seq uint32, report []byte) []byte {
	size := iusbHeaderLen + 1 + len(report)
	pkt := header(typeHID, uint32(size), 0)
	h := make([]byte, iusbHeaderLen)
	copy(h[0:8], "IUSB    ")
	h[8] = 1
	h[10] = 32
	binary.LittleEndian.PutUint32(h[12:], uint32(len(report)+1))
	h[17] = device
	h[18] = protocol
	h[19] = 0x80
	h[20] = 2
	h[21] = mouseFlag
	binary.LittleEndian.PutUint32(h[24:], seq)
	var sum byte
	for _, b := range h {
		sum += b
	}
	h[iusbChecksumAt] = -sum
	pkt = append(pkt, h...)
	pkt = append(pkt, byte(len(report)))
	return append(pkt, report...)
}

// keyboard builds a keyboard packet for an 8-byte boot report.
func (s *hidState) keyboard(report [8]byte) []byte {
	pkt := iusbPacket(keyboardDevice, keyboardProtocol, 0, s.keyboardSeq, report[:])
	s.keyboardSeq++
	return pkt
}

// mouse builds an absolute mouse packet: HID buttons, x and y in 0..32767, no wheel.
func (s *hidState) mouse(buttons byte, x, y uint16) []byte {
	r := make([]byte, mouseDataLen)
	r[0] = buttons
	binary.LittleEndian.PutUint16(r[1:], x)
	binary.LittleEndian.PutUint16(r[3:], y)
	pkt := iusbPacket(mouseDevice, mouseProtocol, 1, s.mouseSeq, r)
	s.mouseSeq++
	return pkt
}

// keyboardReport tracks held keys as a USB boot keyboard report.
type keyboardReport struct {
	modifiers byte
	keys      []byte // at most six, in press order
}

// set records a key press or release (usage is a HID keyboard usage).
func (k *keyboardReport) set(down bool, usage uint16) {
	if usage >= 0xE0 && usage <= 0xE7 {
		bit := byte(1) << (usage - 0xE0)
		if down {
			k.modifiers |= bit
		} else {
			k.modifiers &^= bit
		}
		return
	}
	if usage == 0 || usage > 0xFF {
		return
	}
	u := byte(usage)
	for i, held := range k.keys {
		if held == u {
			if !down {
				k.keys = append(k.keys[:i], k.keys[i+1:]...)
			}
			return
		}
	}
	if down && len(k.keys) < 6 {
		k.keys = append(k.keys, u)
	}
}

func (k *keyboardReport) bytes() [8]byte {
	var r [8]byte
	r[0] = k.modifiers
	copy(r[2:], k.keys)
	return r
}

// hidButtons converts browser buttons (left 1, middle 2, right 4) to HID (left 1, right 2,
// middle 4).
func hidButtons(b byte) byte {
	out := b & 1
	if b&2 != 0 {
		out |= 4
	}
	if b&4 != 0 {
		out |= 2
	}
	return out
}

// scaleAbsolute maps pixel v of a size-pixel axis to the mouse's 0..32767 range, the last pixel
// to the far edge.
func scaleAbsolute(v, size int) uint16 {
	if size <= 1 || v <= 0 {
		return 0
	}
	if v >= size-1 {
		return 32767
	}
	return uint16((v*32767 + (size-1)/2) / (size - 1))
}
