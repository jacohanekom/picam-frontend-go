package relay

import (
	"fmt"
	"log"
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

	up, err := m.getOrCreateUpstream(pi, stream)
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

	v := &viewer{pc: pc, track: track}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			up.removeViewer(v)
		}
	})

	// Forwards this browser's PLI (e.g. on reconnect / packet loss) up to
	// the shared upstream connection, so picam-orchestrator sends a fresh
	// keyframe — mirrors the C++ original's PliHandler on the downstream
	// track. Exits on its own once this sender/PeerConnection closes
	// (ReadRTCP then returns an error).
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					up.requestKeyframe()
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
