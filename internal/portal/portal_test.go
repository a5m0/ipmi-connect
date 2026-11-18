package portal

import "testing"

func TestFileURIPath(t *testing.T) {
	good := map[string]string{
		"file:///home/u/x.iso":                     "/home/u/x.iso",
		"file:///run/user/1000/doc/ab12/My%20.iso": "/run/user/1000/doc/ab12/My .iso",
		"file://localhost/srv/a.img":               "/srv/a.img",
	}
	for in, want := range good {
		if got, err := fileURIPath(in); err != nil || got != want {
			t.Errorf("fileURIPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://x/a.iso", "file://evil.example/a.iso", "relative.iso", "file:relative"} {
		if _, err := fileURIPath(bad); err == nil {
			t.Errorf("fileURIPath(%q) accepted", bad)
		}
	}
}

func TestHandleTokenIsValidBusPathElement(t *testing.T) {
	tok := newHandleToken()
	for _, c := range tok {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
		if !ok {
			t.Fatalf("token %q has %q, not allowed in an object path element", tok, c)
		}
	}
}
