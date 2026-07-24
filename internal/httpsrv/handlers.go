package httpsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"picam-frontend/internal/config"
)

func (s *Server) registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /pis.json", s.handlePis)
	mux.HandleFunc("GET /status.json", s.handleStatus)
	mux.HandleFunc("GET /osd", s.handleOSD)
	mux.HandleFunc("GET /annotate", s.handleAnnotate)
	mux.HandleFunc("GET /camera", s.handleCamera)
	mux.HandleFunc("GET /lux-switch", s.handleLuxSwitch)
	mux.HandleFunc("GET /ir-light", s.handleIRLight)
	mux.HandleFunc("POST /webrtc/offer", s.handleOffer)
	mux.HandleFunc("GET /", s.handleIndex)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleIndex serves the single-page web UI. Everything the page needs at
// runtime (the Pi list, live status, stream control) comes from the JSON
// endpoints below, called from the page's own JS.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.webDir, "index.html"))
}

type piEntry struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (s *Server) handlePis(w http.ResponseWriter, r *http.Request) {
	backends := s.relay.Backends()
	out := make([]piEntry, len(backends))
	for i, b := range backends {
		out[i] = piEntry{Name: b.Name, Label: b.Label}
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveBackend picks the Pi named by ?pi=, or the first configured Pi if
// omitted — same fallback the C++ original used.
func (s *Server) resolveBackend(r *http.Request) (config.Backend, bool) {
	name := r.URL.Query().Get("pi")
	if name == "" {
		return s.relay.DefaultBackend()
	}
	return s.relay.FindBackend(name)
}

// proxyGet forwards a GET to path on the selected backend, carrying over
// only the named query parameters (each independently optional —
// picam-orchestrator itself validates/defaults each; this is a pure
// relay) and passing the backend's JSON response straight through.
func (s *Server) proxyGet(w http.ResponseWriter, r *http.Request, path string, forward []string) {
	pi, ok := s.resolveBackend(r)
	if !ok {
		http.Error(w, "Unknown pi", http.StatusNotFound)
		return
	}

	q := url.Values{}
	for _, key := range forward {
		if v := r.URL.Query().Get(key); v != "" {
			q.Set(key, v)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status, body, err := s.client.Get(ctx, pi, path, q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "backend unreachable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleStatus implements GET /status.json?pi=X.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/status.json", nil)
}

// handleOSD implements GET /osd?pi=X&camera_id=<bool>&time=<bool>. A
// legacy combined enabled= is still forwarded too, for any caller still
// using the old single-flag API.
func (s *Server) handleOSD(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/osd", []string{"camera_id", "time", "enabled"})
}

// handleAnnotate implements GET /annotate?pi=X&lores=<bool>&main=<bool>.
func (s *Server) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/annotate", []string{"lores", "main"})
}

// handleCamera implements GET /camera?pi=X&id=N.
func (s *Server) handleCamera(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/camera", []string{"id"})
}

// handleLuxSwitch implements GET /lux-switch?pi=X&enabled=<bool>&threshold=<int>.
func (s *Server) handleLuxSwitch(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/lux-switch", []string{"enabled", "threshold"})
}

// handleIRLight implements GET /ir-light?pi=X&enabled=<bool>&threshold=<int>&max_on_minutes=<int>.
func (s *Server) handleIRLight(w http.ResponseWriter, r *http.Request) {
	s.proxyGet(w, r, "/ir-light", []string{"enabled", "threshold", "max_on_minutes"})
}
