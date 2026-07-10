package httpsrv

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"picam-frontend/internal/relay"
)

type offerRequest struct {
	SDP string `json:"sdp"`
}

type answerResponse struct {
	SDP string `json:"sdp"`
}

// handleOffer implements POST /webrtc/offer?pi=X&stream=Y: WHEP-style
// signaling for a browser viewer. See relay.Manager.Subscribe for how the
// resulting connection is wired to the shared upstream relay — the actual
// media then flows relayed, not proxied.
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	pi, ok := s.resolveBackend(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown pi"})
		return
	}

	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "main"
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	var req offerRequest
	if err := json.Unmarshal(body, &req); err != nil || req.SDP == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing sdp"})
		return
	}

	answerSDP, err := s.relay.Subscribe(pi, stream, req.SDP)
	if err != nil {
		log.Printf("[Relay] /webrtc/offer failed for %s/%s: %v", pi.Name, stream, err)
		switch {
		case errors.Is(err, relay.ErrUpstreamUnavailable):
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not reach backend"})
		case errors.Is(err, relay.ErrGatherTimeout):
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate answer"})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}

	writeJSON(w, http.StatusOK, answerResponse{SDP: answerSDP})
}
