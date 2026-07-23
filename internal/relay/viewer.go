package relay

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"picam-frontend/internal/config"
)

// viewer is one browser's downstream PeerConnection, subscribed to a
// single upstream relay's fanned-out RTP.
type viewer struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP
	left  atomic.Bool

	mgr *Manager
	pi  config.Backend

	// maxStream is the browser's own original ?stream= request ("main"
	// or "lores"), fixed at connect time — a ceiling *group*, not a
	// literal picam-orchestrator stream name (see Subscribe, which maps
	// "main" to an initial upstream of "main-high"). Overview/thumbnail
	// viewers request "lores" and are pinned there — adaptQuality skips
	// them entirely. Detail-view viewers request "main" and range
	// between "main-low" (floor) and "main-high" (ceiling) — never
	// dropping below picam-orchestrator's native resolution, just a
	// lower bitrate — based on THIS viewer's own downstream connection
	// quality. This is the actual browser<->picam-frontend leg, unlike
	// picam-orchestrator's own (LAN-only, effectively always-clean)
	// upstream link, so this is where real adaptive quality belongs —
	// picam-orchestrator's own streams are flat/pinned, no adaptation.
	maxStream string

	mu      sync.Mutex
	current *upstream // which upstream is currently fanning out to this viewer; mutated by adaptQuality via Manager.switchViewerStream
}

// currentUpstream returns the upstream currently fanning out to this
// viewer (may differ from the one it first subscribed to, if
// adaptQuality has since moved it).
func (v *viewer) currentUpstream() *upstream {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.current
}

// Subscribe implements the browser-facing (downstream) leg of one
// POST /webrtc/offer request: answers offerSDP with a sendonly VP8 track
// that receives whatever RTP the upstream (pi, stream) relay is fanning
// out (see upstream.fanOut) — no decode/re-encode.
//
// A recover() guards this function as defense-in-depth: pion itself
// returns normal errors rather than panicking on a malformed offer, but a
// bug in this glue code must still not be able to take down every other
// connected viewer.
func (m *Manager) Subscribe(pi config.Backend, stream, offerSDP string) (answerSDP string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in webrtc negotiation: %v", p)
		}
	}()

	// The browser's stream request is a ceiling group, not a literal
	// picam-orchestrator stream name: "main" establishes its initial
	// upstream at "main-high" (the adaptive ceiling — see adaptQuality
	// below); "lores" is unrelated and unchanged (grid-view thumbnails,
	// always pinned there, no ladder).
	upstreamStream := stream
	if stream == "main" {
		upstreamStream = "main-high"
	}
	up, err := m.getOrCreateUpstream(pi, upstreamStream)
	if err != nil {
		return "", err
	}

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "picam-"+stream,
	)
	if err != nil {
		pc.Close()
		return "", err
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()
		return "", err
	}

	v := &viewer{pc: pc, track: track, mgr: m, pi: pi, maxStream: stream, current: up}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			v.currentUpstream().removeViewer(v)
		}
	})

	// Watches this browser's own RTCP feedback: PLI (e.g. on reconnect /
	// packet loss) is forwarded up to whichever upstream currently feeds
	// this viewer, so picam-orchestrator sends a fresh keyframe — mirrors
	// the C++ original's PliHandler on the downstream track. Receiver
	// Reports drive adaptive quality (see adaptQuality) — this
	// browser<->picam-frontend leg, not picam-frontend's own upstream
	// link to picam-orchestrator (LAN-only, no STUN/TURN, effectively
	// always clean), is the connection whose real-world quality varies
	// and is worth adapting to. Exits on its own once this
	// sender/PeerConnection closes (ReadRTCP then returns an error).
	go func() {
		const (
			// EMA smoothing: how much weight each new Receiver Report
			// gets. Receiver Reports arrive every few seconds, so this
			// reacts within a couple of reports without being knocked
			// around by one noisy sample.
			lossEMAAlpha = 0.4

			// Hysteresis: downgrade readily (8% sustained loss is
			// already a visibly struggling connection), but only
			// upgrade back once loss is nearly clean, and never within
			// switchCooldown of the last switch — without this gap, a
			// connection hovering right at the boundary would flap
			// between bitrate tiers every report.
			downgradeLossThreshold = 0.08
			upgradeLossThreshold   = 0.01
			switchCooldown         = 8 * time.Second
		)
		lossEMA := -1.0 // negative sentinel: no sample yet
		var lastSwitch time.Time

		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				switch pkt := p.(type) {
				case *rtcp.PictureLossIndication:
					v.currentUpstream().requestKeyframe()

				case *rtcp.ReceiverReport:
					if v.maxStream == "lores" {
						continue // pinned floor viewer (e.g. an overview thumbnail) — no ladder to adapt
					}
					for _, rr := range pkt.Reports {
						frac := float64(rr.FractionLost) / 256.0
						if lossEMA < 0 {
							lossEMA = frac
						} else {
							lossEMA = lossEMA*(1-lossEMAAlpha) + frac*lossEMAAlpha
						}
					}
					v.adaptQuality(lossEMA, &lastSwitch, downgradeLossThreshold, upgradeLossThreshold, switchCooldown)
				}
			}
		}
	}()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		pc.Close()
		return "", err
	}

	// Must be created before CreateAnswer/SetLocalDescription to avoid a
	// race with gathering completing before we start waiting on it.
	gatherComplete := webrtc.GatheringCompletePromise(pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return "", err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return "", err
	}

	select {
	case <-gatherComplete:
	case <-time.After(5 * time.Second):
		pc.Close()
		return "", ErrGatherTimeout
	}

	final := pc.LocalDescription()
	if final == nil {
		pc.Close()
		return "", ErrGatherTimeout
	}

	up.addViewer(v)
	up.requestKeyframe() // fresh viewer shouldn't wait for a spontaneous keyframe

	log.Printf("[Relay] Browser viewer connected %s/%s", pi.Name, stream)

	return final.SDP, nil
}

