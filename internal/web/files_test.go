package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type listing struct {
	Path    string `json:"path"`
	Parent  string `json:"parent"`
	Entries []struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir"`
		Size int64  `json:"size"`
	} `json:"entries"`
}

func list(t *testing.T, srv *Server, h http.Handler, query string) (int, listing) {
	t.Helper()
	rec := mediaRequest(srv, h, http.MethodGet, "/api/files?"+query, "")
	var l listing
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatalf("body %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, l
}

func TestFileBrowser(t *testing.T) {
	srv, h := mediaServer(t, &fakeMediaConsole{})
	dir := realTemp(t)
	srv.Media.AddRoot(dir)
	outside := realTemp(t)
	_ = os.WriteFile(filepath.Join(outside, "secret.iso"), make([]byte, 10), 0o600)
	_ = os.Symlink(filepath.Join(outside, "secret.iso"), filepath.Join(dir, "escape.iso"))
	_ = os.Symlink(outside, filepath.Join(dir, "escape-dir"))
	for _, name := range []string{"b.iso", "a.IMG", "notes.txt", ".hidden.iso"} {
		_ = os.WriteFile(filepath.Join(dir, name), make([]byte, 10), 0o600)
	}
	for _, name := range []string{"Zeta", "alpha", ".git"} {
		_ = os.Mkdir(filepath.Join(dir, name), 0o700)
	}
	_ = os.Symlink(filepath.Join(dir, "b.iso"), filepath.Join(dir, "link.iso"))

	code, l := list(t, srv, h, "path="+url.QueryEscape(dir)+"&mode=file")
	if code != 200 {
		t.Fatalf("HTTP %d", code)
	}
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.Name)
	}
	// Folders first, then images, each sorted case-insensitively; no hidden entries, no other files.
	want := []string{"alpha", "Zeta", "a.IMG", "b.iso", "link.iso"}
	if len(got) != len(want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries %v, want %v", got, want)
		}
	}
	// The root has no parent to go up to; links out of the roots are not listed.
	if l.Path != dir || l.Parent != "" || !l.Entries[0].Dir || l.Entries[3].Size != 10 {
		t.Errorf("listing %+v", l)
	}
	if code, l := list(t, srv, h, "path="+url.QueryEscape(filepath.Join(dir, "alpha"))+"&mode=file"); code != 200 || l.Parent != dir {
		t.Errorf("subfolder: HTTP %d parent %q, want %q", code, l.Parent, dir)
	}

	if _, l := list(t, srv, h, "path="+url.QueryEscape(dir)+"&mode=dir"); len(l.Entries) != 2 {
		t.Errorf("dir mode lists %d entries, want only the 2 folders", len(l.Entries))
	}
	if code, l := list(t, srv, h, "mode=file"); code != 200 || l.Path != dir {
		t.Errorf("default listing: HTTP %d path %q, want the first root", code, l.Path)
	}
	for _, q := range []string{
		"path=relative&mode=file",
		"path=" + url.QueryEscape(filepath.Join(dir, "b.iso")),
		"path=" + url.QueryEscape(filepath.Join(dir, "missing")),
		"path=" + url.QueryEscape(outside),                          // outside the roots
		"path=" + url.QueryEscape(filepath.Join(dir, "escape-dir")), // a link out of the roots
		"path=" + url.QueryEscape(filepath.Join(dir, ".git")),       // hidden
		"path=" + url.QueryEscape(filepath.Join(dir, "alpha", "..", "..")),
		"path=%2Fetc",
	} {
		if code, _ := list(t, srv, h, q); code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", q, code)
		}
	}
}

func TestFileBrowserRequiresToken(t *testing.T) {
	_, h := mediaServer(t, &fakeMediaConsole{})
	r, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/files?mode=file", nil)
	r.Host = "127.0.0.1:8080"
	rec := newRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("HTTP %d, want 401", rec.Code)
	}
}
