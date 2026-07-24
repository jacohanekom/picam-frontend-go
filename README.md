# picam-frontend (Go)

A from-scratch Go reimplementation of [`picam-frontend`](../picam_frontend) — a single web UI for viewing and controlling one or more [`picam-orchestrator`](../picam-orchestrator-go) backends (each running on its own Pi). Same config file format and HTTP endpoint surface as the original C++ implementation — see that project's README for the full protocol-level rationale; this one focuses on what's specific to the Go port.

## Why a Go port

The original C++ implementation vendors [libdatachannel](https://github.com/paullouisageneau/libdatachannel) via CMake `FetchContent` (needs network access at configure time) and links `libssl`. This port instead uses:

- **[pion/webrtc](https://github.com/pion/webrtc)** (pure Go) for WebRTC/ICE/DTLS/SRTP — no vendored C++ WebRTC stack, no OpenSSL build step. Same pion dependency the sibling [`picam-orchestrator-go`](../picam-orchestrator-go) already uses.
- **Go's standard `net/http`** for every proxied request (`/status.json`, `/osd`, `/annotate`, `/camera`, and the outbound WHEP offer to picam-orchestrator), instead of hand-rolled raw sockets. The C++ original's `connectWithTimeout`, manual HTTP request-line construction, and CRLF-injection sanitizing of every forwarded query value are all just... not needed: `net/http` resolves hostnames, applies timeouts via `context`, and safely percent-encodes query parameters on its own.

Both WebRTC legs remain a **raw RTP relay**, exactly like the C++ original: the upstream (picam-orchestrator-facing) track's RTP packets are fanned out verbatim to every downstream (browser-facing) track via `TrackLocalStaticRTP.WriteRTP` — no decode, no re-encode. This is pion's own documented SFU pattern (see its `examples/broadcast`), generalized from 1-to-1 to 1-to-N. The single-page web UI (`web/index.html`) is unchanged — it talks to the same JSON/WHEP endpoints either way.

picam-orchestrator streams its `main` feed at full native capture resolution (no downscale) as two simultaneous, independently-bitrated VP8 encodes — `main-high`/`main-low` — rather than one fixed encode. A browser's detail-view request for `main` starts this process's upstream connection on `main-high`; `relay.viewer.adaptQuality` then moves that specific browser viewer's own upstream between `main-high` and `main-low` based on *that browser's* downstream RTCP packet-loss reports (never picam-orchestrator's own link to this process, which is LAN-only and effectively always clean — see picam-orchestrator-go's README for why the real adaptation belongs here, not there). A struggling viewer never drops below native resolution, just bitrate/quality. `lores` is unrelated to any of this — a third, always-pinned, always-native-lores-resolution stream used unconditionally for the grid view's overview thumbnails.

## Requirements

**Build:**
- Go 1.22+ (no cgo, no system libraries — `CGO_ENABLED=0` is set explicitly in the Debian build)

**Runtime:**
- One or more reachable `picam-orchestrator` (or `picam-orchestrator-go`) backends

## Build

```bash
go build -o picam-frontend ./cmd/picam-frontend
```

No network access is needed at build time beyond the initial `go mod download` (every dependency is pure Go).

## Install (Debian package)

```bash
dpkg -i picam-frontend_*.deb
systemctl enable --now picam-frontend
```

The package creates a `picam-frontend` system user, installs the systemd unit, and deploys a default `config.ini` and the web UI assets.

### From the APT repository

CI publishes to a signed APT repository (shared with other aipicam Raspberry Pi packages) hosted on Cloudflare R2, with two channels:

- **`main`** — pushing a `v*` tag publishes the clean release version here.
- **`nightly`** — every push (to any branch, and PRs) publishes a dev build here, versioned with a `+<UTC timestamp>` suffix.

```bash
curl -fsSL https://apt.aipicam.com/pubkey.asc | sudo gpg --dearmor -o /usr/share/keyrings/aipicam.gpg

# stable releases
echo "deb [signed-by=/usr/share/keyrings/aipicam.gpg] https://apt.aipicam.com main main" | sudo tee /etc/apt/sources.list.d/aipicam.list

# or nightly builds instead
echo "deb [signed-by=/usr/share/keyrings/aipicam.gpg] https://apt.aipicam.com nightly main" | sudo tee /etc/apt/sources.list.d/aipicam.list

sudo apt-get update
sudo apt-get install picam-frontend
```

Builds run on GitHub's native `ubuntu-24.04-arm` hosted runner (no QEMU). Uses the same `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `GPG_PRIVATE_KEY`, and `GPG_KEY_ID` repo secrets described in [pi-block-cpu-cores](../pi-block-cpu-cores)'s README, since it publishes into the same shared repo.

## Usage

```bash
./picam-frontend --config config.ini
```

| Flag | Default | Description |
|------|---------|-------------|
| `--config`, `-c` | `config.ini` | Path to configuration file |

Then open `http://localhost` (or whatever `output.http_port` is set to) in a browser.

## Configuration

Same `config.ini` format and defaults as the C++ original (hand-rolled INI parser: `[section]` headers, `key = value` pairs, `;`/`#` comments). See [`config.ini`](config.ini) in this directory for the full annotated file.

```ini
[pis]
# Format: name = host[:port][, Display Label]
# Default port: 81 (picam-orchestrator's default)
front = 10.10.0.50,Front Yard
back  = 10.10.0.51,Back Yard

[output]
http_port = 80
web_dir   = /usr/share/picam-frontend/web

[webrtc]
ice_port_min = 50000     ; port range for both relay legs (upstream-to-Pi, downstream-to-browser)
ice_port_max = 50200
```

## HTTP Endpoints

| Endpoint | Description |
|---|---|
| `GET /` | Serves `index.html` |
| `GET /pis.json` | JSON array of configured Pi objects (`{"name":...,"label":...}`) |
| `POST /webrtc/offer?pi=X&stream=main\|lores` | WHEP-style signaling for a browser viewer — body `{"sdp":"..."}` (SDP offer), response `{"sdp":"..."}` (SDP answer). Media then flows over the resulting WebRTC connection, relayed from Pi X. `main` requests are adaptive (see Architecture) — the browser never chooses `main-high`/`main-low` directly. |
| `GET /status.json?pi=X` | Proxied telemetry JSON from Pi X |
| `GET /camera?pi=X&id=N` | Switch camera lens on Pi X |
| `GET /lux-switch?pi=X&enabled=true\|false&threshold=N` | Configure Pi X's automatic lens switching by ambient light — the switch decision itself runs on picam-orchestrator, not here; see that project's README |
| `GET /osd?pi=X&camera_id=true\|false&time=true\|false` | Toggle OSD overlays |
| `GET /annotate?pi=X&main=true\|false&lores=true\|false` | Toggle annotation |

Every response (including errors) carries `Access-Control-Allow-Origin: *`. If `?pi=` is omitted, the first configured Pi is used.

## Architecture

```
Browser ─WebRTC (VP8)─► picam-frontend (this) ─WebRTC (VP8)─► picam-orchestrator (each Pi)
Browser ──── HTTP ─────► picam-frontend (this) ──── HTTP ─────► picam-orchestrator (each Pi)
                          (status JSON, control commands — a plain proxy)
```

### Package layout

| Package | Responsibility |
|---|---|
| `internal/config` | INI config parsing into a typed `Config` struct |
| `internal/backendhttp` | Shared `net/http` client for proxying requests to a picam-orchestrator backend |
| `internal/relay` | The WebRTC SFU-lite relay: one upstream `PeerConnection` per (Pi, stream), fanned out to per-browser downstream `PeerConnection`s |
| `internal/httpsrv` | Browser-facing HTTP server: static UI, proxy routes, WHEP signaling handoff to `internal/relay` |
| `cmd/picam-frontend` | Startup wiring |

### Relay lifecycle

`relay.Manager.Subscribe` (called from `POST /webrtc/offer`) maps the browser's `stream` request onto an upstream relay key — `"main"` becomes an initial upstream of `"main-high"`, `"lores"` is unchanged — gets-or-creates that upstream, answers the browser's offer with a sendonly VP8 track, and registers a `viewer` against it. The upstream's `OnTrack` read loop fans out every RTP packet it receives to each registered viewer's track (`upstream.fanOut`). When a viewer's `PeerConnection` disconnects, `upstream.removeViewer` unregisters it and — if it was the last viewer — tears down the upstream connection entirely, so picam-orchestrator stops encoding for nobody. A `sync.Mutex` on the `Manager` guards the upstream map (held across the first-establishment network round trip, same tradeoff the C++ original made); each `upstream`'s own mutex guards its viewer set and is never held during I/O, so the hot RTP fan-out path never contends with slow connection setup/teardown.

A `"main"`-ceiling viewer additionally watches its own downstream RTCP Receiver Reports and calls `viewer.adaptQuality` (a smoothed packet-loss estimate with hysteresis — quick to downgrade, slower/cooled-down to upgrade, to avoid flapping on a borderline connection) to move *that one viewer* between the `"main-high"` and `"main-low"` upstreams via `Manager.switchViewerStream` — lazily establishing the target upstream if it's not already live, detaching from the old one, and requesting a fresh keyframe so the viewer's decoder isn't left referencing frames from the upstream it just left. A `"lores"`-pinned viewer (grid-view thumbnails) skips this entirely — there's no ladder to adapt on.

PeerConnection closes triggered from within a connection-state-change callback happen on their own goroutine rather than synchronously in that callback — the C++ original hit a real, reproducible deadlock this way (destroying a `shared_ptr` re-entered the same unsubscribe path on the same thread while its lock was still held); closing asynchronously here side-steps that whole class of bug regardless of whether pion's own callback dispatch has the same hazard.

## Systemd service

```bash
systemctl start   picam-frontend
systemctl stop    picam-frontend
systemctl status  picam-frontend
journalctl -u picam-frontend -f
```

The unit runs as an unprivileged user with `CAP_NET_BIND_SERVICE` (for port 80) and restarts automatically after 3 seconds on failure.
