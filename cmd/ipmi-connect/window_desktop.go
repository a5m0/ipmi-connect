//go:build desktop

// The native console window. Built only with -tags desktop; the default build keeps opening the
// user's browser and links no GUI library. See docs/design/wails-spike.md.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
)

const appID = "io.github.a5m0.IPMIConnect"

// wailsUI is the desktop window and the native dialogs it offers the server.
type wailsUI struct {
	app *application.App
	win *application.WebviewWindow
}

// newNativeUI prepares the window's application. Nothing is shown until Run.
func newNativeUI() nativeUI {
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
	return &wailsUI{app: app}
}

// Run shows the console and blocks until the window closes or ctx ends. It must run on the main
// goroutine (macOS requires the GUI on the main thread). The first value on links is the launch
// link; it goes from the server to the webview in memory, so the one-time code never appears on
// a command line. Later links (a fresh one asked for on the terminal) reload the window.
func (u *wailsUI) Run(ctx context.Context, links <-chan string) error {
	var first string
	select {
	case first = <-links:
	case <-ctx.Done():
		return nil
	}

	u.win = u.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "IPMI Connect",
		Width:  1280,
		Height: 900,
		URL:    first,
		// The page is ours and needs none of WebKit's own menu (Back, Reload, Open Link in New
		// Window, Inspect); right-clicks on the console go to the BMC.
		DefaultContextMenuDisabled: true,
		// Builds with the `production` tag (make desktop) also have no inspector.
	})

	go func() {
		for {
			select {
			case <-ctx.Done():
				u.app.Quit()
				return
			case url := <-links:
				u.win.SetURL(url)
			}
		}
	}()

	if err := u.app.Run(); err != nil {
		return fmt.Errorf("native window: %w", err)
	}
	return nil
}

// SaveFile asks where to save data with the native Save dialog (the FileChooser portal inside a
// Flatpak, through GTK) and writes it there. "" means the user cancelled.
func (u *wailsUI) SaveFile(_ context.Context, title, suggestedName string, data []byte) (string, error) {
	if u.win == nil {
		return "", errors.New("the window is not open yet")
	}
	path, err := u.app.Dialog.SaveFile().
		SetMessage(title).
		SetFilename(suggestedName).
		AddFilter("PNG image", "*.png").
		CanCreateDirectories(true).
		AttachToWindow(u.win).
		PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	// 0644 like any file the user saves from a desktop application; the dialog has already
	// asked before replacing an existing file.
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // the user chose this path
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// PickFiles chooses a disk image (or a folder) with the native Open dialog. It is used only
// where the FileChooser portal is unavailable.
func (u *wailsUI) PickFiles(_ context.Context, title string, directory bool) ([]string, error) {
	if u.win == nil {
		return nil, errors.New("the window is not open yet")
	}
	path, err := u.app.Dialog.OpenFile().
		SetTitle(title).
		CanChooseFiles(!directory).
		CanChooseDirectories(directory).
		AttachToWindow(u.win).
		PromptForSingleSelection()
	if err != nil || path == "" {
		return nil, err
	}
	return []string{path}, nil
}
