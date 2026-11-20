package main

import (
	"context"
	"net"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/web"
)

// fakeUI is a native window that closes as soon as it has its launch link, recording which of
// its native services the server was given by then.
type fakeUI struct {
	srv                       *web.Server
	clipboard, save, pickFile bool
}

func (f *fakeUI) Run(_ context.Context, links <-chan string) error {
	<-links
	f.clipboard = f.srv.ReadClipboard != nil
	f.save = f.srv.SaveFile != nil
	f.pickFile = f.srv.PickFile != nil
	return nil
}

func (f *fakeUI) SaveFile(context.Context, string, string, []byte) (string, error) { return "", nil }
func (f *fakeUI) PickFiles(context.Context, string, bool) ([]string, error)        { return nil, nil }
func (f *fakeUI) ReadClipboard(context.Context) (string, error)                    { return "", nil }

func TestServeInWindowGivesTheServerNativeServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := web.NewServer(0, bmc.SessionState{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{srv: srv}
	if err := serveInWindow(ctx, &app{server: srv}, ui, listener, cancel); err != nil {
		t.Fatalf("serveInWindow: %v", err)
	}
	if !ui.clipboard || !ui.save || !ui.pickFile {
		t.Errorf("native services wired: clipboard %v, save %v, pick %v", ui.clipboard, ui.save, ui.pickFile)
	}
}
