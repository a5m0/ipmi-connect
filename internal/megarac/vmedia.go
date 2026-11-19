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
	"github.com/a5m0/ipmi-connect/internal/debuglog"
	"github.com/a5m0/ipmi-connect/internal/vmedia"
)

// mediaAuthTimeout bounds the tunnel's auth exchange.
const mediaAuthTimeout = 10 * time.Second

// mediaBusyHint explains the ASMB8's one media slot, which it keeps for about two minutes after
// the tunnel holding it closes.
const mediaBusyHint = "the BMC serves one virtual drive at a time and keeps it for about two minutes after a drive closes; try again in two minutes"

var errMediaUSBReset = errors.New("USB reset is not supported on MegaRAC; eject and map the image again")

// mediaDialer opens a TLS tunnel through the BMC's web port that carries service, and returns it
// with the KVM session token that authenticates it.
type mediaDialer func(ctx context.Context, service string) (conn net.Conn, token string, err error)

// services maps drive targets to tunnel services.
var mediaServices = map[vmedia.Target]string{
	vmedia.TargetCD: serviceCD, vmedia.TargetFloppy: serviceFloppy, vmedia.TargetRemovable: serviceHardDisk,
}

// mediaPool holds the client's IUSB tunnel: one at a time, since that is all the ASMB8 serves,
// for whichever drive type (CD, floppy, hard disk) was last attached.
// A tunnel is dialled when its drive type is first mapped and kept until the pool closes or the
// tunnel dies: the BMC keeps a drive type busy for minutes after its tunnel closes, so an eject
// only parks the tunnel, which then reports an empty drive, and the next image reuses it.
type mediaPool struct {
	dial  mediaDialer
	state func(on bool) // tells the KVM session media redirection started or stopped; may be nil

	now func() time.Time

	mu        sync.Mutex
	tunnels   map[vmedia.Target]*mediaTunnel
	dialing   map[vmedia.Target]bool
	stateOn   bool
	closed    bool
	lastClose time.Time // when a tunnel last closed or an auth failed
}

// mediaSettle is how long the ASMB8 keeps its one media slot after the tunnel holding it closes
// (seen: refused at up to a minute, accepted from 1m54s). An auth sent sooner goes unanswered
// and holds the slot again, so the pool waits this long before dialling.
const mediaSettle = 2 * time.Minute

// mediaTunnel is one drive type's tunnel and the image currently in it, if any.
type mediaTunnel struct {
	target vmedia.Target
	conn   net.Conn
	closed chan struct{} // closed when serve returns

	mu      sync.Mutex // held while a request runs, so an eject waits for it
	slot    *mediaSlot // nil while the drive is empty
	changed bool       // report the medium change to the next command
	retired bool       // closed on purpose to make room for another drive type
}

// mediaSlot is an image attached to a tunnel through a view.
type mediaSlot struct {
	view   *mediaSession
	disk   uint16
	drive  *vmedia.Drive
	target atenvm.SCSITarget
}

func newMediaPool(dial mediaDialer, state func(on bool)) *mediaPool {
	return &mediaPool{dial: dial, state: state, now: time.Now, tunnels: map[vmedia.Target]*mediaTunnel{}, dialing: map[vmedia.Target]bool{}}
}

// view returns a new virtual media session on the pool's tunnels.
func (p *mediaPool) view() *mediaSession {
	return &mediaSession{pool: p, slots: map[uint16]*mediaSlot{}, tunnels: map[uint16]*mediaTunnel{}, done: make(chan struct{})}
}

