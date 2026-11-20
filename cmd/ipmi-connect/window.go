package main

import "context"

// nativeUI is the console's own window, in builds that have one (-tags desktop).
type nativeUI interface {
	// Run shows the window with the first link from links and blocks until it closes or ctx
	// ends. It must run on the main goroutine.
	Run(ctx context.Context, links <-chan string) error
	// SaveFile asks where to save data and writes it; "" means cancelled.
	SaveFile(ctx context.Context, title, suggestedName string, data []byte) (string, error)
	// PickFiles chooses a file or folder; nil means cancelled.
	PickFiles(ctx context.Context, title string, directory bool) ([]string, error)
	// ReadClipboard returns the text on the system clipboard ("" when it holds none). The
	// window's webview refuses navigator.clipboard.readText, so the paste panel asks Go.
	ReadClipboard(ctx context.Context) (string, error)
}
