// Package relay is picam-frontend's WebRTC media path: a small SFU-lite
// relay, not a byte proxy. It terminates WebRTC with each browser viewer
// AND with each picam-orchestrator backend, forwarding raw RTP packets
// between them (see upstream.go) — no decode/re-encode. One upstream
// PeerConnection per (pi, stream) pair is shared across every browser
// watching that combination, established lazily on the first subscriber
// and torn down when the last one leaves.
package relay

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/pion/webrtc/v4"

	"picam-frontend/internal/backendhttp"
	"picam-frontend/internal/config"
)

// ErrUpstreamUnavailable means the configured Pi could not be reached (or
// rejected) the upstream WHEP handshake — a 502-shaped condition for
// callers, as opposed to a malformed browser offer.
var ErrUpstreamUnavailable = errors.New("could not reach backend")

// ErrGatherTimeout means ICE gathering never completed for a
// PeerConnection this process created (either leg).
var ErrGatherTimeout = errors.New("failed to generate answer")

// relayKey identifies one upstream relay: a configured Pi's name plus the
// stream ("main-high"/"main-low"/"lores") requested from its
// picam-orchestrator — see viewer.Subscribe for how a browser's own
// "main"/"lores" request maps onto these.
type relayKey struct {
	pi     string
	stream string
}

// Manager owns every known backend, the shared WebRTC API (ICE port
// range, no STUN/TURN — both relay legs are LAN-only, or relayed through
// this process), and the set of currently-live upstream relays.
type Manager struct {
	api    *webrtc.API
	client *backendhttp.Client

	bmu      sync.RWMutex // guards backends only — read on every request, must never block behind upstream network I/O
	backends []config.Backend

	mu        sync.Mutex // guards upstreams; see getOrCreateUpstream/removeViewer for the tradeoff of holding it across network I/O
	upstreams map[relayKey]*upstream
}

// New builds a Manager with no known backends yet — they arrive
// asynchronously from internal/discovery via SetBackends. It does not
// contact any backend until the first browser subscribes.
func New(cfg *config.Config, client *backendhttp.Client) (*Manager, error) {
	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(uint16(cfg.ICEPortMin), uint16(cfg.ICEPortMax)); err != nil {
		return nil, fmt.Errorf("relay: invalid ICE port range %d-%d: %w", cfg.ICEPortMin, cfg.ICEPortMax, err)
	}
	// Convenience for same-host dev/testing (a loopback-only backend);
	// harmless in the real deployment topology since both legs of this
	// relay normally cross a real LAN link.
	se.SetIncludeLoopbackCandidate(true)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	return &Manager{
		api:       api,
		client:    client,
		upstreams: map[relayKey]*upstream{},
	}, nil
}

// SetBackends replaces the full set of known backends — called by
// internal/discovery after every mDNS browse cycle.
func (m *Manager) SetBackends(backends []config.Backend) {
	m.bmu.Lock()
	m.backends = backends
	m.bmu.Unlock()
}

// FindBackend looks up a known Pi by its short name.
func (m *Manager) FindBackend(name string) (config.Backend, bool) {
	m.bmu.RLock()
	defer m.bmu.RUnlock()
	for _, b := range m.backends {
		if b.Name == name {
			return b, true
		}
	}
	return config.Backend{}, false
}

// DefaultBackend returns the first known Pi, used whenever a request
// omits ?pi= — matches the C++ original's fallback.
func (m *Manager) DefaultBackend() (config.Backend, bool) {
	m.bmu.RLock()
	defer m.bmu.RUnlock()
	if len(m.backends) == 0 {
		return config.Backend{}, false
	}
	return m.backends[0], true
}

// Backends returns every currently known Pi.
func (m *Manager) Backends() []config.Backend {
	m.bmu.RLock()
	defer m.bmu.RUnlock()
	return m.backends
}

// getOrCreateUpstream returns the existing upstream relay for (pi, stream),
// or establishes a new one. Holds m.mu for the whole operation, including
// the network round trip on first establishment — simple and correct at
// this project's scale (up to a few Pis, a few dozen viewers), at the cost
// of briefly blocking unrelated (pi, stream) pairs' own first-subscriber
// setup if they happen to race. Matches the C++ original's tradeoff
// exactly; not worth a per-key lock for this traffic level.
func (m *Manager) getOrCreateUpstream(pi config.Backend, stream string) (*upstream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := relayKey{pi: pi.Name, stream: stream}
	if u, ok := m.upstreams[k]; ok {
		return u, nil
	}

	u, err := m.establishUpstream(pi, stream)
	if err != nil {
		return nil, err
	}
	m.upstreams[k] = u
	return u, nil
}

// switchViewerStream moves v from its current upstream to (pi, newStream)
// — lazily establishing that upstream if it doesn't exist yet — and
// requests a keyframe on it so v's decoder gets a clean start rather
// than referencing frames from the stream it just left. Used by
// viewer.adaptQuality. Returns false (leaving v unchanged) if the new
// upstream couldn't be reached.
func (m *Manager) switchViewerStream(v *viewer, newStream string) bool {
	newUp, err := m.getOrCreateUpstream(v.pi, newStream)
	if err != nil {
		log.Printf("[Relay] adaptQuality: could not switch %s to %s: %v", v.pi.Name, newStream, err)
		return false
	}

	v.mu.Lock()
	oldUp := v.current
	v.current = newUp
	v.mu.Unlock()

	newUp.addViewer(v)
	oldUp.detachViewer(v)
	newUp.requestKeyframe()
	return true
}

// Close closes every live upstream relay's PeerConnection. Called once,
// at process shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, u := range m.upstreams {
		u.pc.Close()
		delete(m.upstreams, k)
	}
}
