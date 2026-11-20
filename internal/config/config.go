// Package config locates the application's private files and stores device profiles.
//
// Linux follows the XDG base directories ($XDG_CONFIG_HOME, $XDG_DATA_HOME, which Flatpak points
// inside the sandbox's ~/.var/app/<id>); macOS uses ~/Library/Application Support. Files are
// created readable only by the user. Passwords are never stored: Profile has no field for one.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

const appDir = "ipmi-connect"

// Dir is the configuration directory (profiles, trusted certificates).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDir), nil
}

// DataDir is the data directory; virtual media images may live in its "media" folder.
func DataDir() (string, error) {
	if runtime.GOOS == "linux" || runtime.GOOS == "freebsd" || runtime.GOOS == "openbsd" {
		if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
			return filepath.Join(d, appDir), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", appDir), nil
	}
	return Dir() // macOS and others: alongside the configuration
}

// MediaDir is the application-private folder for virtual media images, created on demand.
func MediaDir() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	d = filepath.Join(d, "media")
	return d, os.MkdirAll(d, 0o700)
}

// TrustPath is the trusted-certificate store.
func TrustPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "known_hosts.json"), nil
}

// ProfilesPath is the device profile store.
func ProfilesPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "profiles.json"), nil
}

// Profile is a saved device. It deliberately has no password.
type Profile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	User    string `json:"user"`
	Vendor  string `json:"vendor"` // auto, dell, supermicro or megarac
	// Explicit legacy preferences: pre-select the consent boxes for this device. Consent itself
	// is recorded in the trust store when the user connects.
	LegacyTLS        bool `json:"legacy_tls,omitempty"`
	InsecureChannels bool `json:"insecure_channels,omitempty"`
}

// Recent is a recently used device (no password).
type Recent struct {
	Address string    `json:"address"`
	User    string    `json:"user"`
	Vendor  string    `json:"vendor"`
	At      time.Time `json:"at"`
}

const (
	maxRecents   = 10
	maxProfiles  = 500
	maxFieldLen  = 256
	maxStoreSize = 1 << 20
)

// Store holds profiles and recents, persisted as JSON when it has a path.
type Store struct {
	path string

	mu       sync.Mutex
	profiles []Profile
	recents  []Recent
}

type storeFile struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
	Recents  []Recent  `json:"recents"`
}

// NewMemoryStore returns an unpersisted store.
func NewMemoryStore() *Store { return &Store{} }

// Open loads the store at path; a missing file is empty.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxStoreSize {
		return nil, fmt.Errorf("%s is too large", path)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	for _, p := range f.Profiles {
		if validate(&p) == nil && len(s.profiles) < maxProfiles {
			s.profiles = append(s.profiles, p)
		}
	}
	for _, r := range f.Recents {
		if _, _, err := bmc.ParseAddress(r.Address); err == nil && len(s.recents) < maxRecents {
			s.recents = append(s.recents, r)
		}
	}
	return s, nil
}

// Profiles returns the saved profiles.
func (s *Store) Profiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Profile{}, s.profiles...)
}

// Recents returns recently used devices, newest first.
func (s *Store) Recents() []Recent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recent{}, s.recents...)
}

func validate(p *Profile) error {
	if _, _, err := bmc.ParseAddress(p.Address); err != nil {
		return err
	}
	p.Name, p.User = strings.TrimSpace(p.Name), strings.TrimSpace(p.User)
	if p.Name == "" {
		p.Name = p.Address
	}
	for _, f := range []string{p.Name, p.User, p.Address} {
		if len(f) > maxFieldLen || !utf8.ValidString(f) || strings.ContainsAny(f, "\x00\r\n") {
			return errors.New("profile fields must be short single-line text")
		}
	}
	switch p.Vendor {
	case "", "auto":
		p.Vendor = "auto"
	case string(bmc.TargetDell), string(bmc.TargetSupermicro), string(bmc.TargetMegaRAC):
	default:
		return fmt.Errorf("unknown vendor %q", p.Vendor)
	}
	return nil
}

// Save adds or updates a profile (a new one gets an ID) and returns it.
func (s *Store) Save(p Profile) (Profile, error) {
	if err := validate(&p); err != nil {
		return Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := append([]Profile{}, s.profiles...)
	found := false
	for i := range s.profiles {
		if p.ID != "" && s.profiles[i].ID == p.ID {
			s.profiles[i], found = p, true
		}
	}
	if !found {
		if len(s.profiles) >= maxProfiles {
			return Profile{}, errors.New("too many profiles")
		}
		p.ID = newID()
		s.profiles = append(s.profiles, p)
	}
	if err := s.saveLocked(); err != nil {
		s.profiles = prev
		return Profile{}, err
	}
	return p, nil
}

// Delete removes a profile.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := append([]Profile{}, s.profiles...)
	out := s.profiles[:0:0]
	for _, p := range s.profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	if len(out) == len(prev) {
		return errors.New("no such profile")
	}
	s.profiles = out
	if err := s.saveLocked(); err != nil {
		s.profiles = prev
		return err
	}
	return nil
}

// AddRecent records a successful connection (address, user, vendor; never the password).
func (s *Store) AddRecent(address, user, vendor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Recent{Address: address, User: user, Vendor: vendor, At: time.Now().UTC()}
	out := []Recent{r}
	for _, x := range s.recents {
		if !strings.EqualFold(x.Address, address) && len(out) < maxRecents {
			out = append(out, x)
		}
	}
	s.recents = out
	return s.saveLocked()
}

// ClearRecents forgets recently used devices.
func (s *Store) ClearRecents() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recents = nil
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(storeFile{Version: 1, Profiles: s.profiles, Recents: s.recents}, "", "  ")
	if err != nil {
		return err
	}
	return trust.WriteFileAtomic(s.path, append(data, '\n'))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
