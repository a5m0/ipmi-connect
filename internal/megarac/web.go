package megarac

import (
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

const (
	webTimeout = 10 * time.Second
	maxWebBody = 1 << 20
	// maxToken bounds the KVM token and session cookie (16 and 35 characters on the ASMB8).
	maxToken = 128
)

// Web is the BMC's web session: /rpc/*.asp pages authenticated by a SessionCookie cookie and a
// CSRFTOKEN header.
type Web struct {
	host, user, password string
	ep                   *bmc.Endpoint
	http                 *http.Client

	mu           sync.Mutex
	cookie, csrf string
}

// NewWeb prepares a session with the BMC at ep; nothing is sent until Login.
func NewWeb(ep *bmc.Endpoint, user, password string) *Web {
	c := ep.HTTPClient(webTimeout)
	c.Jar = nil // the session cookie is set by hand, as the vendor's page script does
	return &Web{host: ep.Host, ep: ep, user: user, password: password, http: c}
}

var (
	cookieRE = regexp.MustCompile(`'SESSION_COOKIE'\s*:\s*'([^']*)'`)
	csrfRE   = regexp.MustCompile(`'CSRFTOKEN'\s*:\s*'([^']*)'`)
	statusRE = regexp.MustCompile(`HAPI_STATUS\s*:\s*(-?\d+)`)
	powerRE  = regexp.MustCompile(`'JF_STATE'\s*:\s*(\d+)`)
	argRE    = regexp.MustCompile(`<argument>([^<]*)</argument>`)
)

// hapiOK reports whether an /rpc reply ends with HAPI_STATUS 0.
func hapiOK(body string) bool {
	m := statusRE.FindStringSubmatch(body)
	return m != nil && m[1] == "0"
}

func (w *Web) do(method, path string, form url.Values) (string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, w.ep.BaseURL()+path, body)
	if err != nil {
		return "", err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w.mu.Lock()
	cookie, csrf := w.cookie, w.csrf
	w.mu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", "SessionCookie="+cookie)
	}
	if csrf != "" {
		req.Header.Set("CSRFTOKEN", csrf)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return "", bmc.FormatError("MegaRAC "+path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxWebBody))
	if err != nil {
		return "", bmc.FormatError("MegaRAC "+path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("MegaRAC %s: HTTP %d", path, resp.StatusCode)
	}
	return string(data), nil
}

// Login authenticates and keeps the session cookie and CSRF token.
func (w *Web) Login() error {
	w.mu.Lock()
	w.cookie, w.csrf = "", ""
	w.mu.Unlock()
	body, err := w.do(http.MethodPost, "/rpc/WEBSES/create.asp",
		url.Values{"WEBVAR_USERNAME": {w.user}, "WEBVAR_PASSWORD": {w.password}})
	if err != nil {
		return err
	}
	m := cookieRE.FindStringSubmatch(body)
	// A failed login still returns a SESSION_COOKIE, holding a "Failure_..." reason.
	if m == nil || !hapiOK(body) || m[1] == "" || strings.HasPrefix(m[1], "Failure") {
		return errors.New("MegaRAC login failed: check the username and password")
	}
	if !plausibleToken(m[1]) {
		return errors.New("MegaRAC login returned an implausible session cookie")
	}
	csrf := ""
	if c := csrfRE.FindStringSubmatch(body); c != nil {
		csrf = c[1]
	}
	w.mu.Lock()
	w.cookie, w.csrf = m[1], csrf
	w.mu.Unlock()
	log.Printf("[MegaRAC] Authenticated successfully with host %s", w.host)
	return nil
}

// Launch holds the KVM arguments from the JViewer JNLP.
type Launch struct {
	KVMToken   string // one-time KVM session token
	WebCookie  string // the web session cookie, which the tunnel and the session carry
	KVMPort    int    // tunnel port
	WebPort    int    // web port named in the tunnel request
	Secure     bool   // tunnel over TLS
	SinglePort bool   // KVM tunnelled through the web port
}

// plausibleToken accepts the short printable tokens these BMCs issue.
func plausibleToken(s string) bool {
	if s == "" || len(s) > maxToken {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

func parseLaunch(body string) (Launch, error) {
	if !strings.Contains(body, "<jnlp") {
		return Launch{}, errors.New("BMC did not return a JNLP (session expired?)")
	}
	args := map[string]string{}
	var prev string
	for _, m := range argRE.FindAllStringSubmatch(body, -1) {
		a := html.UnescapeString(strings.TrimSpace(m[1]))
		if strings.HasPrefix(prev, "-") {
			args[prev] = a
		}
		prev = a
	}
	l := Launch{KVMToken: args["-kvmtoken"], WebCookie: args["-webcookie"], KVMPort: 443, WebPort: 443,
		Secure: args["-kvmsecure"] == "1", SinglePort: args["-singleportenabled"] == "1"}
	if !plausibleToken(l.KVMToken) || !plausibleToken(l.WebCookie) {
		return Launch{}, errors.New("JNLP has no plausible KVM token and session cookie")
	}
	for name, dst := range map[string]*int{"-kvmport": &l.KVMPort, "-websecureport": &l.WebPort} {
		v, ok := args[name]
		if !ok {
			continue
		}
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return Launch{}, fmt.Errorf("JNLP %s %q is not a port", name, v)
		}
		*dst = p
	}
	return l, nil
}

// Launch fetches the JViewer JNLP for a new KVM session. host is the BMC address as the user
// gave it.
func (w *Web) Launch(host string) (Launch, error) {
	q := url.Values{"EXTRNIP": {host}, "JNLPSTR": {"JViewer"}}
	body, err := w.do(http.MethodGet, "/Java/jviewer.jnlp?"+q.Encode(), nil)
	if err != nil {
		return Launch{}, fmt.Errorf("MegaRAC KVM launch: %w", err)
	}
	return parseLaunch(body)
}

// PowerStatus returns bmc.PowerStateOn or bmc.PowerStateOff.
func (w *Web) PowerStatus() (string, error) {
	body, err := w.do(http.MethodGet, "/rpc/hoststatus.asp", nil)
	if err != nil {
		return "", err
	}
	m := powerRE.FindStringSubmatch(body)
	if m == nil || !hapiOK(body) {
		return "", errors.New("MegaRAC power status: unexpected reply")
	}
	if m[1] == "0" {
		return bmc.PowerStateOff, nil
	}
	return bmc.PowerStateOn, nil
}

// Logout ends the web session, which also ends its KVM session.
func (w *Web) Logout() {
	w.mu.Lock()
	cookie := w.cookie
	w.mu.Unlock()
	if cookie == "" {
		return
	}
	if _, err := w.do(http.MethodGet, "/rpc/WEBSES/logout.asp", nil); err != nil {
		log.Printf("[MegaRAC] Logout: %v", err)
	}
	w.mu.Lock()
	w.cookie, w.csrf = "", ""
	w.mu.Unlock()
	log.Printf("[MegaRAC] Logged out of %s", w.host)
}
