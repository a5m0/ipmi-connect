package megarac

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/atenvm"
	"github.com/a5m0/ipmi-connect/internal/vmedia"
)

// mediaAuthTimeout bounds the tunnel's auth exchange.
const mediaAuthTimeout = 10 * time.Second

// mediaBusyHint explains the BMC's habit, seen on the ASMB8, of keeping a drive's slot for a few
// minutes after its tunnel closes.
const mediaBusyHint = "the BMC keeps a drive busy for a few minutes after it is ejected or its connection drops; try again shortly"

var errMediaUSBReset = errors.New("USB reset is not supported on MegaRAC; eject and map the image again")

// mediaDialer opens a TLS tunnel through the BMC's web port that carries service.
type mediaDialer func(ctx context.Context, service string) (net.Conn, error)

// mediaSession is MegaRAC virtual media: one IUSB tunnel per mapped drive (CD, floppy, hard
// disk), each authenticated with the KVM session token. The BMC sends SCSI requests and the
// tunnel answers them from the image.
type mediaSession struct {
	dial  mediaDialer
	token string
	state func(on bool) // tells the KVM session media redirection started or stopped; may be nil

	mu     sync.Mutex
	drives map[uint16]*mediaDrive
	next   uint16
	ended  bool
	err    error
	event  string
	done   chan struct{}
}

type mediaDrive struct {
	disk   uint16
	drive  *vmedia.Drive
	target atenvm.SCSITarget
	conn   net.Conn
	closed chan struct{} // closed when serve returns
}

func newMediaSession(dial mediaDialer, token string, state func(on bool)) *mediaSession {
	return &mediaSession{dial: dial, token: token, state: state, drives: map[uint16]*mediaDrive{}, done: make(chan struct{})}
}

// services maps drive targets to tunnel services.
var mediaServices = map[vmedia.Target]string{
	vmedia.TargetCD: serviceCD, vmedia.TargetFloppy: serviceFloppy, vmedia.TargetRemovable: serviceHardDisk,
}

