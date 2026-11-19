package megarac

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

// testEndpoint reaches addr with cert pinned, as after the user trusted it.
func testEndpoint(t *testing.T, addr string, cert *x509.Certificate) *bmc.Endpoint {
	t.Helper()
	store := trust.NewMemoryStore()
	if err := store.Trust("127.0.0.1", trust.Fingerprint(cert), false); err != nil {
		t.Fatal(err)
	}
	ep, err := bmc.Resolve(context.Background(), addr, &trust.Policy{Store: store, Device: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

const sessionReply = `//Dynamic Data Begin
 WEBVAR_JSONVAR_WEB_SESSION =
 {
 WEBVAR_STRUCTNAME_WEB_SESSION :
 [
 { 'SESSION_COOKIE' : '%s','BMC_IP_ADDR' : '192.0.2.30','CSRFTOKEN' : '%s' },  {} ],
 HAPI_STATUS:0 };
//Dynamic data end
`

// fakeWeb mimics the ASMB8's /rpc pages and JNLP.
type fakeWeb struct {
	mu           sync.Mutex
	password     string
	cookie, csrf string
	power        int
	jnlpHost     string
	logouts      int
	unauthorised int
}

func (f *fakeWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/rpc/WEBSES/create.asp" {
		_ = r.ParseForm()
		if r.PostForm.Get("WEBVAR_USERNAME") != "admin" || r.PostForm.Get("WEBVAR_PASSWORD") != f.password {
			_, _ = fmt.Fprintf(w, sessionReply, "Failure_Login_IPMI_Then_LDAP", "")
			return
		}
		_, _ = fmt.Fprintf(w, sessionReply, f.cookie, f.csrf)
		return
	}
	if r.Header.Get("Cookie") != "SessionCookie="+f.cookie || r.Header.Get("CSRFTOKEN") != f.csrf {
		f.unauthorised++
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/Java/jviewer.jnlp":
		f.jnlpHost = r.URL.Query().Get("EXTRNIP")
		_, _ = io.WriteString(w, `<jnlp spec="1.0+" codebase="https://192.0.2.30:443/Java"><application-desc>`+
			`<argument>-apptype</argument><argument>JViewer</argument>`+
			`<argument>-hostname</argument><argument>192.0.2.30</argument>`+
			`<argument>-kvmtoken</argument><argument>KVMTOKEN0123456</argument>`+
			`<argument>-kvmsecure</argument><argument>1</argument>`+
			`<argument>-kvmport</argument><argument>443</argument>`+
			`<argument>-websecureport</argument><argument>443</argument>`+
			`<argument>-singleportenabled</argument><argument>1</argument>`+
			`<argument>-webcookie</argument><argument>`+f.cookie+`</argument>`+
			`</application-desc></jnlp>`)
	case "/rpc/hoststatus.asp":
		_, _ = fmt.Fprintf(w, "//Dynamic Data Begin\n WEBVAR_JSONVAR_HL_SYSTEM_STATE = { WEBVAR_STRUCTNAME_HL_SYSTEM_STATE : [ { 'JF_STATE' : %d },  {} ],  HAPI_STATUS:0 }; \n", f.power)
	case "/rpc/WEBSES/logout.asp":
		f.logouts++
		_, _ = io.WriteString(w, "//Dynamic Data Begin\n HAPI_STATUS:0 \n")
	default:
		http.NotFound(w, r)
	}
}

func newFakeWeb(t *testing.T, f *fakeWeb, password string) *Web {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return NewWeb(testEndpoint(t, srv.Listener.Addr().String(), srv.Certificate()), "admin", password)
}

func TestWebLoginLaunchPowerLogout(t *testing.T) {
	f := &fakeWeb{password: "pw", cookie: "COOKIE123", csrf: "CSRF456", power: 1}
	w := newFakeWeb(t, f, "pw")
	if err := w.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	l, err := w.Launch("192.0.2.30")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	want := Launch{KVMToken: "KVMTOKEN0123456", WebCookie: "COOKIE123", KVMPort: 443, WebPort: 443, Secure: true, SinglePort: true}
	if l != want {
		t.Errorf("launch %+v, want %+v", l, want)
	}
	if f.jnlpHost != "192.0.2.30" {
		t.Errorf("EXTRNIP %q", f.jnlpHost)
	}
	if st, err := w.PowerStatus(); err != nil || st != bmc.PowerStateOn {
		t.Errorf("PowerStatus = %q, %v", st, err)
	}
	f.mu.Lock()
	f.power = 0
	f.mu.Unlock()
	if st, err := w.PowerStatus(); err != nil || st != bmc.PowerStateOff {
		t.Errorf("PowerStatus = %q, %v; want OFF", st, err)
	}
	w.Logout()
	w.Logout() // a second logout sends nothing
	if f.logouts != 1 || f.unauthorised != 0 {
		t.Errorf("logouts %d, unauthorised requests %d", f.logouts, f.unauthorised)
	}
	if _, err := w.PowerStatus(); err == nil {
		t.Error("want an error after logout")
	}
}

func TestWebLoginRejectsWrongPassword(t *testing.T) {
	w := newFakeWeb(t, &fakeWeb{password: "right", cookie: "c", csrf: "t"}, "wrong")
	err := w.Login()
	if err == nil || !strings.Contains(err.Error(), "check the username and password") {
		t.Fatalf("Login = %v, want a credentials error", err)
	}
}

func TestParseLaunch(t *testing.T) {
	cases := map[string]string{
		"not a jnlp":    `<html>login</html>`,
		"no token":      `<jnlp><argument>-webcookie</argument><argument>c</argument></jnlp>`,
		"bad port":      `<jnlp><argument>-kvmtoken</argument><argument>t</argument><argument>-webcookie</argument><argument>c</argument><argument>-kvmport</argument><argument>99999</argument></jnlp>`,
		"long token":    `<jnlp><argument>-kvmtoken</argument><argument>` + strings.Repeat("t", 200) + `</argument><argument>-webcookie</argument><argument>c</argument></jnlp>`,
		"control chars": `<jnlp><argument>-kvmtoken</argument><argument>t</argument><argument>-webcookie</argument><argument>c&#13;&#10;x</argument></jnlp>`,
	}
	for name, body := range cases {
		if _, err := parseLaunch(body); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	l, err := parseLaunch(`<jnlp><argument>-kvmtoken</argument><argument>t</argument><argument>-webcookie</argument><argument>c</argument></jnlp>`)
	if err != nil || l.KVMPort != 443 || l.WebPort != 443 || l.Secure {
		t.Errorf("defaults %+v, %v", l, err)
	}
}
