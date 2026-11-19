package supermicro

import (
	"errors"
	"fmt"
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

// MegaRACWeb is the web session of BMCs that pair AMI MegaRAC's classic web interface with the
// ATEN iKVM (seen on the ASUS ASMB8-iKVM): login, the KVM ticket, power and logout go through
// /rpc/*.asp pages authenticated by a SessionCookie cookie and a CSRFTOKEN header, while the KVM
// and virtual media are ATEN's, as on Supermicro.
type MegaRACWeb struct {
	host, user, password string
	ep                   *bmc.Endpoint
	http                 *http.Client
	ticketRetryDelay     time.Duration

	mu           sync.Mutex
	cookie, csrf string
}

// NewMegaRACWeb prepares a session with the BMC at ep; nothing is sent until Login.
func NewMegaRACWeb(ep *bmc.Endpoint, user, password string) *MegaRACWeb {
	c := ep.HTTPClient(webTimeout)
	c.Jar = nil // the session cookie is set by hand, as the vendor's page script does
	return &MegaRACWeb{host: ep.Host, ep: ep, user: user, password: password, http: c, ticketRetryDelay: time.Second}
}

// Flavor names the BMC for logs and the console's model line.
func (w *MegaRACWeb) Flavor() string { return "ATEN iKVM with AMI MegaRAC web (ASUS)" }

var (
	megaracCookieRE = regexp.MustCompile(`'SESSION_COOKIE'\s*:\s*'([^']*)'`)
	megaracCSRFRE   = regexp.MustCompile(`'CSRFTOKEN'\s*:\s*'([^']*)'`)
	megaracStatusRE = regexp.MustCompile(`HAPI_STATUS\s*:\s*(-?\d+)`)
	megaracPowerRE  = regexp.MustCompile(`'JF_STATE'\s*:\s*(\d+)`)
)

// hapiStatus returns the HAPI_STATUS of an /rpc reply, which is 0 on success.
func hapiStatus(body string) (int, bool) {
	m := megaracStatusRE.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

func (w *MegaRACWeb) do(method, path string, form url.Values) (string, error) {
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
func (w *MegaRACWeb) Login() error {
	w.mu.Lock()
	w.cookie, w.csrf = "", ""
	w.mu.Unlock()
	body, err := w.do(http.MethodPost, "/rpc/WEBSES/create.asp",
		url.Values{"WEBVAR_USERNAME": {w.user}, "WEBVAR_PASSWORD": {w.password}})
	if err != nil {
		return err
	}
	m := megaracCookieRE.FindStringSubmatch(body)
	status, ok := hapiStatus(body)
	// A failed login still returns a SESSION_COOKIE, holding a "Failure_..." reason.
	if m == nil || !ok || status != 0 || m[1] == "" || strings.HasPrefix(m[1], "Failure") {
		return errors.New("MegaRAC login failed: check the username and password")
	}
	csrf := ""
	if c := megaracCSRFRE.FindStringSubmatch(body); c != nil {
		csrf = c[1]
	}
	w.mu.Lock()
	w.cookie, w.csrf = m[1], csrf
	w.mu.Unlock()
	log.Printf("[Supermicro] Authenticated successfully with host %s (%s)", w.host, w.Flavor())
	return nil
}

// FetchTicket gets one-time KVM credentials from the JViewer JNLP, which on these BMCs launches
// the ATEN viewer. The session may not be ready right after login, so it retries once.
func (w *MegaRACWeb) FetchTicket(host string) (Ticket, error) {
	q := url.Values{"EXTRNIP": {host}, "JNLPSTR": {"JViewer"}}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(w.ticketRetryDelay)
		}
		body, err := w.do(http.MethodGet, "/Java/jviewer.jnlp?"+q.Encode(), nil)
		if err == nil {
			var t Ticket
			if t, err = parseTicket(body, host); err == nil {
				return t, nil
			}
		}
		lastErr = err
	}
	return Ticket{}, fmt.Errorf("MegaRAC KVM ticket: %w", lastErr)
}

// PowerCGI issues a power action through /rpc/hostctl.asp. Its codes follow the IPMI Chassis
// Control command, like ATEN's ipmi.cgi.
func (w *MegaRACWeb) PowerCGI(action bmc.PowerAction) error {
	codes := map[bmc.PowerAction]int{
		bmc.PowerOff: 0, bmc.PowerOn: 1, bmc.PowerCycle: 2, bmc.PowerReset: 3, bmc.PowerSoftOff: 5,
	}
	code, ok := codes[action]
	if !ok {
		return fmt.Errorf("unknown power action: %s", action)
	}
	body, err := w.do(http.MethodPost, "/rpc/hostctl.asp", url.Values{"WEBVAR_POWER_CMD": {strconv.Itoa(code)}})
	if err != nil {
		return err
	}
	if status, ok := hapiStatus(body); !ok || status != 0 {
		return errors.New("MegaRAC power: unexpected reply (web session expired?)")
	}
	log.Printf("[Supermicro] Power %q sent", action)
	return nil
}

// PowerStatus returns "ON" or "OFF" from /rpc/hoststatus.asp.
func (w *MegaRACWeb) PowerStatus() (string, error) {
	body, err := w.do(http.MethodGet, "/rpc/hoststatus.asp", nil)
	if err != nil {
		return "", err
	}
	m := megaracPowerRE.FindStringSubmatch(body)
	if status, ok := hapiStatus(body); m == nil || !ok || status != 0 {
		return "", errors.New("MegaRAC power status: unexpected reply")
	}
	if m[1] == "0" {
		return "OFF", nil
	}
	return "ON", nil
}

// Logout ends the web session. It also ends any KVM session, so call it after the socket closes.
func (w *MegaRACWeb) Logout() {
	w.mu.Lock()
	cookie := w.cookie
	w.mu.Unlock()
	if cookie == "" {
		return
	}
	if _, err := w.do(http.MethodGet, "/rpc/WEBSES/logout.asp", nil); err != nil {
		log.Printf("[Supermicro] Logout: %v", err)
		return
	}
	w.mu.Lock()
	w.cookie, w.csrf = "", ""
	w.mu.Unlock()
	log.Printf("[Supermicro] Successfully logged out session from %s", w.host)
}
