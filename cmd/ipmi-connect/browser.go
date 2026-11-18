package main

import (
	"context"
	"log"
	"os/exec"
	"runtime"
	"time"

	"github.com/a5m0/ipmi-connect/internal/portal"
)

// openBrowser shows url in the user's browser. In a Flatpak it asks the OpenURI portal; elsewhere
// it runs the platform's opener. The URL is passed as an argument, never through a shell. It
// reports whether an opener was started; if not, the caller prints the link instead.
//
// The link's one-time code is briefly visible in the opener's and the browser's command line,
// which other local users can read on systems without hidepid; see docs/security/threat-model.md.
func openBrowser(url string) bool {
	if portal.InFlatpak() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := portal.OpenURI(ctx, url)
		cancel()
		if err == nil {
			return true
		}
		log.Printf("Could not ask the desktop to open the browser (%v); trying xdg-open", err)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Could not open a browser (%v)", err)
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}
