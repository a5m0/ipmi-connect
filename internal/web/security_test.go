package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
)

// testPort is the port test servers pretend to be bound to; requests must name it in Host.
const testPort = 8080

// newTestServer returns a server that believes it is bound to testPort.
func newTestServer(state bmc.SessionState) *Server {
	s := NewServer(0, state)
	s.setBoundPort(testPort)
	return s
}

// authed makes r look like it came from the console page: this server's Host and the token.
func authed(s *Server, r *http.Request) *http.Request {
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Authorization", "Bearer "+s.auth.token)
	return r
}

// wsURL appends a single-use ticket, which the page fetches just before opening the socket.
func wsURL(s *Server, base string) string { return base + "?ticket=" + s.auth.issueTicket() }

func newGuardedServer(t *testing.T) (*Server, http.Handler, *fakeConsole) {
	t.Helper()
	fc := &fakeConsole{}
	srv := newTestServer(bmc.SessionState{})
	srv.SetConsole(fc)
	h, err := srv.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	return srv, h, fc
}

func exchangeCode(h http.Handler, code string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", strings.NewReader(`{"code":"`+code+`"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func launchCode(t *testing.T, s *Server) string {
	t.Helper()
	u := s.LaunchURL()
	_, code, ok := strings.Cut(u, "#c=")
	if !ok {
		t.Fatalf("launch URL %q has no code in its fragment", u)
	}
	return code
}

func TestLaunchCodeIsExchangedOnceForTheToken(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	code := launchCode(t, srv)

	if rec := exchangeCode(h, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong code: status %d, want 401", rec.Code)
	}
	// A wrong guess must not burn the real code... but the real code works only once.
	rec := exchangeCode(h, code)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), srv.auth.token) {
		t.Fatalf("first exchange: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := exchangeCode(h, code); rec.Code != http.StatusUnauthorized {
		t.Errorf("replayed code: status %d, want 401 (the launch link is visible in the process list)", rec.Code)
	}
	u := srv.LaunchURL()
	if strings.Contains(u, srv.auth.token) {
		t.Error("launch URL contains the session token")
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:8080/#c=") {
		t.Errorf("launch URL %q should carry the code in the fragment, which is never sent to a server", u)
	}
}

func TestLaunchCodeExpires(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	now := time.Now()
	srv.auth.now = func() time.Time { return now }
	srv.LaunchTTL = time.Minute
	code := launchCode(t, srv)
	now = now.Add(time.Minute + time.Second)
	rec := exchangeCode(h, code)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("expired code: status %d %q", rec.Code, rec.Body.String())
	}
	// A new link replaces the old one.
	old := launchCode(t, srv)
	fresh := launchCode(t, srv)
	if rec := exchangeCode(h, old); rec.Code != http.StatusUnauthorized {
		t.Errorf("superseded code accepted")
	}
	if rec := exchangeCode(h, fresh); rec.Code != http.StatusOK {
		t.Errorf("fresh code refused: %d", rec.Code)
	}
}

