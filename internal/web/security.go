package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxJSONBody bounds API request bodies; every API payload is a few hundred bytes of JSON.
const maxJSONBody = 4096

const (
	// defaultLaunchTTL is how long a launch link works if nobody opens it.
	defaultLaunchTTL = 2 * time.Minute
	// wsTicketTTL is how long a WebSocket ticket works; the page uses it at once.
	wsTicketTTL  = 30 * time.Second
	maxWSTickets = 16
	// maxConcurrentRequests bounds simultaneous HTTP requests (WebSockets are counted separately).
	maxConcurrentRequests = 32
)

// contentSecurityPolicy allows only this origin's own scripts, styles and connections, no
// inline code or styles, no plugins, and forbids framing (clickjacking the power buttons).
// 'self' in connect-src covers the same-origin WebSocket.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data: blob:; connect-src 'self'; font-src 'none'; object-src 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// newToken returns a random 256-bit secret.
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// auth holds the local console's credentials.
//
//   - The launch link carries a one-time code in its URL fragment (never sent to a server). It
//     expires after a short time and works once.
//   - The page exchanges it for the session token, which it keeps in memory and sessionStorage
//     (per tab, cleared when the tab closes; never localStorage or a cookie: browsers share
//     127.0.0.1 cookies with every other local port and service). The token goes only in an
//     Authorization header.
//   - WebSockets cannot carry headers, so the page trades its token for a single-use ticket that
//     expires in seconds, and puts that in the socket URL instead of the token.
//   - Logout replaces the token, voids tickets and the launch code, and closes every socket.
type auth struct {
	mu            sync.Mutex
	token         string
	launchCode    string
	launchExpires time.Time
	tickets       map[string]time.Time
	now           func() time.Time
}

func newAuth() *auth {
	return &auth{token: newToken(), tickets: map[string]time.Time{}, now: time.Now}
}

// newLaunchCode replaces any previous launch code with a fresh one valid for ttl.
func (a *auth) newLaunchCode(ttl time.Duration) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.launchCode = newToken()
	a.launchExpires = a.now().Add(ttl)
	return a.launchCode
}

// exchange trades a launch code for the session token, once.
func (a *auth) exchange(code string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.launchCode == "" || subtle.ConstantTimeCompare([]byte(code), []byte(a.launchCode)) != 1 {
		return "", errLaunch("this console link has already been used or is not valid")
	}
	a.launchCode = "" // single use, whatever happens next
	if a.now().After(a.launchExpires) {
		return "", errLaunch("this console link has expired")
	}
	return a.token, nil
}

type errLaunch string

func (e errLaunch) Error() string { return string(e) }

func (a *auth) tokenMatches(candidate string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return candidate != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
}

// issueTicket returns a single-use WebSocket ticket.
func (a *auth) issueTicket() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for t, exp := range a.tickets {
		if now.After(exp) {
			delete(a.tickets, t)
		}
	}
	if len(a.tickets) >= maxWSTickets {
		for t := range a.tickets { // drop an arbitrary one: tickets are only outstanding for seconds
			delete(a.tickets, t)
			break
		}
	}
	t := newToken()
	a.tickets[t] = now.Add(wsTicketTTL)
	return t
}

// consumeTicket reports whether t is an unexpired ticket, and voids it.
func (a *auth) consumeTicket(t string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tickets[t]
	if !ok {
		return false
	}
	delete(a.tickets, t)
	return !a.now().After(exp)
}

// logout replaces the token and voids every ticket and the launch code.
func (a *auth) logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = newToken()
	a.tickets = map[string]time.Time{}
	a.launchCode = ""
}

// guard protects the local console.
//
//   - Host must be exactly this server's origin (127.0.0.1, localhost or [::1] with the bound
//     port), defeating DNS rebinding.
//   - Requests that name an Origin must name this origin; the API and WebSocket refuse requests
//     the browser marks as coming from another site or origin (Sec-Fetch-Site). Another port on
//     127.0.0.1 is same-site but not same-origin, so it is refused too.
//   - The API needs the session token in an Authorization header; the WebSocket needs a ticket.
//   - Static assets carry no secrets and are served to anyone who passes the Host check: the page
//     has to load before it can exchange its code.
func (s *Server) guard(next http.Handler) http.Handler {
	slots := make(chan struct{}, maxConcurrentRequests)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")

		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		api := r.URL.Path == "/ws" || strings.HasPrefix(r.URL.Path, "/api/")
		if api {
			h.Set("Cache-Control", "no-store")
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		if r.URL.Path == "/ws" {
			// Sockets are long-lived and limited by maxClients instead of request slots.
			if !s.auth.consumeTicket(r.URL.Query().Get("ticket")) {
				http.Error(w, "not signed in", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "too many requests", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/api/session" {
			s.handleSession(w, r)
			return
		}
		if api && !s.authorized(r) {
			http.Error(w, "not signed in: open the console from the app or the link it printed", http.StatusUnauthorized)
			return
		}
		if api {
			s.touch()
		}
		next.ServeHTTP(w, r)
	})
}

// handleSession exchanges the one-time launch code for the session token.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token, err := s.auth.exchange(req.Code)
	if err != nil {
		http.Error(w, err.Error()+": open the console again from the app", http.StatusUnauthorized)
		return
	}
	s.touch()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// LaunchURL returns a fresh link that signs one browser in. It replaces any earlier link. The
// code sits in the fragment, which browsers never send to a server, and the page removes it
// from the address bar at once.
func (s *Server) LaunchURL() string {
	ttl := s.LaunchTTL
	if ttl <= 0 {
		ttl = defaultLaunchTTL
	}
	code := s.auth.newLaunchCode(ttl)
	return fmt.Sprintf("http://%s:%d/#c=%s", s.urlHost(), s.boundPort(), code)
}

func (s *Server) urlHost() string {
	if s.URLHost != "" {
		return s.URLHost
	}
	return "127.0.0.1"
}

func (s *Server) authorized(r *http.Request) bool {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.auth.tokenMatches(bearer)
}

// hostAllowed reports whether a Host header names exactly this server.
func (s *Server) hostAllowed(hostport string) bool {
	port := s.boundPort()
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if hostport == fmt.Sprintf("%s:%d", h, port) {
			return true
		}
	}
	return false
}

// originAllowed reports whether an Origin header is exactly this server's origin.
func (s *Server) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.User != nil {
		return false
	}
	return s.hostAllowed(u.Host)
}

// requireJSON rejects requests that are not application/json, which a cross-site page cannot send
// without a CORS preflight, and caps the body size.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	return true
}
