// Package httpsrv is picam-frontend's browser-facing HTTP server: it
// serves the single-page web UI, proxies status/control requests to the
// selected Pi's picam-orchestrator backend, and hands off WebRTC signaling
// to internal/relay.
package httpsrv

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"picam-frontend/internal/backendhttp"
	"picam-frontend/internal/relay"
)

// Server is picam-frontend's HTTP server.
type Server struct {
	relay  *relay.Manager
	client *backendhttp.Client
	webDir string
	port   int

	httpSrv *http.Server
}

// New builds a Server. Call Start to begin listening.
func New(port int, webDir string, rel *relay.Manager, client *backendhttp.Client) *Server {
	return &Server{relay: rel, client: client, webDir: webDir, port: port}
}

// Start binds the HTTP listener and begins serving in the background. A
// bind failure is fatal — matches the C++ original's hard-won fix for a
// silently-swallowed bind failure that used to fall back to a random port.
func (s *Server) Start() {
	mux := http.NewServeMux()
	s.registerHandlers(mux)

	addr := fmt.Sprintf(":%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[Frontend] FATAL: bind() failed on port %d: %v\n"+
			"[Frontend] Is something else already using this port? Check: sudo ss -tlnp | grep ':%d '",
			s.port, err, s.port)
	}

	s.httpSrv = &http.Server{Handler: withCORS(mux)}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[HTTP] serve error: %v", err)
		}
	}()
	log.Printf("[Frontend] Listening on http://0.0.0.0:%d", s.port)
}

// Stop shuts down the HTTP server.
func (s *Server) Stop(ctx context.Context) {
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(ctx)
	}
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h.ServeHTTP(w, r)
	})
}
