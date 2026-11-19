package supermicro

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

// fakeMegaRACWeb mimics the ASUS ASMB8-iKVM: AMI MegaRAC /rpc pages handing out ATEN tickets.
// It has no /cgi/login.cgi.
type fakeMegaRACWeb struct {
	mu            sync.Mutex
	password      string
	cookie, csrf  string
	power         []string
	logouts       int
	badCSRF       int
	jnlpHost      string
	powerState    int
	cgiLoginPosts int
}

const megaracReply = `//Dynamic Data Begin
 WEBVAR_JSONVAR_WEB_SESSION =
 {
 WEBVAR_STRUCTNAME_WEB_SESSION :
 [
 { 'SESSION_COOKIE' : '%s','BMC_IP_ADDR' : '192.0.2.30','CSRFTOKEN' : '%s' },  {} ],
 HAPI_STATUS:%d };
//Dynamic data end
`

func (f *fakeMegaRACWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/cgi/login.cgi" {
		f.cgiLoginPosts++
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/rpc/WEBSES/create.asp" {
		_ = r.ParseForm()
		if r.PostForm.Get("WEBVAR_USERNAME") != "admin" || r.PostForm.Get("WEBVAR_PASSWORD") != f.password {
			_, _ = fmt.Fprintf(w, megaracReply, "Failure_Login_IPMI_Then_LDAP", "", 0)
			return
		}
		_, _ = fmt.Fprintf(w, megaracReply, f.cookie, f.csrf, 0)
		return
	}
	if r.Header.Get("Cookie") != "SessionCookie="+f.cookie {
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("CSRFTOKEN") != f.csrf {
		f.badCSRF++
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/Java/jviewer.jnlp":
		f.jnlpHost = r.URL.Query().Get("EXTRNIP")
		if r.URL.Query().Get("JNLPSTR") != "JViewer" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<jnlp spec="1.0+"><application-desc main-class="tw.com.aten.ikvm.KVMMain">`+
			`<argument>`+f.jnlpHost+`</argument><argument>kvmtoken</argument><argument>kvmtoken</argument>`+
			`<argument>null</argument><argument>5900</argument><argument>623</argument>`+
			`<argument>2</argument><argument>0</argument></application-desc></jnlp>`)
	case "/rpc/hoststatus.asp":
		_, _ = fmt.Fprintf(w, "//Dynamic Data Begin\n WEBVAR_JSONVAR_HL_GETHOSTSTATUS = { WEBVAR_STRUCTNAME_HL_GETHOSTSTATUS : [ { 'JF_STATE' : %d },  {} ],  HAPI_STATUS:0 }; \n", f.powerState)
	case "/rpc/hostctl.asp":
		_ = r.ParseForm()
		f.power = append(f.power, r.PostForm.Get("WEBVAR_POWER_CMD"))
		_, _ = io.WriteString(w, "//Dynamic Data Begin\n WEBVAR_JSONVAR_HL_HOSTCTL = { WEBVAR_STRUCTNAME_HL_HOSTCTL : [ {} ],  HAPI_STATUS:0 }; \n")
	case "/rpc/WEBSES/logout.asp":
		f.logouts++
		_, _ = io.WriteString(w, "//Dynamic Data Begin\n HAPI_STATUS:0 \n")
	default:
		http.NotFound(w, r)
	}
}

func newFakeMegaRACClient(t *testing.T, f *fakeMegaRACWeb, password string) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return NewClient(testEndpoint(t, srv.Listener.Addr().String(), srv.Certificate()), "admin", password)
}

func TestMegaRACFallbackLoginTicketPowerLogout(t *testing.T) {
	f := &fakeMegaRACWeb{password: "pw", cookie: "SESSIONabc123", csrf: "CSRFxyz", powerState: 1}
	c := newFakeMegaRACClient(t, f, "pw")

	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	mr, ok := c.web.(*MegaRACWeb)
	if !ok {
		t.Fatalf("web session is %T, want *MegaRACWeb after the ATEN login page 404s", c.web)
	}
	mr.ticketRetryDelay = 0
	if !strings.Contains(c.Flavor(), "MegaRAC") {
		t.Errorf("Flavor = %q", c.Flavor())
	}

	tk, err := c.web.FetchTicket("127.0.0.1")
	if err != nil {
		t.Fatalf("FetchTicket: %v", err)
	}
	if tk.User != "kvmtoken" || tk.Password != "kvmtoken" || tk.Port != 5900 || tk.TLS {
		t.Errorf("ticket %+v", tk)
	}
	if f.jnlpHost != "127.0.0.1" {
		t.Errorf("EXTRNIP = %q", f.jnlpHost)
	}

	if st, err := c.PowerStatus(); err != nil || st != "ON" {
		t.Errorf("PowerStatus = %q, %v", st, err)
	}
	f.mu.Lock()
	f.powerState = 0
	f.mu.Unlock()
	if st, err := c.PowerStatus(); err != nil || st != "OFF" {
		t.Errorf("PowerStatus = %q, %v; want OFF", st, err)
	}
	// No KVM session is up, so this goes through the web page.
	if err := c.PowerControl(bmc.PowerCycle); err != nil {
		t.Fatalf("PowerControl: %v", err)
	}
	c.Logout()

	if len(f.power) != 1 || f.power[0] != "2" {
		t.Errorf("power commands %v, want power cycle 2", f.power)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d", f.logouts)
	}
	if f.badCSRF != 0 {
		t.Errorf("%d requests without the CSRF token", f.badCSRF)
	}
	if f.cgiLoginPosts != 1 {
		t.Errorf("ATEN login tried %d times, want once", f.cgiLoginPosts)
	}
}

func TestMegaRACRejectsWrongPassword(t *testing.T) {
	c := newFakeMegaRACClient(t, &fakeMegaRACWeb{password: "right", cookie: "c", csrf: "t"}, "wrong")
	err := c.Login()
	if err == nil {
		t.Fatal("want error for a Failure_ session cookie")
	}
	if !strings.Contains(err.Error(), "check the username and password") {
		t.Errorf("error %q should point at the credentials", err)
	}
}

// A wrong password on real ATEN firmware must not be retried against the MegaRAC page.
func TestATENWrongPasswordDoesNotFallBack(t *testing.T) {
	f := &fakeBMCWeb{password: "other", sid: "x"}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c := NewClient(testEndpoint(t, srv.Listener.Addr().String(), srv.Certificate()), "ADMIN", "pw")
	if err := c.Login(); err == nil || strings.Contains(err.Error(), "MegaRAC") {
		t.Fatalf("Login = %v; want the ATEN credentials error alone", err)
	}
	if _, ok := c.web.(*WebSession); !ok {
		t.Errorf("web session is %T, want *WebSession", c.web)
	}
}
