package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// clipboardSecret stands in for clipboard contents that must never reach the log.
const clipboardSecret = "clipboard-secret-6c1f"

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

func getClipboard(srv *Server, h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodGet, "/api/clipboard", nil)))
	return rec
}

func TestClipboardWithoutNativeUI(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	rec := getClipboard(srv, h)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", rec.Code)
	}
}

func TestClipboardReturnsText(t *testing.T) {
	logs := captureLog(t)
	srv, h, _ := newGuardedServer(t)
	srv.ReadClipboard = func(context.Context) (string, error) { return clipboardSecret + "\nline 2", nil }
	rec := getClipboard(srv, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil || resp.Text != clipboardSecret+"\nline 2" {
		t.Errorf("response %+v (%v)", resp, err)
	}
	if strings.Contains(logs.String(), clipboardSecret) {
		t.Errorf("clipboard contents were logged: %q", logs.String())
	}
}

func TestClipboardRefusesOversizeText(t *testing.T) {
	logs := captureLog(t)
	srv, h, _ := newGuardedServer(t)
	big := clipboardSecret + strings.Repeat("x", maxClipboardBytes)
	srv.ReadClipboard = func(context.Context) (string, error) { return big, nil }
	rec := getClipboard(srv, h)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", rec.Code)
	}
	if strings.Contains(rec.Body.String(), clipboardSecret) || strings.Contains(logs.String(), clipboardSecret) {
		t.Error("oversize clipboard contents leaked into the response or the log")
	}

	// Exactly the limit is still accepted.
	srv.ReadClipboard = func(context.Context) (string, error) { return strings.Repeat("y", maxClipboardBytes), nil }
	if rec := getClipboard(srv, h); rec.Code != http.StatusOK {
		t.Errorf("text at the limit: status %d, want 200", rec.Code)
	}
}

func TestClipboardReadError(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	srv.ReadClipboard = func(context.Context) (string, error) { return "", errors.New("no clipboard") }
	if rec := getClipboard(srv, h); rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", rec.Code)
	}
}

func TestClipboardNeedsSession(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	called := false
	srv.ReadClipboard = func(context.Context) (string, error) { called = true; return clipboardSecret, nil }
	for _, tt := range []struct {
		name string
		req  func() *http.Request
		want int
	}{
		{"no token", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/clipboard", nil)
		}, http.StatusUnauthorized},
		{"wrong token", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/clipboard", nil)
			r.Header.Set("Authorization", "Bearer nope")
			return r
		}, http.StatusUnauthorized},
		{"another origin", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodGet, "/api/clipboard", nil))
			r.Header.Set("Origin", "https://evil.example")
			return r
		}, http.StatusForbidden},
		{"cross-site fetch", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodGet, "/api/clipboard", nil))
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			return r
		}, http.StatusForbidden},
		{"DNS rebinding Host", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodGet, "/api/clipboard", nil))
			r.Host = "evil.example:8080"
			return r
		}, http.StatusForbidden},
		{"POST", func() *http.Request {
			return authed(srv, httptest.NewRequest(http.MethodPost, "/api/clipboard", nil))
		}, http.StatusMethodNotAllowed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.req())
			if rec.Code != tt.want {
				t.Errorf("status %d, want %d", rec.Code, tt.want)
			}
			if strings.Contains(rec.Body.String(), clipboardSecret) {
				t.Error("a refused request got the clipboard")
			}
		})
	}
	if called {
		t.Error("a refused request read the clipboard")
	}
}

func TestStatusReportsNativeClipboard(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	flag := func() bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodGet, "/api/status", nil)))
		var st struct {
			Clipboard bool `json:"clipboard"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return st.Clipboard
	}
	if flag() {
		t.Error("clipboard flag set without a native clipboard")
	}
	srv.ReadClipboard = func(context.Context) (string, error) { return "", nil }
	if !flag() {
		t.Error("clipboard flag not set with a native clipboard")
	}
}
