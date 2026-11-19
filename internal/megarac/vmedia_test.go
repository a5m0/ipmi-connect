package megarac

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/debuglog"
	"github.com/a5m0/ipmi-connect/internal/vmedia"
)

// memImage is an in-memory disk image.
type memImage struct {
	mu   sync.Mutex
	data []byte
}

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copy(p, m.data[off:]), nil
}

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copy(m.data[off:], p), nil
}

func (m *memImage) bytes(off, n int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data[off:off+n]...)
}

func patternImage(n int) *memImage {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/2048)
	}
	return &memImage{data: b}
}

// fakeMediaBMC plays the BMC's end of one media tunnel.
type fakeMediaBMC struct {
	t       *testing.T
	service string
	conn    net.Conn
	r       *bufio.Reader
	seq     uint32
}

func (f *fakeMediaBMC) read() []byte {
	f.t.Helper()
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := readIUSB(f.r)
	if err != nil {
		f.t.Fatalf("fake BMC (%s) read: %v", f.service, err)
	}
	return frame
}

func (f *fakeMediaBMC) write(b []byte) {
	f.t.Helper()
	_ = f.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := f.conn.Write(b); err != nil {
		f.t.Fatalf("fake BMC (%s) write: %v", f.service, err)
	}
}

// accept reads the auth packet and answers with status (1 = connected).
func (f *fakeMediaBMC) accept(status byte) []byte {
	f.t.Helper()
	auth := f.read()
	body := make([]byte, mediaAuthLen-iusbHeaderLen)
	body[scsiCDBAt-iusbHeaderLen] = opMediaAuthReply
	body[mediaAuthStatAt-iusbHeaderLen] = status
	copy(body[mediaAuthStatAt+1-iusbHeaderLen:], "198.51.100.7")
	f.write(iusbFrame(0, body))
	return auth
}

type scsiResult struct {
	frame            []byte
	status, key, asc byte
	ascq             byte
	data             []byte
}

// scsi sends a request and returns the client's reply.
func (f *fakeMediaBMC) scsi(cdb []byte, data []byte) scsiResult {
	f.t.Helper()
	f.seq++
	req := scsiRequest(f.seq, cdb, data)
	f.write(req)
	rep := f.read()
	if len(rep) < scsiDataAt {
		f.t.Fatalf("reply of %d bytes", len(rep))
	}
	if got := binary.LittleEndian.Uint32(rep[24:]); got != f.seq {
		f.t.Errorf("reply sequence %d, want %d", got, f.seq)
	}
	if !bytes.Equal(rep[32:53], req[32:53]) {
		f.t.Errorf("reply does not echo bytes 32..52: % x", rep[32:53])
	}
	n := binary.LittleEndian.Uint32(rep[scsiDataLenAt:])
	if int(n) != len(rep)-scsiDataAt {
		f.t.Errorf("data length field %d, frame carries %d", n, len(rep)-scsiDataAt)
	}
	return scsiResult{rep, rep[53], rep[54], rep[55], rep[56], rep[scsiDataAt:]}
}

type mediaHarness struct {
	t       *testing.T
	pool    *mediaPool
	sess    *mediaSession // a view on pool, as the Manager gets it
	tunnels chan *fakeMediaBMC
	states  chan bool
	dials   atomic.Int32
	clockMu sync.Mutex
	clock   time.Time // the pool's clock; advance moves it on
}

func (h *mediaHarness) now() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	if h.clock.IsZero() {
		h.clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return h.clock
}

func (h *mediaHarness) advance(d time.Duration) {
	h.now()
	h.clockMu.Lock()
	h.clock = h.clock.Add(d)
	h.clockMu.Unlock()
}

func newMediaHarness(t *testing.T) *mediaHarness {
	h := &mediaHarness{t: t, tunnels: make(chan *fakeMediaBMC, 4), states: make(chan bool, 8)}
	dial := func(_ context.Context, service string) (net.Conn, string, error) {
		h.dials.Add(1)
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		h.tunnels <- &fakeMediaBMC{t: t, service: service, conn: server, r: bufio.NewReader(server)}
		return client, "KVMTOKEN0123456", nil
	}
	h.pool = newMediaPool(dial, func(on bool) { h.states <- on })
	h.pool.now = h.now
	h.sess = h.pool.view()
	t.Cleanup(func() { _ = h.sess.Close(); h.pool.Close() })
	return h
}

