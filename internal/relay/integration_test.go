package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"picam-frontend/internal/backendhttp"
	"picam-frontend/internal/config"
)

// fakeOrchestrator stands in for a real picam-orchestrator's WHEP
// endpoint: it answers the offer with a sendonly VP8 track and then
// writes synthetic RTP packets to it, so a real relay.Manager talking to
// it end-to-end can be observed actually forwarding media.
func fakeOrchestrator(t *testing.T) *httptest.Server {
	t.Helper()

	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(52000, 52100); err != nil {
		t.Fatal(err)
	}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	mux := http.NewServeMux()
	mux.HandleFunc("/webrtc/offer", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SDP string `json:"sdp"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		track, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
			"video", "fake-orchestrator",
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := pc.AddTrack(track); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gatherComplete := webrtc.GatheringCompletePromise(pc)
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		<-gatherComplete

		// Stream synthetic RTP "frames" for as long as the test runs.
		go func() {
			var seq uint16
			for i := 0; ; i++ {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: seq,
						Timestamp:      uint32(i * 3000),
						SSRC:           0xC0FFEE,
					},
					Payload: []byte{0xAA, 0xBB, 0xCC},
				}
				seq++
				if err := track.WriteRTP(pkt); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()

		resp, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	})

	return httptest.NewServer(mux)
}

// fakeBrowser stands in for a browser viewer: it offers a recvonly video
// transceiver directly to Manager.Subscribe (bypassing the HTTP layer,
// which is httpsrv's concern, not relay's) and reports every RTP packet
// it receives on the resulting track.
func fakeBrowser(t *testing.T, m *Manager, pi config.Backend, stream string) (pc *webrtc.PeerConnection, received chan *rtp.Packet) {
	t.Helper()

	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(53000, 53100); err != nil {
		t.Fatal(err)
	}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	received = make(chan *rtp.Packet, 16)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			select {
			case received <- pkt:
			default:
			}
		}
	})

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatal(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherComplete

	answerSDP, err := m.Subscribe(pi, stream, pc.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatal(err)
	}

	return pc, received
}

func TestRelayEndToEnd(t *testing.T) {
	orch := fakeOrchestrator(t)
	defer orch.Close()

	u, err := url.Parse(orch.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	pi := config.Backend{Name: "test", Label: "Test", Host: u.Hostname(), Port: port}

	cfg := &config.Config{
		ICEPortMin: 51000,
		ICEPortMax: 51100,
	}
	m, err := New(cfg, backendhttp.New(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetBackends([]config.Backend{pi})

	// First viewer establishes the upstream relay.
	browser1, recv1 := fakeBrowser(t, m, pi, "main")
	defer browser1.Close()

	select {
	case pkt := <-recv1:
		if pkt.SSRC == 0 {
			t.Fatal("received packet with zero SSRC")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first viewer to receive relayed RTP")
	}

	// A second, concurrent viewer of the SAME (pi, stream) must reuse the
	// existing upstream and also receive media — the core "shared
	// upstream" behavior this relay exists for.
	m.mu.Lock()
	upstreamCount := len(m.upstreams)
	m.mu.Unlock()
	if upstreamCount != 1 {
		t.Fatalf("expected exactly 1 upstream relay after first subscriber, got %d", upstreamCount)
	}

	browser2, recv2 := fakeBrowser(t, m, pi, "main")
	defer browser2.Close()

	select {
	case <-recv2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second viewer to receive relayed RTP")
	}

	m.mu.Lock()
	upstreamCount = len(m.upstreams)
	m.mu.Unlock()
	if upstreamCount != 1 {
		t.Fatalf("expected upstream relay to still be shared (1), got %d", upstreamCount)
	}

	// Closing both viewers should tear the upstream down.
	browser1.Close()
	browser2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		upstreamCount = len(m.upstreams)
		m.mu.Unlock()
		if upstreamCount == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream relay was not torn down after last viewer left (still %d)", upstreamCount)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
