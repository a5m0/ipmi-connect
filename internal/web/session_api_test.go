package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/config"
	"github.com/mhamzahkhan/ipmi-connect/internal/session"
)

type stubSessions struct {
	mu   sync.Mutex
	err  error
	reqs []session.Request
}

func (s *stubSessions) Connect(_ context.Context, r session.Request) (session.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	return session.Status{Phase: session.PhaseConnected}, s.err
}
func (s *stubSessions) Disconnect()            {}
func (s *stubSessions) Status() session.Status { return session.Status{Phase: session.PhaseIdle} }

func connectRequest(srv *Server, h http.Handler, body string) *httptest.ResponseRecorder {
	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestConnectAPIMapsErrorsWithoutEchoingThePassword(t *testing.T) {
	body := `{"address":"192.0.2.10","user":"root","password":"hunter2","trust_fingerprints":["AA"]}`
	for _, tt := range []struct {
		err  error
		code int
		want string
	}{
		{nil, http.StatusOK, `"phase":"connected"`},
		{&session.ConsentRequired{Device: "192.0.2.10", LegacyTLS: []string{"https (TLS 1.0)"}}, http.StatusConflict, `"legacy_tls":["https (TLS 1.0)"]`},
		{&session.InputError{Err: errors.New("enter the BMC password")}, http.StatusBadRequest, "enter the BMC password"},
		{session.ErrBusy, http.StatusConflict, "already in progress"},
		{errors.New("login failed"), http.StatusBadGateway, "login failed"},
	} {
		stub := &stubSessions{err: tt.err}
		srv, h, _ := newGuardedServer(t)
		srv.Sessions = stub
		rec := connectRequest(srv, h, body)
		if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("%v: %d %s", tt.err, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("%v: response echoes the password", tt.err)
		}
		if len(stub.reqs) != 1 || stub.reqs[0].Password != "hunter2" || stub.reqs[0].TrustFingerprints[0] != "AA" {
			t.Errorf("request reached the manager as %v", stub.reqs)
		}
	}
}

func TestProfilesAPIDropsPasswords(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	srv.Profiles = configMemory()
	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/profiles",
		strings.NewReader(`{"name":"r1","address":"192.0.2.10","user":"root","vendor":"dell","password":"hunter2"}`)))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodGet, "/api/profiles", nil)))
	if !strings.Contains(rec.Body.String(), `"address":"192.0.2.10"`) || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("list: %s", rec.Body.String())
	}
}

func TestDetachReleasesHeldInputAndEndsSockets(t *testing.T) {
	fc := &fakeConsole{}
	srv, url := startTestServer(t, fc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv, url), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"key","down":true,"hidCode":225}`))
	eventually(t, func() bool { k, _ := fc.snapshot(); return len(k) == 1 })

	srv.Detach() // returns only after the held key is released
	if keys, _ := fc.snapshot(); !reflect.DeepEqual(keys, []keyEvent{{true, 225}, {false, 225}}) {
		t.Errorf("keys after Detach = %v", keys)
	}
	ended := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		ended = ended || (strings.Contains(string(data), `"type":"session-ended"`) && strings.Contains(string(data), `"reason":"session ended"`))
	}
	if !ended {
		t.Error("the page was not told that the session ended")
	}
	if srv.console() != nil {
		t.Error("console still attached")
	}
	// No new socket can attach to a detached console.
	if _, resp, err := websocket.Dial(ctx, wsURL(srv, url), nil); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("socket opened with no BMC attached (%v)", err)
	}
}

func TestIdleTimeoutFiresOnlyWithoutBrowsers(t *testing.T) {
	srv := newTestServer(bmc.SessionState{})
	srv.IdleTimeout = 80 * time.Millisecond
	fired := make(chan struct{})
	srv.OnIdle = func() { close(fired) }
	srv.clientsMu.Lock()
	c := newWSClient(nil)
	srv.clients[c] = struct{}{}
	srv.clientsMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.watchIdle(ctx)
	select {
	case <-fired:
		t.Fatal("idle fired while a browser was connected")
	case <-time.After(250 * time.Millisecond):
	}
	srv.removeClient(c)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("idle never fired after the browser left")
	}
}

func configMemory() *config.Store { return config.NewMemoryStore() }
