//go:build desktop

// The native console window (spike). Built only with -tags desktop; the default build keeps
// opening the user's browser and stays free of any GUI library. See docs/design/wails-spike.md.

package main

import (
	"context"
	"fmt"

	"github.com/wailsapp/wails/v3/pkg/application"
)

const appID = "io.github.a5m0.IPMIConnect"

// nativeWindowBuilt reports whether this build can show its own window.
func nativeWindowBuilt() bool { return true }

// runWindow shows the console in a native window and blocks until the window closes or ctx ends.
// It must run on the main goroutine (macOS requires the GUI on the main thread). The first value
// on links is the launch link; it goes straight from the server to the webview in-process, so the
// one-time code never appears on a command line. Later links (a fresh one from the terminal)
// reload the window.
func runWindow(ctx context.Context, links <-chan string) error {
	app := application.New(application.Options{
		Name:        "IPMI Connect",
		Description: "Console for legacy Dell and Supermicro BMCs",
		Linux: application.LinuxOptions{
			ApplicationID: appID,
			ProgramName:   appID,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// Ctrl+C and SIGTERM are handled by run(), which logs out of the BMC first.
		DisableDefaultSignalHandler: true,
	})

	var first string
	select {
	case first = <-links:
	case <-ctx.Done():
		return nil
	}

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "IPMI Connect",
		Width:  1280,
		Height: 900,
		URL:    first,
	})

	go func() {
		for {
			select {
			case <-ctx.Done():
				app.Quit()
				return
			case url := <-links:
				win.SetURL(url)
			}
		}
	}()

	if err := app.Run(); err != nil {
		return fmt.Errorf("native window: %w", err)
	}
	return nil
}
