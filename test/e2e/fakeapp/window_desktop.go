//go:build desktop

package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
)

// openWindow shows url in a native Wails window, as the desktop build does, and blocks until the
// window closes. Used for the desktop documentation screenshots (FAKEAPP_WINDOW=1).
func openWindow(url string) error {
	app := application.New(application.Options{
		Name:                        "IPMI Connect",
		Linux:                       application.LinuxOptions{ApplicationID: "io.github.a5m0.IPMIConnect", ProgramName: "io.github.a5m0.IPMIConnect"},
		DisableDefaultSignalHandler: true,
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "IPMI Connect", Width: 1280, Height: 860, URL: url,
		DefaultContextMenuDisabled: true,
	})
	return app.Run()
}
