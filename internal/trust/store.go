package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Device is what the user has decided about one BMC.
type Device struct {
	// Fingerprints are the SHA-256 leaf fingerprints accepted for any of the device's TLS
	// endpoints (its web server and, on Dell, its KVM port).
	Fingerprints []string `json:"fingerprints,omitempty"`
	// LegacyTLS records consent to TLS 1.0/1.1 and CBC/3DES suites for this device.
	LegacyTLS bool `json:"legacy_tls,omitempty"`
	// InsecureChannels records consent to channels without effective encryption (plain RFB
	// KVM, RC4 virtual media under a public key, or an APCP channel without TLS).
	InsecureChannels bool      `json:"insecure_channels,omitempty"`
	Updated          time.Time `json:"updated"`
}

// Store holds trust decisions, persisted as JSON when it has a path.
type Store struct {
	path string // empty: in memory only

	mu      sync.Mutex
	devices map[string]Device
}

// storeFile is the on-disk format.
type storeFile struct {
	Version int               `json:"version"`
	Devices map[string]Device `json:"devices"`
}

const storeVersion = 1

// maxStoreFile bounds the trust file we are willing to parse.
const maxStoreFile = 4 << 20

// NewMemoryStore returns a store that is not persisted (for tests and --no-save runs).
func NewMemoryStore() *Store { return &Store{devices: map[string]Device{}} }

// OpenStore loads the store at path; a missing file is an empty store.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]Device{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxStoreFile {
		return nil, fmt.Errorf("%s is too large to be a trust store", path)
	}
	var file storeFile
	if err := json.NewDecoder(f).Decode(&file); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	for k, d := range file.Devices {
		var fps []string
		for _, fp := range d.Fingerprints {
			if c, err := ParseFingerprint(fp); err == nil && !slices.Contains(fps, c) {
				fps = append(fps, c)
			}
		}
		d.Fingerprints = fps
		s.devices[DeviceKey(k)] = d
	}
	return s, nil
}

// Path is where the store is saved ("" for memory).
func (s *Store) Path() string { return s.path }

// Get returns the decisions for a device.
func (s *Store) Get(device string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[DeviceKey(device)]
	d.Fingerprints = slices.Clone(d.Fingerprints)
	return d, ok
}

// List returns every device, keyed by device name.
func (s *Store) List() map[string]Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Device, len(s.devices))
	for k, d := range s.devices {
		d.Fingerprints = slices.Clone(d.Fingerprints)
		out[k] = d
	}
	return out
}

// Trust pins fp for a device that has no pin, or adds it to the device's set when the device
// presents several certificates (e.g. a different one on its KVM port). Call it only after the
// user has confirmed the fingerprint. Adding to a device that already has pins is refused unless
// allowAdditional is set, so a changed certificate cannot be pinned by mistake: use Replace.
func (s *Store) Trust(device, fp string, allowAdditional bool) error {
	c, err := ParseFingerprint(fp)
	if err != nil {
		return err
	}
	return s.update(device, func(d *Device) error {
		if slices.Contains(d.Fingerprints, c) {
			return nil
		}
		if len(d.Fingerprints) > 0 && !allowAdditional {
			return fmt.Errorf("%s already has a pinned certificate; replace it explicitly", DeviceKey(device))
		}
		d.Fingerprints = append(d.Fingerprints, c)
		return nil
	})
}

// Replace discards a device's pins and pins fps instead: the explicit workflow for a BMC whose
// certificate legitimately changed.
func (s *Store) Replace(device string, fps []string) error {
	var canon []string
	for _, fp := range fps {
		c, err := ParseFingerprint(fp)
		if err != nil {
			return err
		}
		if !slices.Contains(canon, c) {
			canon = append(canon, c)
		}
	}
	if len(canon) == 0 {
		return errors.New("no fingerprint to pin")
	}
	return s.update(device, func(d *Device) error {
		d.Fingerprints = canon
		return nil
	})
}

// SetConsent records the user's authorisation of obsolete protocols for a device. It only ever
// widens what is allowed; Forget withdraws everything.
func (s *Store) SetConsent(device string, legacyTLS, insecureChannels bool) error {
	return s.update(device, func(d *Device) error {
		d.LegacyTLS = d.LegacyTLS || legacyTLS
		d.InsecureChannels = d.InsecureChannels || insecureChannels
		return nil
	})
}

// Forget removes every decision about a device.
func (s *Store) Forget(device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := DeviceKey(device)
	if _, ok := s.devices[key]; !ok {
		return nil
	}
	prev := s.devices[key]
	delete(s.devices, key)
	if err := s.saveLocked(); err != nil {
		s.devices[key] = prev
		return err
	}
	return nil
}

func (s *Store) update(device string, change func(*Device) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := DeviceKey(device)
	if key == "" {
		return errors.New("no device name")
	}
	prev, existed := s.devices[key]
	d := prev
	d.Fingerprints = slices.Clone(prev.Fingerprints)
	if err := change(&d); err != nil {
		return err
	}
	d.Updated = time.Now().UTC()
	s.devices[key] = d
	if err := s.saveLocked(); err != nil {
		if existed {
			s.devices[key] = prev
		} else {
			delete(s.devices, key)
		}
		return err
	}
	return nil
}

// saveLocked writes the store atomically: a private temporary file in the same directory,
// synced, then renamed over the old one.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	keys := make([]string, 0, len(s.devices))
	for k := range s.devices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data, err := json.MarshalIndent(storeFile{Version: storeVersion, Devices: s.devices}, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, append(data, '\n'))
}

// WriteFileAtomic replaces path with data, readable only by the owner. Parent directories are
// created private (0700).
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
