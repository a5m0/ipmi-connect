package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

// fakeMediaConsole is a console with virtual media; it records calls.
type fakeMediaConsole struct {
	fakeConsole
	mu     sync.Mutex
	calls  []string
	failOn string
	images []bmc.MediaImage // added so far, reported by MediaStatus
}

func (f *fakeMediaConsole) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return errors.New("BMC refused")
	}
	return nil
}

func (f *fakeMediaConsole) AddImage(path string, ro bool) (bmc.MediaImage, error) {
	if err := f.record("add " + path); err != nil {
		return bmc.MediaImage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "abc"
	if len(f.images) > 0 {
		id = fmt.Sprintf("id%d", len(f.images))
	}
	im := bmc.MediaImage{ID: id, Path: path, ReadOnly: ro}
	f.images = append(f.images, im)
	return im, nil
}
func (f *fakeMediaConsole) RemoveImage(id string) error { return f.record("remove " + id) }
func (f *fakeMediaConsole) SetImageReadOnly(id string, ro bool) error {
	if ro {
		return f.record("readonly " + id)
	}
	return f.record("writable " + id)
}
func (f *fakeMediaConsole) MapImage(id string, t bmc.MediaTarget) error {
	return f.record("map " + id + " " + string(t))
}
func (f *fakeMediaConsole) UnmapDrive(t bmc.MediaTarget) error { return f.record("unmap " + string(t)) }
func (f *fakeMediaConsole) USBReset() error                    { return f.record("usb-reset") }
func (f *fakeMediaConsole) MediaStatus() bmc.MediaStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bmc.MediaStatus{Images: append([]bmc.MediaImage{}, f.images...), Drives: []bmc.MediaDrive{}}
}

func (f *fakeMediaConsole) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func mediaServer(t *testing.T, console bmc.Console) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(bmc.SessionState{})
	srv.SetConsole(console)
	srv.Media = NewMediaRoots()
	h, err := srv.routes()
	if err != nil {
		t.Fatal(err)
	}
	return srv, h
}

func mediaRequest(srv *Server, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, r))
	return rec
}

// realTemp is a temporary folder with symlinks resolved (macOS's /var is a link).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMediaAPI(t *testing.T) {
	fc := &fakeMediaConsole{}
	srv, h := mediaServer(t, fc)
	dir := realTemp(t)
	srv.Media.AddRoot(dir)
	iso := filepath.Join(dir, "x.iso")
	_ = os.WriteFile(iso, make([]byte, 2048), 0o600)
	steps := []struct {
		method, path, body string
		wantCode           int
		wantCall           string
	}{
		{http.MethodGet, "/api/media", "", 200, ""},
		{http.MethodPost, "/api/media/images", `{"path":"` + iso + `","read_only":true}`, 200, "add " + iso},
		{http.MethodPost, "/api/media/images", `{"path":"` + iso + `"}`, 200, "add " + iso}, // read-only by default
		{http.MethodPost, "/api/media/images", `{"path":"` + iso + `","read_only":false}`, 400, ""},
		{http.MethodPost, "/api/media/images", `{"path":"/etc/passwd"}`, 400, ""},
		{http.MethodPatch, "/api/media/images/abc", `{"read_only":false}`, 400, ""},
		{http.MethodPatch, "/api/media/images/abc", `{"read_only":false,"confirm_writable":true}`, 200, "writable abc"},
		{http.MethodPost, "/api/media/drives/cd", `{"image_id":"abc"}`, 200, "map abc cd"},
		{http.MethodDelete, "/api/media/drives/cd", "", 200, "unmap cd"},
		{http.MethodPost, "/api/media/usb-reset", "", 200, "usb-reset"},
		{http.MethodDelete, "/api/media/images/abc", "", 200, "remove abc"},
		{http.MethodPost, "/api/media/drives/tape", `{"image_id":"abc"}`, 400, ""},
		{http.MethodPost, "/api/media/images", `{}`, 400, ""},
		{http.MethodPost, "/api/media/drives/cd", `{}`, 400, ""},
	}
	for _, st := range steps {
		before := len(fc.calls)
		rec := mediaRequest(srv, h, st.method, st.path, st.body)
		if rec.Code != st.wantCode {
			t.Errorf("%s %s: HTTP %d (%s), want %d", st.method, st.path, rec.Code, strings.TrimSpace(rec.Body.String()), st.wantCode)
			continue
		}
		if st.wantCall != "" && (len(fc.calls) != before+1 || fc.calls[before] != st.wantCall) {
			t.Errorf("%s %s: calls %v, want %q", st.method, st.path, fc.calls[before:], st.wantCall)
		}
		if rec.Code == 200 {
			var status bmc.MediaStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || status.Images == nil {
				t.Errorf("%s %s: body %s is not a media status", st.method, st.path, rec.Body.String())
			}
		}
	}
}

