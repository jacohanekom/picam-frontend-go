// Package discovery finds picam-orchestrator backends on the LAN via
// mDNS/DNS-SD (Zeroconf/Bonjour) instead of reading a static host list
// from config. See the sibling picam-orchestrator-go's own
// internal/discovery package for the advertising side.
package discovery

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"picam-frontend/internal/config"
)

// ServiceType is the DNS-SD service type picam-orchestrator-go
// advertises itself under. Hardcoded to match that repo's
// internal/discovery.ServiceType exactly — the two are separate Go
// modules, so keep them in sync by hand if this ever changes.
const ServiceType = "_picam-orchestrator._tcp"

// browseWindow bounds each discovery cycle's zeroconf.Browse call. A
// responder on a normal LAN answers within well under a second, so this
// is a safety ceiling more than an expected wait — but every cycle
// blocks for the full window regardless (there's no early-exit once
// responses stop arriving), so it directly sets a floor on how quickly
// a newly-joined Pi can appear.
const browseWindow = 2 * time.Second

// Run browses for picam-orchestrator backends every intervalSecs,
// calling onUpdate with the full current set on every cycle (never a
// delta) — a backend that stops responding (offline, IP changed) simply
// isn't in the next cycle's list, with no separate expiry bookkeeping
// needed. Runs one cycle immediately before the first tick, then blocks
// until ctx is cancelled.
func Run(ctx context.Context, intervalSecs int, onUpdate func([]config.Backend)) {
	if intervalSecs <= 0 {
		intervalSecs = 3
	}
	interval := time.Duration(intervalSecs) * time.Second

	cycle(ctx, onUpdate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle(ctx, onUpdate)
		}
	}
}

func cycle(ctx context.Context, onUpdate func([]config.Backend)) {
	browseCtx, cancel := context.WithTimeout(ctx, browseWindow)
	defer cancel()

	entriesCh := make(chan *zeroconf.ServiceEntry)
	var entries []*zeroconf.ServiceEntry
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range entriesCh {
			entries = append(entries, e)
		}
	}()

	if err := zeroconf.Browse(browseCtx, ServiceType, "local.", entriesCh); err != nil {
		log.Printf("[Discovery] browse failed: %v", err)
		return
	}
	<-browseCtx.Done()
	<-done

	onUpdate(backendsFromEntries(entries))
}

// backendsFromEntries converts raw mDNS service entries into the
// config.Backend shape the rest of the app already uses. Pure and
// side-effect free so the TXT-parsing/address-selection logic is
// unit-testable without a real mDNS network.
func backendsFromEntries(entries []*zeroconf.ServiceEntry) []config.Backend {
	var backends []config.Backend
	for _, e := range entries {
		host := ""
		if len(e.AddrIPv4) > 0 {
			host = e.AddrIPv4[0].String()
		} else if len(e.AddrIPv6) > 0 {
			host = e.AddrIPv6[0].String()
		}
		if host == "" {
			continue
		}

		label := e.Instance
		for _, txt := range e.Text {
			if v, ok := strings.CutPrefix(txt, "label="); ok {
				label = v
				break
			}
		}

		backends = append(backends, config.Backend{
			Name:  e.Instance,
			Label: label,
			Host:  host,
			Port:  e.Port,
		})
	}
	return backends
}
