package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfilesRoundTripWithoutPasswords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Save(Profile{Name: " Rack 1 ", Address: "192.0.2.10", User: "root", Vendor: "dell", LegacyTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.Name != "Rack 1" {
		t.Errorf("saved %+v", p)
	}
	if _, err := s.Save(Profile{Address: "https://x"}); err == nil {
		t.Error("URL accepted as an address")
	}
	if _, err := s.Save(Profile{Address: "192.0.2.11", Vendor: "hp"}); err == nil {
		t.Error("unknown vendor accepted")
	}
	if err := s.AddRecent("192.0.2.10", "root", "dell"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRecent("192.0.2.20", "ADMIN", "supermicro"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRecent("192.0.2.10", "root", "dell"); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Profiles(); len(got) != 1 || got[0] != p {
		t.Errorf("profiles after reload = %+v", got)
	}
	if got := s2.Recents(); len(got) != 2 || got[0].Address != "192.0.2.10" {
		t.Errorf("recents = %+v", got)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(strings.ToLower(string(data)), "pass") {
		t.Errorf("profile file mentions a password: %s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", info.Mode().Perm())
	}
	if err := s2.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(p.ID); err == nil {
		t.Error("deleting twice succeeded")
	}
}

func TestDirsFollowXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG directories apply on Linux")
	}
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	d, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if d != filepath.Join(base, "cfg", "ipmi-connect") {
		t.Errorf("Dir = %s", d)
	}
	m, err := MediaDir()
	if err != nil {
		t.Fatal(err)
	}
	if m != filepath.Join(base, "data", "ipmi-connect", "media") {
		t.Errorf("MediaDir = %s", m)
	}
	if info, err := os.Stat(m); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("media dir %v %v", info, err)
	}
}