// noState checks that no media state was sent.
func (h *mediaHarness) noState() {
	h.t.Helper()
	select {
	case on := <-h.states:
		h.t.Errorf("unexpected media state %v", on)
	default:
	}
}

// mapParked maps image onto a parked tunnel, which must not dial.
func (h *mediaHarness) mapParked(img vmedia.Image, o vmedia.MapOptions) uint16 {
	h.t.Helper()
	disk, err := h.sess.Map(context.Background(), img, o)
	if err != nil {
		h.t.Fatalf("Map onto a parked tunnel: %v", err)
	}
	select {
	case <-h.tunnels:
		h.t.Fatal("Map dialled a new tunnel instead of reusing the parked one")
	default:
	}
	return disk
}

// diskOf returns the disk number the view reports for target.
func (h *mediaHarness) diskOf(target vmedia.Target) uint16 {
	h.t.Helper()
	for _, d := range h.sess.Drives() {
		if d.Target == target {
			return d.Disk
		}
	}
	h.t.Fatalf("no %s drive in %+v", target, h.sess.Drives())
	return 0
}

// sense checks a reply's status and sense.
func sense(t *testing.T, what string, r scsiResult, status, key, asc, ascq byte) {
	t.Helper()
	if r.status != status || r.key != key || r.asc != asc || r.ascq != ascq {
		t.Errorf("%s: status %d sense %02x/%02x/%02x, want %d %02x/%02x/%02x", what, r.status, r.key, r.asc, r.ascq, status, key, asc, ascq)
	}
}

// mapAsync maps image and returns the BMC end of its tunnel and the Map result.
func (h *mediaHarness) mapAsync(img vmedia.Image, o vmedia.MapOptions) (*fakeMediaBMC, <-chan error) {
	h.t.Helper()
	res := make(chan error, 1)
	go func() {
		_, err := h.sess.Map(context.Background(), img, o)
		res <- err
	}()
	select {
	case f := <-h.tunnels:
		return f, res
	case err := <-res:
		h.t.Fatalf("Map returned before dialling: %v", err)
	case <-time.After(5 * time.Second):
		h.t.Fatal("Map did not dial")
	}
	return nil, nil
}

func (h *mediaHarness) mapOK(img vmedia.Image, o vmedia.MapOptions) *fakeMediaBMC {
	h.t.Helper()
	f, res := h.mapAsync(img, o)
	f.accept(1)
	if err := <-res; err != nil {
		h.t.Fatalf("Map: %v", err)
	}
	return f
}

