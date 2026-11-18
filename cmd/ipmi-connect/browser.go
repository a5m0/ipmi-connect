package main

import (
	"context"
	"log"
	"os/exec"
	"runtime"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/portal"
)

// openBrowser shows url in the user's browser. In a Flatpak it asks the OpenURI portal; elsewhere
// it runs the platform's opener. The URL is passed as an argument, never through a shell. If
// nothing works the link printed in the log is the fallback.
func openBrowser(url string) {
	if portal.InFlatpak() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := portal.OpenURI(ctx, url)
		cancel()
		if err == nil {
			return
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
		log.Printf("Could not open a browser (%v): open the console link above yourself", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
