package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/a5m0/ipmi-connect/internal/config"
	"github.com/a5m0/ipmi-connect/internal/session"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

// Sessions is what the server needs from the session manager.
type Sessions interface {
	Connect(ctx context.Context, req session.Request) (session.Status, error)
	Disconnect()
	Status() session.Status
}

// connectTimeout bounds one connection attempt: probes, login and channel setup.
const connectTimeout = 90 * time.Second

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/connect", s.handleConnect)
	mux.HandleFunc("POST /api/disconnect", s.handleDisconnect)
	mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /api/profiles", s.handleProfiles)
	mux.HandleFunc("POST /api/profiles", s.handleSaveProfile)
	mux.HandleFunc("DELETE /api/profiles/{id}", s.handleDeleteProfile)
	mux.HandleFunc("DELETE /api/recents", s.handleClearRecents)
	mux.HandleFunc("GET /api/trust", s.handleTrustList)
	mux.HandleFunc("DELETE /api/trust/{device}", s.handleTrustForget)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("this console cannot connect to other BMCs"))
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req session.Request
	if err := decode(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, badRequest("bad connection request"))
		return
	}
	// Probes and a slow BMC login take longer than the server's default write timeout.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(connectTimeout + 10*time.Second))
	_ = rc.SetReadDeadline(time.Now().Add(connectTimeout + 10*time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), connectTimeout)
	defer cancel()

	st, err := s.Sessions.Connect(ctx, req)
	req.Password = "" // done with it
	var consent *session.ConsentRequired
	var input *session.InputError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, st)
	case errors.As(err, &consent):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "confirmation needed", "consent": consent})
	case errors.As(err, &input):
		writeJSONError(w, http.StatusBadRequest, err)
	case errors.Is(err, session.ErrBusy):
		writeJSONError(w, http.StatusConflict, err)
	default:
		log.Printf("[Session] Connection failed: %q", err)
		writeJSONError(w, http.StatusBadGateway, err)
	}
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("no session manager"))
		return
	}
	s.Sessions.Disconnect()
	writeJSON(w, http.StatusOK, s.Sessions.Status())
}

// handleShutdown quits the application after answering: the BMC is logged out first.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.OnShutdown == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("quit from the terminal (Ctrl+C)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "quitting"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.OnShutdown()
}

type profileView struct {
	config.Profile
	Fingerprints []string `json:"fingerprints"`
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"profiles": []profileView{}, "recents": []config.Recent{}, "enabled": s.Profiles != nil}
	if s.Profiles != nil {
		var views []profileView
		for _, p := range s.Profiles.Profiles() {
			v := profileView{Profile: p, Fingerprints: []string{}}
			if s.Trust != nil {
				if d, ok := s.Trust.Get(p.Address); ok {
					v.Fingerprints = d.Fingerprints
				}
			}
			views = append(views, v)
		}
		if views != nil {
			resp["profiles"] = views
		}
		resp["recents"] = s.Profiles.Recents()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	if s.Profiles == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("profiles are disabled"))
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var p config.Profile // no password field: one sent by mistake is dropped by the decoder
	if err := decode(r, &p); err != nil {
		writeJSONError(w, http.StatusBadRequest, badRequest("bad profile"))
		return
	}
	saved, err := s.Profiles.Save(p)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if s.Profiles == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("profiles are disabled"))
		return
	}
	if err := s.Profiles.Delete(r.PathValue("id")); err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleClearRecents(w http.ResponseWriter, r *http.Request) {
	if s.Profiles != nil {
		if err := s.Profiles.ClearRecents(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

type trustView struct {
	Name string `json:"device"`
	trust.Device
}

func (s *Server) handleTrustList(w http.ResponseWriter, r *http.Request) {
	out := []trustView{}
	if s.Trust != nil {
		for k, d := range s.Trust.List() {
			out = append(out, trustView{Name: k, Device: d})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleTrustForget removes a device's pinned certificates and consents; its next connection
// asks again.
func (s *Server) handleTrustForget(w http.ResponseWriter, r *http.Request) {
	if s.Trust == nil {
		writeJSONError(w, http.StatusNotImplemented, badRequest("no trust store"))
		return
	}
	device := r.PathValue("device")
	if err := s.Trust.Forget(device); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("[Trust] Forgot certificates and consents for %q at the user's request", trust.DeviceKey(device))
	writeJSON(w, http.StatusOK, map[string]string{"status": "forgotten"})
}