func TestMediaAPIReportsConsoleErrors(t *testing.T) {
	srv, h := mediaServer(t, &fakeMediaConsole{failOn: "map"})
	rec := mediaRequest(srv, h, http.MethodPost, "/api/media/drives/cd", `{"image_id":"abc"}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "BMC refused") {
		t.Errorf("HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestMediaAPIUnsupportedConsole(t *testing.T) {
	srv, h := mediaServer(t, &fakeConsole{})
	if rec := mediaRequest(srv, h, http.MethodGet, "/api/media", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("HTTP %d, want 501 for a console without virtual media", rec.Code)
	}
}

func TestMediaAPIRequiresTokenAndJSON(t *testing.T) {
	srv, h := mediaServer(t, &fakeMediaConsole{})
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/media/images", strings.NewReader(`{"path":"/x.iso"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("HTTP %d, want 401 without the session token", rec.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/media/images", strings.NewReader(`{"path":"/x.iso"}`))
	r.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed(srv, r))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("HTTP %d, want 415 for a non-JSON body", rec.Code)
	}
}

func TestCreateImageAPI(t *testing.T) {
	fc := &fakeMediaConsole{}
	srv, h := mediaServer(t, fc)
	dir := realTemp(t)
	srv.Media.AddRoot(dir)
	blank := dir + "/rescue.img"
	rec := mediaRequest(srv, h, http.MethodPost, "/api/media/create", `{"path":"`+blank+`","size":1474560,"label":"rescue"}`)
	if rec.Code != 200 {
		t.Fatalf("blank: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if st, err := os.Stat(blank); err != nil || st.Size() != 1474560 {
		t.Errorf("blank image: %v", err)
	}
	if fc.calls[len(fc.calls)-1] != "add "+blank {
		t.Errorf("calls %v: the new image was not added to the list", fc.calls)
	}

	src := filepath.Join(dir, "src")
	_ = os.Mkdir(src, 0o700)
	_ = os.WriteFile(src+"/a.txt", []byte("x"), 0o600)
	rec = mediaRequest(srv, h, http.MethodPost, "/api/media/create", `{"path":"`+dir+`/files.img","folder":"`+src+`"}`)
	if rec.Code != 200 {
		t.Fatalf("folder: HTTP %d %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{
		`{"path":"` + dir + `/x.img"}`,                             // neither size nor folder
		`{"path":"` + dir + `/y.img","size":1474560,"folder":"/"}`, // both
	} {
		if rec := mediaRequest(srv, h, http.MethodPost, "/api/media/create", body); rec.Code != 400 {
			t.Errorf("%s: HTTP %d, want 400", body, rec.Code)
		}
	}
	if rec := mediaRequest(srv, h, http.MethodPost, "/api/media/create", `{"path":"`+blank+`","size":1474560}`); rec.Code != 400 ||
		!strings.Contains(rec.Body.String(), "exists") {
		t.Errorf("overwrite: HTTP %d %s", rec.Code, rec.Body.String())
	}
	outside := realTemp(t)
	for _, body := range []string{
		`{"path":"` + outside + `/new.img","size":1474560}`,       // folder outside the roots
		`{"path":"` + dir + `/.hidden.img","size":1474560}`,       // hidden name
		`{"path":"` + dir + `/z.img","folder":"` + outside + `"}`, // source folder outside the roots
		`{"path":"` + dir + `/../escape.img","size":1474560}`,     // traversal
	} {
		if rec := mediaRequest(srv, h, http.MethodPost, "/api/media/create", body); rec.Code != 400 {
			t.Errorf("%s: HTTP %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// Inserting a file into a drive adds it to the list (or reuses its entry) and maps it, in one
// request: the media panel's Insert button.
func TestInsertByPathAddsAndMapsInOneStep(t *testing.T) {
	fc := &fakeMediaConsole{}
	srv, h := mediaServer(t, fc)
	dir := realTemp(t)
	srv.Media.AddRoot(dir)
	iso, img := filepath.Join(dir, "x.iso"), filepath.Join(dir, "d.img")
	_ = os.WriteFile(iso, make([]byte, 2048), 0o600)
	_ = os.WriteFile(img, make([]byte, 1474560), 0o600)

	steps := []struct {
		target, body string
		wantCode     int
		wantCalls    []string
	}{
		{"cd", `{"path":"` + iso + `"}`, 200, []string{"add " + iso, "map abc cd"}},
		{"cd", `{"path":"` + iso + `"}`, 200, []string{"map abc cd"}}, // already listed: reused
		{"removable", `{"path":"` + img + `","read_only":false}`, 400, nil},
		{"removable", `{"path":"` + img + `","read_only":false,"confirm_writable":true}`, 200,
			[]string{"add " + img, "map id1 removable"}},
		{"removable", `{"path":"` + img + `"}`, 200, []string{"readonly id1", "map id1 removable"}},
		{"cd", `{"path":"/etc/passwd"}`, 400, nil},
		{"cd", `{"path":"` + iso + `","image_id":"abc"}`, 400, nil},
	}
	for _, st := range steps {
		before := len(fc.callLog())
		rec := mediaRequest(srv, h, http.MethodPost, "/api/media/drives/"+st.target, st.body)
		if rec.Code != st.wantCode {
			t.Errorf("%s %s: HTTP %d (%s), want %d", st.target, st.body, rec.Code, strings.TrimSpace(rec.Body.String()), st.wantCode)
			continue
		}
		if got := fc.callLog()[before:]; !slices.Equal(got, st.wantCalls) && (len(got) != 0 || st.wantCalls != nil) {
			t.Errorf("%s %s: calls %q, want %q", st.target, st.body, got, st.wantCalls)
		}
	}
}
