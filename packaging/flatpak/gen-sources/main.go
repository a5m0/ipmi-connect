// Command gen-sources writes the Flatpak sources that let flatpak-builder build ipmi-connect
// offline: every module the build vendors, as a module zip from proxy.golang.org pinned by
// SHA-256 and unpacked into vendor/, plus the vendor/modules.txt that `go mod vendor` writes.
//
// Run from the repository root: go run ./packaging/flatpak/gen-sources
// It needs network access to the Go module proxy; the Flatpak build that uses its output does not.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

type source struct {
	Type            string `json:"type"`
	URL             string `json:"url,omitempty"`
	SHA256          string `json:"sha256"`
	Dest            string `json:"dest"`
	StripComponents int    `json:"strip-components,omitempty"`
	Path            string `json:"path,omitempty"`
	DestFilename    string `json:"dest-filename,omitempty"`
}

const outDir = "packaging/flatpak"

func main() {
	tmp, err := os.MkdirTemp("", "gen-sources-")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	vendor := filepath.Join(tmp, "vendor")
	cmd := exec.Command("go", "mod", "vendor", "-o", vendor)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("go mod vendor: %v", err)
	}
	modulesTxt, err := os.ReadFile(filepath.Join(vendor, "modules.txt"))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "modules.txt"), modulesTxt, 0o644); err != nil {
		log.Fatal(err)
	}

	var sources []source
	sc := bufio.NewScanner(strings.NewReader(string(modulesTxt)))
	for sc.Scan() {
		// "# path version" lines name the vendored modules.
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[0] != "#" || !strings.HasPrefix(f[2], "v") {
			continue
		}
		mod, ver := f[1], f[2]
		url := fmt.Sprintf("https://proxy.golang.org/%s/@v/%s.zip", escape(mod), escape(ver))
		sum, err := sha256URL(url)
		if err != nil {
			log.Fatalf("%s: %v", url, err)
		}
		sources = append(sources, source{
			Type: "archive", URL: url, SHA256: sum, Dest: "vendor/" + mod,
			StripComponents: strings.Count(mod, "/") + 1, // the zip's root is <module>@<version>/
		})
		log.Printf("%s %s", mod, ver)
	}
	h := sha256.Sum256(modulesTxt)
	sources = append(sources, source{Type: "file", Path: "modules.txt", SHA256: hex.EncodeToString(h[:]), Dest: "vendor"})

	data, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "go-modules.json"), append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}

// escape applies the module proxy's case encoding: an upper-case letter becomes '!' and lower.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sha256URL(url string) (string, error) {
	resp, err := http.Get(url) //nolint:gosec,noctx // a fixed proxy URL, build tooling only
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