func (h *mediaHarness) state(want bool) {
	h.t.Helper()
	select {
	case got := <-h.states:
		if got != want {
			h.t.Errorf("media state %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no media state %v sent", want)
	}
}

func TestMediaServicesAndAuth(t *testing.T) {
	for _, tc := range []struct {
		target     vmedia.Target
		service    string
		clientData byte
		size       int
	}{
		{vmedia.TargetCD, serviceCD, 0, 4 * 2048},
		{vmedia.TargetFloppy, serviceFloppy, 0, 1440 << 10},
		{vmedia.TargetRemovable, serviceHardDisk, mediaImageType, 4 << 20},
	} {
		h := newMediaHarness(t)
		f, res := h.mapAsync(patternImage(tc.size), vmedia.MapOptions{Target: tc.target, Size: int64(tc.size)})
		if f.service != tc.service {
			t.Errorf("%s: tunnel service %q, want %q", tc.target, f.service, tc.service)
		}
		auth := f.accept(1)
		if err := <-res; err != nil {
			t.Fatalf("%s: Map: %v", tc.target, err)
		}
		want, _ := mediaAuthPacket("KVMTOKEN0123456", tc.clientData)
		if !bytes.Equal(auth, want) {
			t.Errorf("%s: auth packet\n% x\nwant\n% x", tc.target, auth, want)
		}
		h.state(true)
		if d := h.sess.Drives(); len(d) != 1 || d[0].Target != tc.target {
			t.Errorf("%s: drives %+v", tc.target, d)
		}
	}
}

func TestMediaAuthRefused(t *testing.T) {
	for status, want := range map[byte]string{5: "privilege", 8: "two minutes", 3: "198.51.100.7"} {
		h := newMediaHarness(t)
		f, res := h.mapAsync(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
		f.accept(status)
		err := <-res
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: error %v, want it to mention %q", status, err, want)
		}
		if len(h.sess.Drives()) != 0 {
			t.Errorf("status %d: a refused drive is listed", status)
		}
	}
}

func TestMediaAuthUnanswered(t *testing.T) {
	h := newMediaHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() {
		f := <-h.tunnels
		f.read() // the auth packet, never answered
	}()
	_, err := h.sess.Map(ctx, patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	if err == nil || !strings.Contains(err.Error(), "two minutes") {
		t.Errorf("unanswered auth: %v, want a hint to retry in two minutes", err)
	}
}

func TestMediaCDReads(t *testing.T) {
	h := newMediaHarness(t)
	img := patternImage(8 * 2048)
	f := h.mapOK(img, vmedia.MapOptions{Target: vmedia.TargetCD, Size: 8 * 2048})

	if r := f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil); r.status != 0 {
		t.Errorf("TEST UNIT READY: status %d sense %x/%x", r.status, r.key, r.asc)
	}
	r := f.scsi([]byte{0x25, 0, 0, 0, 0, 0, 0, 0, 0, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, []byte{0, 0, 0, 7, 0, 0, 8, 0}) {
		t.Errorf("READ CAPACITY: status %d data % x", r.status, r.data)
	}
	r = f.scsi([]byte{0x28, 0, 0, 0, 0, 2, 0, 0, 3, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, img.bytes(2*2048, 3*2048)) {
		t.Errorf("READ(10) lba 2 x3: status %d, %d bytes", r.status, len(r.data))
	}
	r = f.scsi([]byte{0xA8, 0, 0, 0, 0, 7, 0, 0, 0, 1, 0, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, img.bytes(7*2048, 2048)) {
		t.Errorf("READ(12) lba 7 x1: status %d, %d bytes", r.status, len(r.data))
	}
	// Past the end: CHECK CONDITION with the sense in the reply.
	r = f.scsi([]byte{0x28, 0, 0, 0, 0, 7, 0, 0, 2, 0}, nil)
	if r.status != 1 || r.key != 0x05 || r.asc != 0x21 || len(r.data) != 0 {
		t.Errorf("READ past the end: status %d sense %02x/%02x, %d bytes", r.status, r.key, r.asc, len(r.data))
	}
	// More than one reply can carry.
	r = f.scsi([]byte{0x28, 0, 0, 0, 0, 0, 0, 0, 0x41, 0}, nil)
	if r.status != 1 || r.key != 0x05 || r.asc != 0x24 {
		t.Errorf("oversized READ: status %d sense %02x/%02x", r.status, r.key, r.asc)
	}
	// Writes to a CD are refused.
	r = f.scsi([]byte{0x2A, 0, 0, 0, 0, 0, 0, 0, 1, 0}, make([]byte, 2048))
	if r.status != 1 || r.key != 0x07 {
		t.Errorf("WRITE to CD: status %d sense %02x", r.status, r.key)
	}
	if st := h.sess.Drives()[0]; st.BytesRead != 4*2048 {
		t.Errorf("BytesRead %d", st.BytesRead)
	}
}

func TestMediaDiskWrites(t *testing.T) {
	h := newMediaHarness(t)
	img := patternImage(64 * 512)
	f := h.mapOK(img, vmedia.MapOptions{Target: vmedia.TargetRemovable, Size: 64 * 512})
	block := bytes.Repeat([]byte{0xAB}, 2*512)
	if r := f.scsi([]byte{0x2A, 0, 0, 0, 0, 5, 0, 0, 2, 0}, block); r.status != 0 {
		t.Fatalf("WRITE(10): status %d sense %02x/%02x", r.status, r.key, r.asc)
	}
	if !bytes.Equal(img.bytes(5*512, 2*512), block) {
		t.Error("WRITE(10) did not reach the image")
	}
	// A data length that does not match the CDB is refused untouched.
	before := img.bytes(9*512, 512)
	if r := f.scsi([]byte{0x2A, 0, 0, 0, 0, 9, 0, 0, 2, 0}, make([]byte, 512)); r.status != 1 {
		t.Error("short WRITE accepted")
	}
	if !bytes.Equal(img.bytes(9*512, 512), before) {
		t.Error("short WRITE changed the image")
	}

	ro := newMediaHarness(t)
	roImg := patternImage(64 * 512)
	g := ro.mapOK(roImg, vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 64 * 512, ReadOnly: true})
	r := g.scsi([]byte{0x2A, 0, 0, 0, 0, 0, 0, 0, 1, 0}, make([]byte, 512))
	if r.status != 1 || r.key != 0x07 || r.asc != 0x27 {
		t.Errorf("WRITE to read-only: status %d sense %02x/%02x", r.status, r.key, r.asc)
	}
	if !bytes.Equal(roImg.bytes(0, 512), patternImage(512).bytes(0, 512)) {
		t.Error("read-only image changed")
	}
}

func TestMediaHardDiskTypeQuery(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(64*512), vmedia.MapOptions{Target: vmedia.TargetRemovable, Size: 64 * 512})
	// JViewer answers with the drive type appended after an empty SCSI reply.
	f.write(scsiRequest(9, []byte{opMediaSetHDType}, nil))
	rep := f.read()
	if len(rep) != scsiDataAt+1 || rep[scsiDataAt] != mediaImageType {
		t.Errorf("drive type reply % x, want %02x at %d", rep, mediaImageType, scsiDataAt)
	}
	if binary.LittleEndian.Uint32(rep[24:]) != 9 {
		t.Error("drive type reply does not echo the sequence number")
	}
}

func TestMediaUnmapAndClose(t *testing.T) {
	h := newMediaHarness(t)
	h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048}); err == nil {
		t.Error("a second CD was mapped")
	}
	if drives := h.sess.Drives(); len(drives) != 1 {
		t.Fatalf("drives %+v", drives)
	}
	cdDisk := h.diskOf(vmedia.TargetCD)
	if err := h.sess.Unmap(cdDisk); err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Unmap(cdDisk); err == nil {
		t.Error("a second Unmap of the same disk succeeded")
	}
	if d := h.sess.Drives(); len(d) != 0 {
		t.Errorf("drives after the CD eject %+v", d)
	}
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if h.sess.Err() != nil {
		t.Errorf("Err after Close = %v", h.sess.Err())
	}
	h.noState()
}