func TestLogoutInvalidatesTokenTicketsAndSockets(t *testing.T) {
	fc := &fakeConsole{}
	srv, url := startTestServer(t, fc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv, url), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if _, _, err := conn.Read(ctx); err != nil { // resize
		t.Fatal(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"key","down":true,"hidCode":4}`))
	eventually(t, func() bool { k, _ := fc.snapshot(); return len(k) == 1 })

	oldToken := srv.auth.token
	pending := srv.auth.issueTicket()
	h, _ := srv.routes()
	rec := httptest.NewRecorder()
	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	r.Host = srv.hostForTest()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}

	// The socket closes and its held key is released.
	eventually(t, func() bool { k, _ := fc.snapshot(); return len(k) == 2 })
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break
		}
	}
	// The old token and an outstanding ticket no longer work.
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	r.Host = srv.hostForTest()
	r.Header.Set("Authorization", "Bearer "+oldToken)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("old token after logout: %d", rec.Code)
	}
	if _, resp, err := websocket.Dial(ctx, url+"?ticket="+pending, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("ticket issued before logout still works (%v)", err)
	}
}

func TestWebSocketTicketsAreSingleUseAndExpire(t *testing.T) {
	srv, url := startTestServer(t, &fakeConsole{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticket := srv.auth.issueTicket()
	conn, _, err := websocket.Dial(ctx, url+"?ticket="+ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.CloseNow()
	if _, resp, err := websocket.Dial(ctx, url+"?ticket="+ticket, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("reused ticket accepted (%v)", err)
	}
	// The long-lived token is not accepted in the URL.
	if _, resp, err := websocket.Dial(ctx, url+"?t="+srv.auth.token, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token in the socket URL accepted (%v)", err)
	}
	now := time.Now()
	srv.auth.now = func() time.Time { return now }
	stale := srv.auth.issueTicket()
	now = now.Add(wsTicketTTL + time.Second)
	if _, resp, err := websocket.Dial(ctx, url+"?ticket="+stale, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired ticket accepted (%v)", err)
	}
}

func TestWSTicketNeedsToken(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/ws-ticket", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ticket without token: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodPost, "/api/ws-ticket", nil)))
	var got struct{ Ticket string }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Ticket) != 64 {
		t.Errorf("ticket: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHostAndOriginMustBeThisServer(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	for _, tt := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8080", "", http.StatusOK},
		{"localhost:8080", "http://localhost:8080", http.StatusOK},
		{"[::1]:8080", "http://[::1]:8080", http.StatusOK},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusOK},
		{"127.0.0.1", "", http.StatusForbidden},                           // no port
		{"127.0.0.1:9999", "", http.StatusForbidden},                      // another local port
		{"evil.example:8080", "", http.StatusForbidden},                   // DNS rebinding
		{"127.0.0.1:8080", "http://127.0.0.1:9999", http.StatusForbidden}, // another local service
		{"127.0.0.1:8080", "http://localhost.evil.example:8080", http.StatusForbidden},
		{"127.0.0.1:8080", "https://127.0.0.1:8080", http.StatusForbidden},
		{"127.0.0.1:8080", "null", http.StatusForbidden},
	} {
		r := authed(srv, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		r.Host = tt.host
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tt.want {
			t.Errorf("Host %q Origin %q: %d, want %d", tt.host, tt.origin, rec.Code, tt.want)
		}
	}
}

func TestSameSiteOtherPortIsRefused(t *testing.T) {
	srv, h, fc := newGuardedServer(t)
	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"power_on"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-site") // e.g. a page on http://127.0.0.1:3000
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || len(fc.power) != 0 {
		t.Errorf("same-site request: %d, power %v", rec.Code, fc.power)
	}
}

func TestDestructivePowerNeedsConfirmation(t *testing.T) {
	srv, h, fc := newGuardedServer(t)
	for body, want := range map[string]int{
		`{"action":"power_off"}`:                   http.StatusBadRequest,
		`{"action":"power_reset","confirm":false}`: http.StatusBadRequest,
		`{"action":"power_cycle","confirm":true}`:  http.StatusOK,
		`{"action":"power_on"}`:                    http.StatusOK,
	} {
		r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", body, rec.Code, want)
		}
	}
	if len(fc.power) != 2 {
		t.Errorf("power actions reaching the BMC: %v", fc.power)
	}
}

func TestMethodsAreRestricted(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/api/power"},
		{http.MethodGet, "/api/logout"},
		{http.MethodPost, "/api/status"},
		{http.MethodDelete, "/api/connect"},
		{http.MethodPut, "/api/media/images"},
		{http.MethodPost, "/index.html"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, authed(srv, httptest.NewRequest(tt.method, tt.path, nil)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 405", tt.method, tt.path, rec.Code)
		}
	}
}

func TestConcurrentRequestLimit(t *testing.T) {
	srv := newTestServer(bmc.SessionState{})
	block := make(chan struct{})
	entered := make(chan struct{}, maxConcurrentRequests)
	h := srv.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-block
	}))
	for i := 0; i < maxConcurrentRequests; i++ {
		go h.ServeHTTP(httptest.NewRecorder(), authed(srv, httptest.NewRequest(http.MethodGet, "/api/status", nil)))
	}
	for i := 0; i < maxConcurrentRequests; i++ {
		<-entered
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodGet, "/api/status", nil)))
	close(block)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("request over the limit: %d, want 503", rec.Code)
	}
}

func TestStaticAssetsNeedNoSession(t *testing.T) {
	_, h, _ := newGuardedServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status %d: the page must load before it can exchange its launch code", rec.Code)
	}
}

func TestGuardRejects(t *testing.T) {
	srv, h, fc := newGuardedServer(t)

	tests := []struct {
		name   string
		req    func() *http.Request
		status int
	}{
		{"API without session", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/status", nil)
		}, http.StatusUnauthorized},
		{"API with wrong token", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/status", nil)
			r.Header.Set("Authorization", "Bearer nope")
			return r
		}, http.StatusUnauthorized},
		{"API with token in a cookie only", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/status", nil)
			r.AddCookie(&http.Cookie{Name: "ipmi_connect_session", Value: srv.auth.token})
			return r
		}, http.StatusUnauthorized},
		{"DNS rebinding Host", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodGet, "/api/status", nil))
			r.Host = "evil.example:8080"
			return r
		}, http.StatusForbidden},
		{"power as text/plain (no-CORS form post)", func() *http.Request {
			return authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"power_off"}`)))
		}, http.StatusUnsupportedMediaType},
		{"power from another origin", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"power_off"}`)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "https://evil.example")
			return r
		}, http.StatusForbidden},
		{"power from a cross-site fetch", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"power_off"}`)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			return r
		}, http.StatusForbidden},
		{"oversized power body", func() *http.Request {
			r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"`+strings.Repeat("x", 10000)+`"}`)))
			r.Header.Set("Content-Type", "application/json")
			return r
		}, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.req())
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
	if len(fc.power) != 0 {
		t.Errorf("a rejected request reached the BMC: %v", fc.power)
	}
}

func TestGuardAllowsSameOriginPower(t *testing.T) {
	srv, h, fc := newGuardedServer(t)

	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/power", strings.NewReader(`{"action":"power_on"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK || len(fc.power) != 1 || fc.power[0] != bmc.PowerOn {
		t.Errorf("status %d, power %v", rec.Code, fc.power)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, httptest.NewRequest(http.MethodGet, "/", nil)))

	for header, want := range map[string]string{
		"Content-Security-Policy":    "frame-ancestors 'none'",
		"X-Frame-Options":            "DENY",
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
}

func TestVirtualMediaUploadIsGone(t *testing.T) {
	srv, h, _ := newGuardedServer(t)
	r := authed(srv, httptest.NewRequest(http.MethodPost, "/api/vmedia/mount", strings.NewReader("x")))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: the fake upload endpoint wrote files to disk", rec.Code)
	}
}

func TestCSPForbidsInlineCode(t *testing.T) {
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(contentSecurityPolicy, bad) {
			t.Errorf("CSP allows %q: %s", bad, contentSecurityPolicy)
		}
	}
}

// hostForTest is the Host header a request to this server carries.
func (s *Server) hostForTest() string { return "127.0.0.1:" + strconv.Itoa(s.boundPort()) }