// Map opens a tunnel for the image's drive type and serves it.
func (s *mediaSession) Map(ctx context.Context, image vmedia.Image, o vmedia.MapOptions) (uint16, error) {
	service, ok := mediaServices[o.Target]
	if !ok {
		return 0, fmt.Errorf("MegaRAC has no %s drive", o.Target)
	}
	drive, err := vmedia.NewDrive(image, o)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	switch {
	case s.ended:
		s.mu.Unlock()
		return 0, errors.New("the virtual media session has ended")
	case s.mappedLocked(o.Target):
		s.mu.Unlock()
		return 0, fmt.Errorf("the %s drive is already mapped", o.Target)
	}
	s.mu.Unlock()

	conn, err := s.dial(ctx, service)
	if err != nil {
		return 0, fmt.Errorf("MegaRAC %s tunnel: %w", o.Target, err)
	}
	var clientData byte
	if o.Target == vmedia.TargetRemovable {
		clientData = mediaImageType
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := s.authenticate(ctx, conn, r, clientData); err != nil {
		_ = conn.Close()
		return 0, fmt.Errorf("MegaRAC %s drive: %w", o.Target, err)
	}

	s.mu.Lock()
	if s.ended || s.mappedLocked(o.Target) {
		s.mu.Unlock()
		_ = conn.Close()
		return 0, fmt.Errorf("the %s drive could not be mapped: the session changed", o.Target)
	}
	s.next++
	d := &mediaDrive{disk: s.next, drive: drive, target: atenvm.NewTarget(drive), conn: conn, closed: make(chan struct{})}
	first := len(s.drives) == 0
	s.drives[d.disk] = d
	s.mu.Unlock()
	if first && s.state != nil {
		s.state(true)
	}
	log.Printf("[MegaRAC] %s drive attached (%d blocks of %d bytes)", o.Target, drive.Blocks, drive.BlockSize)
	go s.serve(d, r)
	return d.disk, nil
}

func (s *mediaSession) mappedLocked(t vmedia.Target) bool {
	for _, d := range s.drives {
		if d.drive.Target == t {
			return true
		}
	}
	return false
}

// authenticate sends the session token and checks the BMC's 0xF1 answer.
func (s *mediaSession) authenticate(ctx context.Context, conn net.Conn, r *bufio.Reader, clientData byte) error {
	deadline := time.Now().Add(mediaAuthTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	auth, err := mediaAuthPacket(s.token, clientData)
	if err != nil {
		return err
	}
	if _, err := conn.Write(auth); err != nil {
		return fmt.Errorf("sending the session token: %w", err)
	}
	rep, err := readIUSB(r)
	if err != nil {
		return fmt.Errorf("the BMC did not accept the drive (%w); %s", err, mediaBusyHint)
	}
	if len(rep) <= mediaAuthStatAt || rep[scsiCDBAt] != opMediaAuthReply {
		return fmt.Errorf("unexpected first packet from the BMC (%d bytes)", len(rep))
	}
	switch status := rep[mediaAuthStatAt]; status {
	case 1:
		return nil
	case 5:
		return errors.New("the user has no virtual media privilege")
	case 8:
		return errors.New("the BMC has no free session for this drive: " + mediaBusyHint)
	default:
		other := ""
		if len(rep) > mediaAuthStatAt+1 {
			end := min(len(rep), mediaAuthStatAt+1+24)
			other = strings.TrimRight(string(rep[mediaAuthStatAt+1:end]), "\x00 ")
		}
		if other != "" {
			return fmt.Errorf("the drive is in use by another client (%s)", other)
		}
		return fmt.Errorf("the BMC refused the drive (status %d)", status)
	}
}

// serve answers the BMC's requests on one drive's tunnel until it closes.
func (s *mediaSession) serve(d *mediaDrive, r *bufio.Reader) {
	defer close(d.closed)
	for {
		req, err := readIUSB(r)
		if err != nil {
			s.driveEnded(d, fmt.Errorf("MegaRAC %s tunnel closed: %w", d.drive.Target, err))
			return
		}
		if len(req) < scsiStatusAt {
			s.driveEnded(d, fmt.Errorf("MegaRAC %s tunnel: short request of %d bytes", d.drive.Target, len(req)))
			return
		}
		var rep []byte
		kill := false
		switch req[scsiCDBAt] {
		case opMediaKill:
			rep, kill = scsiReply(req, 1, 0x05, 0x20, 0, nil), true
		case opMediaSetHDType:
			// JViewer appends its drive type to an empty reply, leaving the data length 0.
			rep = scsiReply(req, 1, 0x05, 0x20, 0, nil)
			rep = append(rep, mediaImageType)
			binary.LittleEndian.PutUint32(rep[12:], uint32(len(rep)-iusbHeaderLen))
			setIUSBChecksum(rep)
		default:
			rep = d.execute(req)
		}
		_ = d.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := d.conn.Write(rep); err != nil {
			s.driveEnded(d, fmt.Errorf("MegaRAC %s tunnel write: %w", d.drive.Target, err))
			return
		}
		if kill {
			s.driveEnded(d, fmt.Errorf("the BMC stopped the %s drive (another client may have taken it)", d.drive.Target))
			return
		}
	}
}

// requestSense is REQUEST SENSE for the target's pending sense.
var requestSense = []byte{atenvm.CmdRequestSense, 0, 0, 0, 18, 0}

// execute runs one SCSI request against the drive.
func (d *mediaDrive) execute(req []byte) []byte {
	cdb := make([]byte, scsiCDBLen)
	copy(cdb, req[scsiCDBAt:min(len(req), scsiCDBAt+scsiCDBLen)])
	fail := func() []byte {
		sense, _ := d.target.Execute(requestSense, 18)
		if len(sense) < 14 {
			return scsiReply(req, 1, 0x04, 0x44, 0, nil) // hardware error: internal target failure
		}
		return scsiReply(req, 1, sense[2]&0x0F, sense[12], sense[13], nil)
	}
	switch cdb[0] {
	case atenvm.CmdWrite10, atenvm.CmdWrite12:
		var data []byte
		if len(req) > scsiDataAt {
			data = req[scsiDataAt:]
		}
		if d.target.WriteData(cdb, data) != atenvm.CSWStatusGood {
			return fail()
		}
		return scsiReply(req, 0, 0, 0, 0, nil)
	case atenvm.CmdRead10, atenvm.CmdRead12:
		blocks := uint64(binary.BigEndian.Uint16(cdb[7:]))
		if cdb[0] == atenvm.CmdRead12 {
			blocks = uint64(binary.BigEndian.Uint32(cdb[6:]))
		}
		if blocks*uint64(d.drive.BlockSize) > maxMediaData {
			return scsiReply(req, 1, 0x05, 0x24, 0, nil) // invalid field in CDB
		}
	}
	data, ok := d.target.Execute(cdb, maxMediaData)
	if !ok {
		return fail()
	}
	if cdb[0] == atenvm.CmdStartStopUnit && cdb[4]&0x03 == 0x02 {
		log.Printf("[MegaRAC] The host ejected the %s drive", d.drive.Target)
	}
	return scsiReply(req, 0, 0, 0, 0, data)
}

// driveEnded ends the whole session when a drive's tunnel fails on its own; an Unmap or Close
// that closed it has already removed it.
func (s *mediaSession) driveEnded(d *mediaDrive, err error) {
	s.mu.Lock()
	if s.drives[d.disk] != d {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.end(err, d)
}

// end closes every tunnel and marks the session done with err. self is the drive whose serve
// loop is calling, which cannot wait for itself.
func (s *mediaSession) end(err error, self *mediaDrive) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.err = true, err
	if err != nil {
		s.event = err.Error()
	}
	drives := s.drives
	s.drives = map[uint16]*mediaDrive{}
	s.mu.Unlock()
	for _, d := range drives {
		_ = d.conn.Close()
	}
	for _, d := range drives {
		if d != self {
			<-d.closed
		}
	}
	if len(drives) > 0 && s.state != nil {
		s.state(false)
	}
	close(s.done)
}

// Unmap closes one drive's tunnel, which detaches it from the host.
func (s *mediaSession) Unmap(disk uint16) error {
	s.mu.Lock()
	d := s.drives[disk]
	if d == nil {
		s.mu.Unlock()
		return fmt.Errorf("no drive %d is mapped", disk)
	}
	delete(s.drives, disk)
	last := len(s.drives) == 0
	s.mu.Unlock()
	_ = d.conn.Close()
	<-d.closed
	if last && s.state != nil {
		s.state(false)
	}
	log.Printf("[MegaRAC] %s drive detached", d.drive.Target)
	return nil
}

// Drives reports the mapped drives.
func (s *mediaSession) Drives() []vmedia.DriveStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]vmedia.DriveStatus, 0, len(s.drives))
	for disk, d := range s.drives {
		out = append(out, d.drive.Status(disk))
	}
	return out
}

// USBReset is not available: JViewer has no such command for MegaRAC.
func (s *mediaSession) USBReset() error { return errMediaUSBReset }

// Close detaches every drive.
func (s *mediaSession) Close() error {
	s.end(nil, nil)
	return nil
}

// Done is closed when the session ends.
func (s *mediaSession) Done() <-chan struct{} { return s.done }

// Err is why the session ended on its own, or nil.
func (s *mediaSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// LastEvent describes the last notable event.
func (s *mediaSession) LastEvent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.event
}
