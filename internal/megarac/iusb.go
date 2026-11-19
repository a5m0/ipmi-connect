package megarac

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// IUSB framing of the virtual media tunnels: a 32-byte header whose u32 at 12 is the length of
// what follows, then (for SCSI) the CDB at 41, status and sense at 53..56, a u32 data length at
// 57 and the data from 61. Multi-byte header fields are little-endian; the CDB is ordinary SCSI.
const (
	iusbSignature = "IUSB    "
	mediaDevice   = 5 // what JViewer sends for every media drive type
	mediaProtocol = 1
	directionIn   = 0x80 // client to BMC

	scsiCDBAt     = 41
	scsiCDBLen    = 12
	scsiStatusAt  = 53 // status, then sense key, ASC and ASCQ
	scsiDataLenAt = 57
	scsiDataAt    = 61

	// maxMediaData is the most data one reply carries: what JViewer's buffers hold, and above
	// what the BMC asks for (64 CD sectors, 256 disk sectors).
	maxMediaData = 128 << 10
	maxIUSBFrame = scsiDataAt + maxMediaData

	opMediaAuth      = 0xF2 // client: session token
	opMediaAuthReply = 0xF1 // BMC: connection status at 62
	opMediaSetHDType = 0xF4 // BMC, hard disk only: the client answers with its drive type
	opMediaKill      = 0xF6 // BMC: another client took the drive, or the BMC stopped it

	mediaAuthLen     = 160 // session token type 0
	mediaAuthTokenAt = 63
	mediaAuthStatAt  = 62
	mediaImageType   = 0x80 // hard disk drive type for an image file
)

// setIUSBChecksum sets byte 11 so the header's bytes sum to zero.
func setIUSBChecksum(h []byte) {
	h[iusbChecksumAt] = 0
	var sum byte
	for _, b := range h[:iusbHeaderLen] {
		sum += b
	}
	h[iusbChecksumAt] = -sum
}

// readIUSB reads one IUSB frame, header included.
func readIUSB(r io.Reader) ([]byte, error) {
	h := make([]byte, iusbHeaderLen)
	if _, err := io.ReadFull(r, h); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("IUSB frame header cut short")
		}
		return nil, err
	}
	if string(h[:8]) != iusbSignature || h[10] != iusbHeaderLen {
		return nil, fmt.Errorf("not an IUSB frame (header % x)", h[:11])
	}
	n := binary.LittleEndian.Uint32(h[12:])
	if n > maxIUSBFrame-iusbHeaderLen {
		return nil, fmt.Errorf("IUSB frame of %d bytes is too large", n)
	}
	frame := make([]byte, iusbHeaderLen+int(n))
	copy(frame, h)
	if _, err := io.ReadFull(r, frame[iusbHeaderLen:]); err != nil {
		return nil, fmt.Errorf("IUSB frame cut short: %w", err)
	}
	return frame, nil
}

// mediaAuthPacket builds the 0xF2 packet that opens a media tunnel with the KVM session token.
// clientData is 0 for CD and floppy, the drive type for a hard disk.
func mediaAuthPacket(token string, clientData byte) ([]byte, error) {
	if len(token) > mediaAuthLen-mediaAuthTokenAt-1 {
		return nil, errors.New("the KVM session token is too long for the media tunnel")
	}
	p := make([]byte, mediaAuthLen)
	copy(p, iusbSignature)
	p[8], p[10] = 1, iusbHeaderLen
	binary.LittleEndian.PutUint32(p[12:], mediaAuthLen-iusbHeaderLen)
	p[17], p[18], p[19] = mediaDevice, mediaProtocol, directionIn
	p[22] = clientData
	p[scsiCDBAt] = opMediaAuth
	copy(p[mediaAuthTokenAt:], token)
	setIUSBChecksum(p)
	return p, nil
}

// scsiReply answers req as JViewer does: its first 61 bytes echoed (header, sequence number and
// CDB included), then status, sense and data.
func scsiReply(req []byte, status, key, asc, ascq byte, data []byte) []byte {
	rep := make([]byte, scsiDataAt+len(data))
	copy(rep[:scsiDataAt], req)
	rep[19] = directionIn
	binary.LittleEndian.PutUint32(rep[12:], uint32(len(rep)-iusbHeaderLen))
	rep[scsiStatusAt], rep[scsiStatusAt+1], rep[scsiStatusAt+2], rep[scsiStatusAt+3] = status, key, asc, ascq
	binary.LittleEndian.PutUint32(rep[scsiDataLenAt:], uint32(len(data)))
	copy(rep[scsiDataAt:], data)
	setIUSBChecksum(rep)
	return rep
}