// adaptQuality switches this viewer between the "main-high" and
// "main-low" upstream relays for its Pi — two independently-bitrated
// encodes of picam-orchestrator's native-resolution main stream, never a
// resolution drop — based on a smoothed packet-loss estimate (see the
// RTCP goroutine in Subscribe). Only ever called for a "main"-ceiling
// viewer (see the maxStream=="lores" guard where this is invoked); a
// "lores" viewer has no ladder to adapt on since it's an unrelated,
// always-pinned stream for grid-view thumbnails. Was previously a
// resolution ladder (main/lores); repointed to a bitrate ladder on the
// same native resolution once picam-orchestrator stopped downscaling
// main and started producing two bitrate tiers of it instead — see
// picam-orchestrator-go's README. No longer mirrored by an equivalent
// mechanism in picam-orchestrator-go's webrtcsrv.Client: that link
// (picam-frontend's own upstream to picam-orchestrator) is LAN-only and
// effectively always clean, so real adaptation only ever belonged here,
// on the browser<->picam-frontend leg.
func (v *viewer) adaptQuality(lossEMA float64, lastSwitch *time.Time, downThresh, upThresh float64, cooldown time.Duration) {
	now := time.Now()
	if now.Sub(*lastSwitch) < cooldown {
		return
	}
	v.mu.Lock()
	current := v.current.key.stream
	v.mu.Unlock()

	switch {
	case current == "main-high" && lossEMA > downThresh:
		if v.mgr.switchViewerStream(v, "main-low") {
			*lastSwitch = now
			log.Printf("[Relay] %s viewer downgraded main-high->main-low (loss=%.1f%%)", v.pi.Name, lossEMA*100)
		}
	case current == "main-low" && lossEMA < upThresh:
		if v.mgr.switchViewerStream(v, "main-high") {
			*lastSwitch = now
			log.Printf("[Relay] %s viewer upgraded main-low->main-high (loss=%.1f%%)", v.pi.Name, lossEMA*100)
		}
	}
}
