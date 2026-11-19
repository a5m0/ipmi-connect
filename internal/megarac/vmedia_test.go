package megarac

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

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
	sess    *mediaSession
	tunnels chan *fakeMediaBMC
	states  chan bool
}

func newMediaHarness(t *testing.T) *mediaHarness {
	h := &mediaHarness{t: t, tunnels: make(chan *fakeMediaBMC, 4), states: make(chan bool, 8)}
	dial := func(_ context.Context, service string) (net.Conn, error) {
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		h.tunnels <- &fakeMediaBMC{t: t, service: service, conn: server, r: bufio.NewReader(server)}
		return client, nil
	}
	h.sess = newMediaSession(dial, "KVMTOKEN0123456", func(on bool) { h.states <- on })
	t.Cleanup(func() { _ = h.sess.Close() })
	return h
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
	for status, want := range map[byte]string{5: "privilege", 8: "maximum", 3: "198.51.100.7"} {
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
	cd := h.mapOK(patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048})
	h.state(true)
	h.mapOK(patternImage(64*512), vmedia.MapOptions{Target: vmedia.TargetRemovable, Size: 64 * 512})
	if _, err := h.sess.Map(context.Background(), patternImage(2048), vmedia.MapOptions{Target: vmedia.TargetCD, Size: 2048}); err == nil {
		t.Error("a second CD was mapped")
	}
	drives := h.sess.Drives()
	if len(drives) != 2 {
		t.Fatalf("drives %+v", drives)
	}
	var cdDisk uint16
	for _, d := range drives {
		if d.Target == vmedia.TargetCD {
			cdDisk = d.Disk
		}
	}
	if err := h.sess.Unmap(cdDisk); err != nil {
		t.Fatal(err)
	}
	_ = cd.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := cd.r.ReadByte(); err == nil {
		t.Error("CD tunnel still open after Unmap")
	}
	select {
	case on := <-h.states:
		t.Errorf("media state %v sent while a drive is still mapped", on)
	default:
	}
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	h.state(false)
	select {
	case <-h.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if h.sess.Err() != nil {
		t.Errorf("Err after Close = %v", h.sess.Err())
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