// attach puts slot into the tunnel for target, dialling one if there is none.
func (p *mediaPool) attach(ctx context.Context, target vmedia.Target, slot *mediaSlot) (*mediaTunnel, error) {
	service, ok := mediaServices[target]
	if !ok {
		return nil, fmt.Errorf("MegaRAC has no %s drive", target)
	}
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		return nil, errors.New("virtual media has been closed")
	case p.dialing[target]:
		p.mu.Unlock()
		return nil, fmt.Errorf("the %s drive is already being attached", target)
	}
	if t := p.tunnels[target]; t != nil {
		defer p.mu.Unlock()
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.slot != nil {
			return nil, fmt.Errorf("the %s drive is already mapped", target)
		}
		t.slot, t.changed = slot, true
		return t, nil
	}
	// The ASMB8 serves one media tunnel at a time: it leaves a second drive type's auth
	// unanswered, and the failed attempt keeps that type busy for minutes. So another type's
	// image must be ejected first, and its parked tunnel is closed to make room.
	if len(p.dialing) > 0 {
		p.mu.Unlock()
		return nil, errors.New("another drive is being attached; try again when it is done")
	}
	var parked []*mediaTunnel
	for other, t := range p.tunnels {
		t.mu.Lock()
		inUse := t.slot != nil
		t.mu.Unlock()
		if inUse {
			p.mu.Unlock()
			return nil, fmt.Errorf("the BMC serves one virtual drive at a time; eject the %s drive first", other)
		}
		parked = append(parked, t)
	}
	for _, t := range parked {
		delete(p.tunnels, t.target)
	}
	p.dialing[target] = true
	p.mu.Unlock()
	for _, t := range parked {
		t.mu.Lock()
		t.retired = true
		t.mu.Unlock()
		_ = t.conn.Close()
		<-t.closed
		log.Printf("[MegaRAC] Empty %s drive closed to make room for the %s drive", t.target, target)
	}

	p.mu.Lock()
	if wait := mediaSettle - p.now().Sub(p.lastClose); !p.lastClose.IsZero() && wait > 0 {
		delete(p.dialing, target)
		off := p.stateOffLocked()
		p.mu.Unlock()
		if off && p.state != nil {
			p.state(false)
		}
		return nil, fmt.Errorf("the BMC frees its virtual drive about two minutes after a drive closes; try again in %v", wait.Round(time.Second))
	}
	p.mu.Unlock()

	t, r, err := p.open(ctx, target, service)
	p.mu.Lock()
	delete(p.dialing, target)
	if err == nil && p.closed {
		err = errors.New("virtual media has been closed")
		_ = t.conn.Close()
	}
	if err != nil {
		if errors.As(err, new(mediaAuthError)) {
			p.lastClose = p.now()
		}
		off := p.stateOffLocked()
		p.mu.Unlock()
		if off && p.state != nil {
			p.state(false)
		}
		return nil, err
	}
	t.slot = slot
	p.tunnels[target] = t
	first := !p.stateOn
	p.stateOn = true
	p.mu.Unlock()
	if first && p.state != nil {
		p.state(true)
	}
	go p.serve(t, r)
	return t, nil
}

// mediaAuthError is an auth the BMC did not accept: that attempt holds its media slot.
type mediaAuthError struct{ err error }

func (e mediaAuthError) Error() string { return e.err.Error() }
func (e mediaAuthError) Unwrap() error { return e.err }

// active reports whether media redirection is on: a tunnel is open, with or without an image.
func (p *mediaPool) active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stateOn
}

// stateOffLocked notes that media redirection stopped if no tunnel is left, and reports
// whether to send that.
func (p *mediaPool) stateOffLocked() bool {
	if len(p.tunnels) > 0 || !p.stateOn {
		return false
	}
	p.stateOn = false
	return true
}

// open dials and authenticates a tunnel for target.
func (p *mediaPool) open(ctx context.Context, target vmedia.Target, service string) (*mediaTunnel, *bufio.Reader, error) {
	conn, token, err := p.dial(ctx, service)
	if err != nil {
		return nil, nil, fmt.Errorf("MegaRAC %s tunnel: %w", target, err)
	}
	var clientData byte
	if target == vmedia.TargetRemovable {
		clientData = mediaImageType
	}
	r := bufio.NewReaderSize(conn, 64<<10)
	if err := authenticate(ctx, conn, r, token, clientData); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("MegaRAC %s drive: %w", target, mediaAuthError{err})
	}
	log.Printf("[MegaRAC] %s tunnel opened", target)
	return &mediaTunnel{target: target, conn: conn, closed: make(chan struct{})}, r, nil
}

// eject empties t if slot is still in it. It returns once no request is using the image.
func (p *mediaPool) eject(t *mediaTunnel, slot *mediaSlot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.slot == slot {
		t.slot, t.changed = nil, false
	}
}