func TestMediaEjectKeepsTunnel(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(4*2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 4 * 2048})
	h.state(true)
	if err := h.sess.Unmap(h.diskOf(vmedia.TargetCD)); err != nil {
		t.Fatal(err)
	}
	// The tunnel stays up and reports an empty drive.
	sense(t, "TEST UNIT READY after eject", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x02, 0x3A, 0)
	r := f.scsi([]byte{0x28, 0, 0, 0, 0, 0, 0, 0, 1, 0}, nil)
	sense(t, "READ after eject", r, 1, 0x02, 0x3A, 0)
	if len(r.data) != 0 {
		t.Errorf("READ after eject returned %d bytes", len(r.data))
	}
	if len(h.sess.Drives()) != 0 {
		t.Errorf("drives after eject %+v", h.sess.Drives())
	}
	h.noState()
}

func TestMediaRemapReusesTunnel(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(4*2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 4 * 2048})
	h.state(true)
	if err := h.sess.Unmap(h.diskOf(vmedia.TargetCD)); err != nil {
		t.Fatal(err)
	}
	second := &memImage{data: bytes.Repeat([]byte{0x5A}, 2*2048)}
	h.mapParked(second, vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2 * 2048})
	sense(t, "first TEST UNIT READY after the swap", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x06, 0x28, 0)
	sense(t, "second TEST UNIT READY", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 0, 0, 0, 0)
	r := f.scsi([]byte{0x28, 0, 0, 0, 0, 1, 0, 0, 1, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, second.bytes(2048, 2048)) {
		t.Errorf("READ of the new image: status %d, %d bytes", r.status, len(r.data))
	}
	if n := h.dials.Load(); n != 1 {
		t.Errorf("%d dials, want 1", n)
	}
	h.noState()
}

