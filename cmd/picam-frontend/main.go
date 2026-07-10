// Command picam-frontend is a Go port of the C++ picam-frontend service:
// a single web UI for viewing one or more picam-orchestrator backends
// (each running on its own Pi). Reads a static list of Pi addresses from
// config, serves the browser page, and proxies status JSON + WebRTC media
// from whichever Pi+stream the viewer has selected — the browser only ever
// talks to this one process, never directly to a Pi.
//
// WebRTC media is peer-to-peer by nature, which would otherwise break that
// "browser only ever talks to picam-frontend" guarantee — so this process
// is a small SFU-lite relay, not just a byte proxy for the media path: it
// terminates WebRTC with each browser viewer AND with each
// picam-orchestrator backend, forwarding raw RTP packets between them (see
// internal/relay). One upstream PeerConnection per (pi, stream) pair is
// shared across every browser watching that combination.
//
// See picam-frontend-go/README.md for the full picture, and the sibling
// ../picam-frontend (C++) project's README for the protocol-level
// rationale this is a behavioral port of.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"picam-frontend/internal/backendhttp"
	"picam-frontend/internal/config"
	"picam-frontend/internal/httpsrv"
	"picam-frontend/internal/relay"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "config.ini", "path to configuration file")
	flag.StringVar(&cfgPath, "c", "config.ini", "path to configuration file (shorthand)")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("[Config] %v", err)
	}
	logConfig(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := backendhttp.New(5 * time.Second)

	rel, err := relay.New(cfg, client)
	if err != nil {
		log.Fatalf("[Relay] %v", err)
	}

	srv := httpsrv.New(cfg.HTTPPort, cfg.WebDir, rel, client)
	srv.Start()

	log.Printf("[Main] Ready. Open http://<this-host>%s in a browser.", portSuffix(cfg.HTTPPort))

	<-ctx.Done()

	log.Printf("[Main] Shutting down.")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Stop(shutdownCtx)
	shutdownCancel()

	// Closes every relay's upstream PeerConnection explicitly rather than
	// relying on process exit to tear them down — closes cleanly notify
	// picam-orchestrator instead of just vanishing from its perspective.
	rel.Close()
}

func logConfig(cfg *config.Config) {
	log.Printf("[Config] viewer : http://0.0.0.0:%d", cfg.HTTPPort)
	log.Printf("[Config] webrtc : ice ports %d-%d", cfg.ICEPortMin, cfg.ICEPortMax)
	log.Printf("[Config] pis    : %d configured", len(cfg.Backends))
	for _, b := range cfg.Backends {
		log.Printf("  - %s (%s) -> %s:%d", b.Name, b.Label, b.Host, b.Port)
	}
	if len(cfg.Backends) == 0 {
		log.Printf("[Config] WARNING: no [pis] entries configured — add at least one to /etc/picam-frontend/config.ini")
	}
}

func portSuffix(port int) string {
	if port == 80 {
		return ""
	}
	return fmt.Sprintf(":%d", port)
}