// died drops a tunnel that failed or was stopped and ends the view using it, if any.
func (p *mediaPool) died(t *mediaTunnel, err error) {
	p.mu.Lock()
	closing := p.closed
	p.lastClose = p.now()
	last := false
	if p.tunnels[t.target] == t {
		delete(p.tunnels, t.target)
		if len(p.tunnels) == 0 && p.stateOn {
			p.stateOn, last = false, true
		}
	}
	p.mu.Unlock()
	_ = t.conn.Close()
	t.mu.Lock()
	slot := t.slot
	t.slot = nil
	if t.retired {
		err = nil
	}
	t.mu.Unlock()
	if closing {
		err = nil
	}
	if slot != nil {
		slot.view.end(err)
	} else if err != nil {
		log.Printf("[MegaRAC] Empty %s drive dropped: %v", t.target, err)
	}
	if last && p.state != nil {
		p.state(false)
	}
}

// Close closes every tunnel, which detaches the drives from the host, and refuses new ones.
func (p *mediaPool) Close() {
	p.mu.Lock()
	p.closed = true
	tunnels := p.tunnels
	p.tunnels = map[vmedia.Target]*mediaTunnel{}
	wasOn := p.stateOn
	p.stateOn = false
	p.mu.Unlock()
	for _, t := range tunnels {
		_ = t.conn.Close()
	}
	for _, t := range tunnels {
		<-t.closed
	}
	if wasOn && p.state != nil {
		p.state(false)
	}
	if len(tunnels) > 0 {
		log.Printf("[MegaRAC] Virtual media tunnels closed")
	}
}

// authenticate sends the session token and checks the BMC's 0xF1 answer.
func authenticate(ctx context.Context, conn net.Conn, r *bufio.Reader, token string, clientData byte) error {
	deadline := time.Now().Add(mediaAuthTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	auth, err := mediaAuthPacket(token, clientData)
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

// serve answers the BMC's requests on one tunnel until it closes.
func (p *mediaPool) serve(t *mediaTunnel, r *bufio.Reader) {
	defer close(t.closed)
	for {
		req, err := readIUSB(r)
		if err != nil {
			p.died(t, fmt.Errorf("MegaRAC %s tunnel closed: %w", t.target, err))
			return
		}
		if len(req) < scsiStatusAt {
			p.died(t, fmt.Errorf("MegaRAC %s tunnel: short request of %d bytes", t.target, len(req)))
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
			if req[scsiCDBAt] >= opMediaPrivate {
				// Other private opcodes, such as the 0xF3 the BMC sends about a minute in:
				// JViewer answers them as an invalid command, whatever is in the drive.
				rep = scsiReply(req, 1, 0x05, 0x20, 0, nil)
				break
			}
			rep = t.execute(req)
		}
		if debuglog.On(debuglog.MegaRACMedia) {
			debuglog.Printf(debuglog.MegaRACMedia, "%s", describeMediaRequest(t.target, req, rep))
		}
		_ = t.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := t.conn.Write(rep); err != nil {
			p.died(t, fmt.Errorf("MegaRAC %s tunnel write: %w", t.target, err))
			return
		}
		if kill {
			p.died(t, fmt.Errorf("the BMC stopped the %s drive (another client may have taken it)", t.target))
			return
		}
	}
}

// maxTraceTail is the most request data the trace shows, and only for private opcodes: enough
// for the one-byte argument of 0xF3, too little to carry anything from a disk.
const maxTraceTail = 4

// describeMediaRequest is the megarac-media trace line for one request and its reply: the
// drive, the opcode and CDB, the reply's status and sense and its data length. It never shows
// SCSI data; private opcodes with 1 to 4 bytes of request data show them as the tail.
func describeMediaRequest(target vmedia.Target, req, rep []byte) string {
	cdb := req[scsiCDBAt:min(len(req), scsiCDBAt+scsiCDBLen)]
	var st [4]byte
	var n uint32
	if len(rep) >= scsiDataAt {
		copy(st[:], rep[scsiStatusAt:])
		n = binary.LittleEndian.Uint32(rep[scsiDataLenAt:])
	}
	line := fmt.Sprintf("%s op=%02x cdb=% x -> st=%02x/%02x/%02x/%02x len=%d", target, cdb[0], cdb, st[0], st[1], st[2], st[3], n)
	if cdb[0] >= opMediaPrivate && len(req) > scsiDataAt && len(req)-scsiDataAt <= maxTraceTail {
		line += fmt.Sprintf(" tail=% x", req[scsiDataAt:])
	}
	return line
}

// execute answers one SCSI request from the image in the tunnel: an empty drive has no medium,
// and the first command after a new image goes in reports the change.
func (t *mediaTunnel) execute(req []byte) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.slot == nil:
		return scsiReply(req, 1, 0x02, 0x3A, 0, nil) // not ready: medium not present
	case t.changed && req[scsiCDBAt] != atenvm.CmdInquiry:
		t.changed = false
		return scsiReply(req, 1, 0x06, 0x28, 0, nil) // unit attention: medium may have changed
	}
	return t.slot.execute(req)
}