func TestMediaViewCloseKeepsTunnelPoolCloseEndsIt(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	<-h.sess.Done()
	sense(t, "TEST UNIT READY after the view closed", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x02, 0x3A, 0)
	h.noState()

	// A new view reuses the parked tunnel.
	h.sess = h.pool.view()
	h.mapParked(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})

	h.pool.Close()
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := f.r.ReadByte(); err == nil {
		t.Error("tunnel still open after the pool closed")
	}
	h.state(false)
	select {
	case <-h.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the pool did not end the view")
	}
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 2048}); err == nil {
		t.Error("Map after the pool closed succeeded")
	}
}

func TestMediaPrivateOpcodes(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	// The BMC sends 0xF3 (with one data byte) about a minute in; JViewer answers 05/20/00.
	probe := func(what string) {
		t.Helper()
		f.seq++
		f.write(scsiRequest(f.seq, []byte{0xF3}, []byte{0}))
		rep := f.read()
		if len(rep) != scsiDataAt || rep[53] != 1 || rep[54] != 0x05 || rep[55] != 0x20 || rep[56] != 0 {
			t.Errorf("%s: 0xF3 reply % x", what, rep[min(len(rep), 53):])
		}
	}
	probe("mapped")
	if err := h.sess.Unmap(h.diskOf(vmedia.TargetCD)); err != nil {
		t.Fatal(err)
	}
	probe("empty")
	h.mapParked(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	probe("after the swap")
	// The private opcode must not use up the medium-change report.
	sense(t, "TEST UNIT READY after 0xF3", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x06, 0x28, 0)
}

// The ASMB8 serves one media tunnel at a time: a second drive type's auth goes unanswered
// while another tunnel is open, and the failed attempt leaves that type busy for minutes.
func TestMediaOneDriveAtATime(t *testing.T) {
	h := newMediaHarness(t)
	h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	_, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 2048})
	if err == nil || !strings.Contains(err.Error(), "one virtual drive at a time") || !strings.Contains(err.Error(), "cd") {
		t.Errorf("second drive type: %v, want a one-at-a-time error naming the cd drive", err)
	}
	if n := h.dials.Load(); n != 1 {
		t.Errorf("%d dials, want 1: the second drive must be refused before dialling", n)
	}
}

func TestMediaOtherTypeReplacesParkedTunnel(t *testing.T) {
	h := newMediaHarness(t)
	cd := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	if err := h.sess.Unmap(h.diskOf(vmedia.TargetCD)); err != nil {
		t.Fatal(err)
	}
	// The parked CD is closed to free the BMC's one slot, which it frees about two minutes
	// later: dialling sooner would go unanswered and keep it busy longer.
	_, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 2048})
	if err == nil || !strings.Contains(err.Error(), "try again in 2m0s") {
		t.Errorf("floppy straight after the CD: %v, want a try-again-in-2m0s error", err)
	}
	_ = cd.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := cd.r.ReadByte(); err == nil {
		t.Error("the parked CD tunnel is still open after asking for the floppy")
	}
	if n := h.dials.Load(); n != 1 {
		t.Errorf("%d dials, want 1", n)
	}
	h.state(false)
	h.advance(90 * time.Second)
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 2048}); err == nil || !strings.Contains(err.Error(), "try again in 30s") {
		t.Errorf("floppy 90s later: %v, want try again in 30s", err)
	}
	h.advance(30 * time.Second)
	fd := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetFloppy, Size: 2048})
	h.state(true)
	sense(t, "floppy TEST UNIT READY", fd.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 0, 0, 0, 0)
}

func TestMediaFailedAuthStartsTheWait(t *testing.T) {
	h := newMediaHarness(t)
	f, res := h.mapAsync(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	f.accept(8)
	if err := <-res; err == nil {
		t.Fatal("refused auth mapped")
	}
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048}); err == nil || !strings.Contains(err.Error(), "try again in") {
		t.Errorf("retry straight after a refusal: %v, want a try-again error", err)
	}
	if n := h.dials.Load(); n != 1 {
		t.Errorf("%d dials, want 1", n)
	}
}

