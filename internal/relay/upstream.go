package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"picam-frontend/internal/config"
)

// upstream is one WebRTC PeerConnection from this frontend to a single
// picam-orchestrator (pi, stream) pair, shared by every browser currently
// watching that combination.
//
// Media is relayed as raw RTP: the upstream track's RTP packets arrive
// already-packetized straight from picam-orchestrator (recvonly, no
// decode), and are fanned out verbatim via WriteRTP to every downstream
// viewer's track — no decode, no re-encode. This is pion's own
// documented SFU pattern (see examples/broadcast), generalized from a
// 1-to-1 forward to 1-to-N.
type upstream struct {
	mgr *Manager
	key relayKey
	pc  *webrtc.PeerConnection

	mu      sync.Mutex
	viewers map[*viewer]struct{}
	track   *webrtc.TrackRemote // set once OnTrack fires; needed to target PLI
}

// establishUpstream performs the full WHEP-client offer/answer round trip
// against pi's picam-orchestrator (blocking network I/O) and starts
// relaying whatever RTP the resulting recvonly track receives.
func (m *Manager) establishUpstream(pi config.Backend, stream string) (*upstream, error) {
	// No ICEServers: no STUN/TURN. picam-frontend always relays, so
	// there's no direct Pi<->browser path; ICE only needs to find a
	// route between two processes on the same LAN.
	pc, err := m.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		pc.Close()
		return nil, err
	}

	u := &upstream{mgr: m, key: relayKey{pi: pi.Name, stream: stream}, pc: pc, viewers: map[*viewer]struct{}{}}

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		u.mu.Lock()
		u.track = track
		u.mu.Unlock()

		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			u.fanOut(pkt)
		}
	})

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil) // we're the offerer (recvonly)
	if err != nil {
		pc.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, err
	}
	select {
	case <-gatherComplete:
	case <-time.After(5 * time.Second):
		pc.Close()
		return nil, fmt.Errorf("%w: ICE gathering timed out generating offer to %s", ErrUpstreamUnavailable, pi.Name)
	}

	final := pc.LocalDescription()
	if final == nil {
		pc.Close()
		return nil, fmt.Errorf("%w: no local description after gathering for %s", ErrUpstreamUnavailable, pi.Name)
	}

	answerSDP, err := m.postOffer(pi, stream, final.SDP)
	if err != nil {
		pc.Close()
		return nil, err
	}

	// No manual CreateAnswer/SetLocalDescription call here — we're the
	// offerer, so this SetRemoteDescription just processes the answer and
	// settles signaling state.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		pc.Close()
		return nil, err
	}

	return u, nil
}

// postOffer sends the WHEP-style offer to pi's picam-orchestrator and
// returns its SDP answer.
func (m *Manager) postOffer(pi config.Backend, stream, offerSDP string) (string, error) {
	reqBody, err := json.Marshal(map[string]string{"sdp": offerSDP})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, body, err := m.client.PostJSON(ctx, pi, "/webrtc/offer", url.Values{"stream": {stream}}, reqBody)
	if err != nil {
		return "", fmt.Errorf("%w: %s for /webrtc/offer: %v", ErrUpstreamUnavailable, pi.Name, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: %s returned HTTP %d for /webrtc/offer: %s", ErrUpstreamUnavailable, pi.Name, status, truncate(body, 200))
	}

	var resp struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.SDP == "" {
		return "", fmt.Errorf("%w: %s returned no sdp in /webrtc/offer response: %s", ErrUpstreamUnavailable, pi.Name, truncate(body, 200))
	}
	return resp.SDP, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// fanOut relays one RTP packet to every currently-subscribed downstream
// viewer's track — the core of the relay.
func (u *upstream) fanOut(pkt *rtp.Packet) {
	u.mu.Lock()
	viewers := make([]*viewer, 0, len(u.viewers))
	for v := range u.viewers {
		viewers = append(viewers, v)
	}
	u.mu.Unlock()

	for _, v := range viewers {
		_ = v.track.WriteRTP(pkt)
	}
}

// requestKeyframe sends a PLI (picture loss indication) upstream so
// picam-orchestrator sends a fresh keyframe. A fresh viewer needs one
// promptly rather than waiting for whatever the upstream encoder's next
// spontaneous one is — there isn't one on a fixed schedule — so without
// this a new viewer joining an already-established relay could otherwise
// wait indefinitely for a decodable frame. No-ops if the upstream track
// hasn't arrived yet (only possible in the brief window between this
// relay's own establishment and its first RTP packet).
func (u *upstream) requestKeyframe() {
	u.mu.Lock()
	track := u.track
	u.mu.Unlock()
	if track == nil {
		return
	}
	_ = u.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
}

// addViewer registers v as a subscriber of this relay.
func (u *upstream) addViewer(v *viewer) {
	u.mu.Lock()
	u.viewers[v] = struct{}{}
	u.mu.Unlock()
}

// removeViewer drops v from this relay's subscriber set; if that was the
// last viewer, tears down the upstream PeerConnection entirely (no point
// keeping picam-orchestrator encoding for nobody).
//
// Both PeerConnection closes below happen on their own goroutine rather
// than synchronously here. Found the hard way in the C++ original (a real,
// reproducible hang): there, erasing a viewer's shared_ptr ran its
// PeerConnection destructor synchronously, which re-invoked this exact
// unsubscribe path reentrantly on the same thread before the outer call
// had released its lock, deadlocking on a non-recursive mutex. pion's own
// callback dispatch may not have that same reentrancy hazard, but closing
// asynchronously side-steps the entire question rather than relying on it
// — same defensive pattern already used for downstream clients elsewhere
// in this codebase (see the sibling picam-orchestrator-go's Client.markDead).
func (u *upstream) removeViewer(v *viewer) {
	if !v.left.CompareAndSwap(false, true) {
		return // Disconnected/Failed/Closed can each fire; only act once
	}
	u.detachViewer(v)
	go v.pc.Close()
}

// detachViewer removes v from this upstream's fan-out set without
// closing v's own PeerConnection — used by adaptQuality (via
// Manager.switchViewerStream) to move a still-live viewer to a
// different upstream, as opposed to removeViewer's full teardown when
// the viewer disconnects entirely. Tears down this upstream if that was
// its last viewer, same as removeViewer (no point keeping
// picam-orchestrator encoding a stream nobody's watching anymore).
func (u *upstream) detachViewer(v *viewer) {
	u.mgr.mu.Lock()
	u.mu.Lock()
	delete(u.viewers, v)
	empty := len(u.viewers) == 0
	u.mu.Unlock()
	if empty {
		if cur, ok := u.mgr.upstreams[u.key]; ok && cur == u {
			delete(u.mgr.upstreams, u.key)
		}
	}
	u.mgr.mu.Unlock()

	if empty {
		log.Printf("[Relay] Last viewer left %s/%s, tearing down upstream", u.key.pi, u.key.stream)
		go u.pc.Close()
	}
}