// requestSense is REQUEST SENSE for the target's pending sense.
var requestSense = []byte{atenvm.CmdRequestSense, 0, 0, 0, 18, 0}

// execute runs one SCSI request against the drive.
func (d *mediaSlot) execute(req []byte) []byte {
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

// mediaSession is the virtual media session the Manager sees: the images it attached to the
// pool's tunnels. Unmapping or closing it parks the tunnels rather than closing them; it ends on
// its own when a tunnel holding one of its images dies.
type mediaSession struct {
	pool *mediaPool

	mu      sync.Mutex
	slots   map[uint16]*mediaSlot
	tunnels map[uint16]*mediaTunnel
	next    uint16
	ended   bool
	err     error
	event   string
	done    chan struct{}
}

// Map attaches the image to its drive type's tunnel, opening the tunnel if needed.
func (s *mediaSession) Map(ctx context.Context, image vmedia.Image, o vmedia.MapOptions) (uint16, error) {
	drive, err := vmedia.NewDrive(image, o)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return 0, errors.New("the virtual media session has ended")
	}
	for _, d := range s.slots {
		if d.drive.Target == o.Target {
			s.mu.Unlock()
			return 0, fmt.Errorf("the %s drive is already mapped", o.Target)
		}
	}
	s.next++
	slot := &mediaSlot{view: s, disk: s.next, drive: drive, target: atenvm.NewTarget(drive)}
	s.mu.Unlock()

	t, err := s.pool.attach(ctx, o.Target, slot)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		s.pool.eject(t, slot)
		return 0, fmt.Errorf("the %s drive could not be mapped: the session ended", o.Target)
	}
	s.slots[slot.disk], s.tunnels[slot.disk] = slot, t
	s.mu.Unlock()
	log.Printf("[MegaRAC] %s drive attached (%d blocks of %d bytes)", o.Target, drive.Blocks, drive.BlockSize)
	return slot.disk, nil
}

// end parks every tunnel this view uses and marks it done with err.
func (s *mediaSession) end(err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.err = true, err
	if err != nil {
		s.event = err.Error()
	}
	slots, tunnels := s.slots, s.tunnels
	s.slots, s.tunnels = map[uint16]*mediaSlot{}, map[uint16]*mediaTunnel{}
	s.mu.Unlock()
	for disk, slot := range slots {
		s.pool.eject(tunnels[disk], slot)
	}
	close(s.done)
}

// Unmap takes the image out of its drive; the tunnel stays open with the drive empty.
func (s *mediaSession) Unmap(disk uint16) error {
	s.mu.Lock()
	slot, t := s.slots[disk], s.tunnels[disk]
	if slot == nil {
		s.mu.Unlock()
		return fmt.Errorf("no drive %d is mapped", disk)
	}
	delete(s.slots, disk)
	delete(s.tunnels, disk)
	s.mu.Unlock()
	s.pool.eject(t, slot)
	log.Printf("[MegaRAC] %s drive ejected (the drive stays connected, empty)", slot.drive.Target)
	return nil
}

// Drives reports the mapped drives.
func (s *mediaSession) Drives() []vmedia.DriveStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]vmedia.DriveStatus, 0, len(s.slots))
	for disk, d := range s.slots {
		out = append(out, d.drive.Status(disk))
	}
	return out
}

// USBReset is not available: JViewer has no such command for MegaRAC.
func (s *mediaSession) USBReset() error { return errMediaUSBReset }

// Close ejects every image this view attached. The tunnels stay open until the pool closes.
func (s *mediaSession) Close() error {
	s.end(nil)
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
