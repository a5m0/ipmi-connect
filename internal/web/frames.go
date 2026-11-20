package web

import (
	"math"
	"time"
)

// SetScreenSize records the host's current video mode so /api/status and newly connected
// browsers see the real resolution.
func (s *Server) SetScreenSize(width, height int) {
	s.StateMu.Lock()
	defer s.StateMu.Unlock()
	s.State.ScreenWidth = width
	s.State.ScreenHeight = height
}

// SetFPS records the rate at which the BMC is delivering video frames, which /api/status reports.
// Browsers only receive changes, so they cannot count it themselves.
func (s *Server) SetFPS(fps int) {
	s.StateMu.Lock()
	defer s.StateMu.Unlock()
	s.State.FPS = fps
}

// SetPing records the round trip to the BMC (0: unknown) for /api/status, rounded to 0.1 ms.
func (s *Server) SetPing(rtt time.Duration) {
	s.StateMu.Lock()
	defer s.StateMu.Unlock()
	s.State.PingMS = math.Round(float64(rtt.Microseconds())/100) / 10
}

// SetStatus records a console status message ("No video signal", "View only", ...) for
// /api/status; an empty text clears it.
func (s *Server) SetStatus(text string) {
	s.StateMu.Lock()
	defer s.StateMu.Unlock()
	s.State.StatusText = text
}