func TestMediaParkedTunnelLossRedials(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	if err := h.sess.Unmap(h.diskOf(vmedia.TargetCD)); err != nil {
		t.Fatal(err)
	}
	_ = f.conn.Close()
	h.state(false) // the last tunnel closed
	select {
	case <-h.sess.Done():
		t.Fatal("losing a parked tunnel ended the view")
	default:
	}
	h.advance(mediaSettle)
	g := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	sense(t, "TEST UNIT READY on the new tunnel", g.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 0, 0, 0, 0)
	if n := h.dials.Load(); n != 2 {
		t.Errorf("%d dials, want 2", n)
	}
}

func TestMediaManagerRemapDialsOnce(t *testing.T) {
	h := newMediaHarness(t)
	m := vmedia.NewManager(func(context.Context) (vmedia.Session, error) { return h.pool.view(), nil })
	t.Cleanup(m.Close)
	dir := t.TempDir()
	var ids []string
	for i, name := range []string{"one.iso", "two.iso"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i + 1)}, 4*2048), 0o600); err != nil {
			t.Fatal(err)
		}
		im, err := m.AddImage(path, true)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, im.ID)
	}
	mapped := make(chan error, 1)
	go func() { mapped <- m.MapImage(ids[0], bmc.TargetCD) }()
	var f *fakeMediaBMC
	select {
	case f = <-h.tunnels:
	case <-time.After(5 * time.Second):
		t.Fatal("no tunnel dialled")
	}
	f.accept(1)
	if err := <-mapped; err != nil {
		t.Fatal(err)
	}
	if err := m.UnmapDrive(bmc.TargetCD); err != nil {
		t.Fatal(err)
	}
	if err := m.MapImage(ids[1], bmc.TargetCD); err != nil {
		t.Fatalf("MapImage after eject: %v", err)
	}
	if n := h.dials.Load(); n != 1 {
		t.Errorf("%d dials, want 1", n)
	}
	sense(t, "TEST UNIT READY after the swap", f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil), 1, 0x06, 0x28, 0)
	r := f.scsi([]byte{0x28, 0, 0, 0, 0, 0, 0, 0, 1, 0}, nil)
	if r.status != 0 || !bytes.Equal(r.data, bytes.Repeat([]byte{2}, 2048)) {
		t.Errorf("READ of the second image: status %d, %d bytes", r.status, len(r.data))
	}
}

func TestMediaKillEndsSession(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	f.scsi([]byte{opMediaKill}, nil)
	select {
	case <-h.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not end the session")
	}
	if err := h.sess.Err(); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Err = %v", err)
	}
}

func TestMediaTunnelLossEndsSession(t *testing.T) {
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	_ = f.conn.Close()
	select {
	case <-h.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a lost tunnel did not end the session")
	}
	if h.sess.Err() == nil {
		t.Error("want an error for a lost tunnel")
	}
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048}); err == nil {
		t.Error("Map after the session ended succeeded")
	}
}

func TestMediaUSBResetUnsupported(t *testing.T) {
	h := newMediaHarness(t)
	if err := h.sess.USBReset(); !errors.Is(err, errMediaUSBReset) {
		t.Errorf("USBReset = %v", err)
	}
}

func TestClientMediaClosedWithTheKVM(t *testing.T) {
	// Slow enough that the media tunnel's close is handled before the new KVM session starts.
	fastReconnect(t, 300*time.Millisecond)
	f := newFakeBMCServer(t)
	c, m, _, stop := startMappedCD(t, f)

	// The BMC drops the KVM session and the media tunnel with it.
	f.dropKVM()
	_ = m.conn.Close()
	waitFor(t, "the drive to be released", func() bool {
		st := c.MediaStatus()
		return len(st.Drives) == 0 && st.Error != ""
	})
	waitFor(t, "the new KVM session", func() bool { return f.servedKVM() == 2 })
	// Mapping again must wait out the BMC's two minutes rather than dial into a held slot.
	im := c.MediaStatus().Images[0]
	err := c.MapImage(im.ID, bmc.TargetCD)
	if err == nil || !strings.Contains(err.Error(), "try again in") {
		t.Errorf("MapImage straight after: %v, want the two-minute wait", err)
	}
	select {
	case <-f.media:
		t.Error("a media tunnel was dialled inside the two minutes")
	default:
	}
	stop()
	if got := f.kvmEvents(2); strings.Contains(got, "media 1") {
		t.Errorf("new KVM session messages %q: no drive is connected", got)
	}
}

