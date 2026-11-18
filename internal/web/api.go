package web

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/session"
)

// statusResponse is /api/status: the console telemetry plus the session manager's view.
type statusResponse struct {
	bmc.SessionState
	Phase       string `json:"phase"`
	LastError   string `json:"last_error,omitempty"`
	Version     string `json:"version"`
	Desktop     bool   `json:"desktop"`
	IdleTimeout int    `json:"idle_timeout_seconds,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.StateMu.RLock()
	resp := statusResponse{SessionState: s.State, Version: s.Version, Desktop: s.Desktop, IdleTimeout: int(s.IdleTimeout / time.Second)}
	s.StateMu.RUnlock()
	resp.Phase = session.PhaseIdle
	if s.bound() != nil {
		resp.Phase = session.PhaseConnected
	}
	if s.Sessions != nil {
		st := s.Sessions.Status()
		resp.Phase, resp.LastError = st.Phase, st.LastError
		resp.Channels, resp.SecurityLevel = st.Channels, st.SecurityLevel
	}
	if resp.Channels == nil {
		resp.Channels = []bmc.ChannelSecurity{}
	}
	resp.Connected = s.bound() != nil
	writeJSON(w, http.StatusOK, resp)
}

// powerActions are the actions the API accepts.
var powerActions = map[bmc.PowerAction]struct{}{
	bmc.PowerOn: {}, bmc.PowerOff: {}, bmc.PowerReset: {}, bmc.PowerCycle: {}, bmc.PowerSoftOff: {},
}

// destructivePower lists actions that can lose the host's unsaved work: the request must say it
// was confirmed by the user.
var destructivePower = map[bmc.PowerAction]bool{
	bmc.PowerOff: true, bmc.PowerReset: true, bmc.PowerCycle: true, bmc.PowerSoftOff: true,
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		Action  bmc.PowerAction `json:"action"`
		Confirm bool            `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, badRequest("send {\"action\": ..., \"confirm\": true}"))
		return
	}
	if _, known := powerActions[req.Action]; !known {
		writeJSONError(w, http.StatusBadRequest, badRequest("unknown power action"))
		return
	}
	if destructivePower[req.Action] && !req.Confirm {
		writeJSONError(w, http.StatusBadRequest, badRequest("this power action needs confirmation"))
		return
	}

	console := s.console()
	if console == nil {
		writeJSONError(w, http.StatusServiceUnavailable, badRequest("no BMC is connected"))
		return
	}
	log.Printf("[Server] Received chassis power action: %q", req.Action)
	if err := console.PowerControl(req.Action); err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}

	s.startFollowPower(console)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": string(req.Action)})
}

// handleWSTicket issues a single-use ticket for opening the console socket.
func (s *Server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"ticket": s.auth.issueTicket()})
}

// handleLogout signs every browser out: the token is replaced and every socket closes. The BMC
// session is left as it is; the Disconnect action ends that.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.logout()
	s.closeSockets()
	log.Printf("[Server] Browser signed out; open the console from the app for a new link")
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

const (
	powerFollowInterval = time.Second
	powerFollowDuration = 30 * time.Second
)

// startFollowPower re-reads the power state every second for a while after a power action, so
// the UI reflects it within a second or two instead of at the next slow poll. Repeated actions
// extend the one running follower rather than starting more; it reports whether it started one.
func (s *Server) startFollowPower(console bmc.Console) bool {
	reader, ok := console.(bmc.PowerReader)
	if !ok {
		return false
	}
	s.followMu.Lock()
	defer s.followMu.Unlock()
	s.followUntil = time.Now().Add(s.powerFollowFor)
	if s.following {
		return false
	}
	s.following = true
	done := s.connCtx.Done()
	if b := s.bound(); b != nil {
		done = b.ctx.Done()
	}
	go s.followPower(reader, done)
	return true
}

func (s *Server) followPower(reader bmc.PowerReader, done <-chan struct{}) {
	t := time.NewTicker(s.powerFollowEvery)
	defer t.Stop()
	for {
		s.followMu.Lock()
		if time.Now().After(s.followUntil) {
			s.following = false
			s.followMu.Unlock()
			return
		}
		s.followMu.Unlock()
		select {
		case <-done:
			s.followMu.Lock()
			s.following = false
			s.followMu.Unlock()
			return
		case <-t.C:
			state, err := reader.PowerState()
			if err != nil {
				state = bmc.PowerStateUnknown
			}
			s.SetPower(state)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
