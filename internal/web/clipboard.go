package web

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	// maxClipboardBytes bounds the clipboard text GET /api/clipboard returns: 16384 characters
	// (the paste panel's limit) of up to four UTF-8 bytes each.
	maxClipboardBytes = 64 << 10
	// clipboardTimeout bounds a clipboard read; another application owns the selection and may
	// be slow to hand it over.
	clipboardTimeout = 5 * time.Second
)

// handleClipboard returns the desktop clipboard's text as {"text": ...}. The desktop window's
// webview refuses navigator.clipboard.readText, so the paste panel falls back to this route,
// which reads the clipboard through the native UI. The guard has already checked the session,
// Host, Origin and Sec-Fetch-Site, and set Cache-Control: no-store. The text is never logged.
func (s *Server) handleClipboard(w http.ResponseWriter, r *http.Request) {
	if s.ReadClipboard == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("no native clipboard is available"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), clipboardTimeout)
	defer cancel()
	text, err := s.ReadClipboard(ctx)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, errors.New("could not read the clipboard"))
		return
	}
	if len(text) > maxClipboardBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, badRequest("the clipboard holds more than 64 KiB of text"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}
