package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxListing bounds one folder listing.
const maxListing = 5000

type fileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// MediaRoots confines virtual media to places the user chose: a set of folders (by default
// the home folder and the application's private media folder, plus any given with
// --media-dir) and individual files picked in the desktop's file chooser. Paths are resolved
// through symlinks before the check, so a link inside a root cannot reach outside it, and hidden
// entries below a root (~/.ssh, ~/.config, ...) are never offered or accepted.
type MediaRoots struct {
	mu    sync.Mutex
	roots []string        // resolved, cleaned absolute folders
	files map[string]bool // resolved files picked explicitly
}

// NewMediaRoots returns roots for the given folders; ones that do not exist are skipped.
func NewMediaRoots(dirs ...string) *MediaRoots {
	m := &MediaRoots{files: map[string]bool{}}
	for _, d := range dirs {
		m.AddRoot(d)
	}
	return m
}

// AddRoot allows a folder and everything (not hidden) below it.
func (m *MediaRoots) AddRoot(dir string) {
	if dir == "" || !filepath.IsAbs(dir) {
		return
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.roots {
		if r == real {
			return
		}
	}
	m.roots = append(m.roots, real)
}

// AddFile allows one file the user picked explicitly, returning its resolved path.
func (m *MediaRoots) AddFile(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[real] = true
	return real, nil
}

// Roots lists the allowed folders.
func (m *MediaRoots) Roots() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.roots...)
}

var errOutsideRoots = errors.New("that location is outside the folders ipmi-connect may use for virtual media")

// Resolve returns the real path of an existing file or folder, if it is allowed.
func (m *MediaRoots) Resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("give a full path")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("cannot open %s", filepath.Base(path))
	}
	contained, ok := m.contain(real)
	if !ok {
		return "", errOutsideRoots
	}
	return contained, nil
}

// ResolveNew returns the real path for a file to be created, if its folder is allowed and its
// name is not hidden. The file must not exist yet.
func (m *MediaRoots) ResolveNew(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("give a full path")
	}
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if !filepath.IsLocal(name) {
		return "", errors.New("give a file name")
	}
	if strings.HasPrefix(name, ".") {
		return "", errors.New("the file name must not start with a dot")
	}
	dir, err := m.Resolve(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, name)
	if _, err := os.Lstat(full); err == nil {
		return "", fmt.Errorf("%s already exists", name)
	}
	return full, nil
}

// contain checks a resolved path against the roots and returns it rebuilt from trusted parts:
// a picked file is returned as the copy stored when it was picked, and a path inside a root as
// that root joined with a local relative path (no "..", not absolute, nothing hidden). Callers
// open what contain returns, never the path they were given.
func (m *MediaRoots) contain(real string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for picked := range m.files {
		if picked == real {
			return picked, true
		}
	}
	for _, r := range m.roots {
		rel, err := filepath.Rel(r, real)
		if err != nil {
			continue
		}
		if rel == "." {
			return r, true
		}
		if !filepath.IsLocal(rel) {
			continue // outside the root
		}
		if hasHiddenComponent(rel) {
			continue
		}
		return filepath.Join(r, rel), true
	}
	return "", false
}

func hasHiddenComponent(rel string) bool {
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func (m *MediaRoots) isRoot(real string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.roots {
		if r == real {
			return true
		}
	}
	return false
}

// handleFiles lists a folder for the image picker: subfolders and, in "file" mode, .iso and
// .img files, within the media roots. Browsers never reveal a picked file's real path, and
// copying images through the browser would break writes to .img files, so the server browses
// for them. Hidden entries and links leading outside the roots are left out; file contents are
// never read.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	roots := s.Media
	if roots == nil || len(roots.Roots()) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "parent": "", "entries": []fileEntry{}, "roots": []string{}})
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = roots.Roots()[0]
	}
	real, err := roots.Resolve(path)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	dirents, err := os.ReadDir(real)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, badRequest("cannot open that folder"))
		return
	}
	files := r.URL.Query().Get("mode") == "file"

	entries := []fileEntry{}
	for _, d := range dirents {
		if strings.HasPrefix(d.Name(), ".") || len(entries) >= maxListing {
			continue
		}
		full := filepath.Join(real, d.Name())
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue
		}
		target, ok := roots.contain(resolved)
		if !ok {
			continue // a link out of the roots
		}
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch {
		case info.IsDir():
			entries = append(entries, fileEntry{Name: d.Name(), Dir: true})
		case files && info.Mode().IsRegular() && (ext == ".iso" || ext == ".img"):
			entries = append(entries, fileEntry{Name: d.Name(), Size: info.Size()})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := ""
	if !roots.isRoot(real) {
		if p, err := roots.Resolve(filepath.Dir(real)); err == nil {
			parent = p
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": real, "parent": parent, "entries": entries, "roots": roots.Roots()})
}