// traceOn switches the megarac-media trace on for the test and captures the log.
func traceOn(t *testing.T, on bool) *lockedBuffer {
	t.Helper()
	debuglog.Reset()
	if on {
		debuglog.Enable(debuglog.MegaRACMedia)
	}
	buf := &lockedBuffer{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { debuglog.Reset(); log.SetOutput(out); log.SetFlags(flags) })
	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMediaTraceSCSI(t *testing.T) {
	buf := traceOn(t, true)
	h := newMediaHarness(t)
	img := patternImage(8 * 2048)
	f := h.mapOK(img, vmedia.MapOptions{Target: vmedia.TargetCD, Size: 8 * 2048})
	f.scsi([]byte{0x00, 0, 0, 0, 0, 0}, nil)
	r := f.scsi([]byte{0x28, 0, 0, 0, 0, 2, 0, 0, 3, 0}, nil)
	if r.status != 0 {
		t.Fatalf("READ: status %d", r.status)
	}
	got := buf.String()
	for _, want := range []string{
		"[Debug megarac-media] ",
		"op=28 cdb=28 00 00 00 00 02 00 00 03 00 00 00 -> st=00/00/00/00 len=6144",
		"cd op=00 cdb=00 00 00 00 00 00 00 00 00 00 00 00 -> st=00/00/00/00 len=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	// Never the data, in any form.
	for off := 0; off+8 <= len(r.data); off += 512 {
		run := r.data[off : off+8]
		if strings.Contains(got, string(run)) || strings.Contains(got, fmt.Sprintf("% x", run)) || strings.Contains(got, fmt.Sprintf("%x", run)) {
			t.Fatalf("trace carries image data at %d:\n%s", off, got)
		}
	}
}

func TestMediaTraceOff(t *testing.T) {
	buf := traceOn(t, false)
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(8*2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 8 * 2048})
	f.scsi([]byte{0x28, 0, 0, 0, 0, 2, 0, 0, 3, 0}, nil)
	if got := buf.String(); strings.Contains(got, "[Debug") {
		t.Errorf("traced while off:\n%s", got)
	}
}

func TestMediaTracePrivateTail(t *testing.T) {
	buf := traceOn(t, true)
	h := newMediaHarness(t)
	f := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	f.scsi([]byte{0xF3}, []byte{0x02})
	if got := buf.String(); !strings.Contains(got, "op=f3 cdb=f3 00") || !strings.Contains(got, "-> st=01/05/20/00 len=0 tail=02") {
		t.Errorf("trace:\n%s", got)
	}
}

func TestDescribeMediaRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   byte
		data []byte
		tail string
	}{
		{"private, no data", 0xF3, nil, ""},
		{"private, 4 bytes", 0xF3, []byte{1, 2, 3, 4}, " tail=01 02 03 04"},
		{"private, 5 bytes", 0xF3, []byte{1, 2, 3, 4, 5}, ""},
		{"SCSI write data is never shown", 0x2A, []byte{9}, ""},
	} {
		req := scsiRequest(1, []byte{tc.op}, tc.data)
		rep := scsiReply(req, 1, 0x05, 0x20, 0, nil)
		got := describeMediaRequest(vmedia.TargetCD, req, rep)
		if !strings.HasSuffix(got, "len=0"+tc.tail) {
			t.Errorf("%s: %q, want suffix %q", tc.name, got, "len=0"+tc.tail)
		}
		if !strings.HasPrefix(got, vmedia.TargetCD.String()+" op=") {
			t.Errorf("%s: %q does not start with the drive", tc.name, got)
		}
	}
}
