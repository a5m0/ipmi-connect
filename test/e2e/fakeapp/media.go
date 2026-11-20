package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/vmedia"
)

// fakeReadRate is how fast the fake host reads a mapped image, so the transfer readouts move.
const (
	fakeReadChunk = 64 << 10
	fakeReadEvery = 100 * time.Millisecond
)

// fakeMediaSession is a virtual media session whose "host" keeps reading every mapped drive.
// It drives the real vmedia.Manager, so the panel, the stats bar and the graph show real data.
type fakeMediaSession struct {
	mu     sync.Mutex
	drives map[uint16]*fakeDrive
	next   uint16
	done   chan struct{}
	once   sync.Once
}

type fakeDrive struct {
	opts   vmedia.MapOptions
	read   int64
	cancel context.CancelFunc
}

func openFakeMedia(context.Context) (vmedia.Session, error) {
	return &fakeMediaSession{drives: map[uint16]*fakeDrive{}, done: make(chan struct{})}, nil
}

func (f *fakeMediaSession) Map(_ context.Context, image vmedia.Image, o vmedia.MapOptions) (uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.drives {
		if d.opts.Target == o.Target {
			return 0, errors.New("drive in use")
		}
	}
	f.next++
	disk := f.next
	ctx, cancel := context.WithCancel(context.Background())
	d := &fakeDrive{opts: o, cancel: cancel}
	f.drives[disk] = d
	go func() {
		buf := make([]byte, fakeReadChunk)
		var off int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(fakeReadEvery):
			}
			if o.Size > 0 {
				_, _ = image.ReadAt(buf, off%o.Size)
			}
			off += fakeReadChunk
			f.mu.Lock()
			d.read += fakeReadChunk
			f.mu.Unlock()
		}
	}()
	return disk, nil
}

func (f *fakeMediaSession) Unmap(disk uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.drives[disk]
	if !ok {
		return errors.New("no such drive")
	}
	d.cancel()
	delete(f.drives, disk)
	return nil
}

func (f *fakeMediaSession) Drives() []vmedia.DriveStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vmedia.DriveStatus, 0, len(f.drives))
	for disk, d := range f.drives {
		out = append(out, vmedia.DriveStatus{Disk: disk, Target: d.opts.Target, ReadOnly: d.opts.ReadOnly,
			BlockSize: 2048, Blocks: uint32(d.opts.Size / 2048), BytesRead: d.read})
	}
	return out
}

func (f *fakeMediaSession) USBReset() error { return nil }
func (f *fakeMediaSession) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}
func (f *fakeMediaSession) Done() <-chan struct{} { return f.done }
func (f *fakeMediaSession) Err() error            { return nil }
func (f *fakeMediaSession) LastEvent() string     { return "" }
