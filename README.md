# picam-frontend (Go)

A from-scratch Go reimplementation of [`picam-frontend`](../picam_frontend) — a single web UI for viewing and controlling one or more [`picam-orchestrator`](../picam-orchestrator-go) backends (each running on its own Pi). Same config file format and HTTP endpoint surface as the original C++ implementation — see that project's README for the full protocol-level rationale; this one focuses on what's specific to the Go port.

## Why a Go port

The original C++ implementation vendors [libdatachannel](https://github.com/paullouisageneau/libdatachannel) via CMake `FetchContent` (needs network access at configure time) and links `libssl`. This port instead uses:

- **[pion/webrtc](https://github.com/pion/webrtc)** (pure Go) for WebRTC/ICE/DTLS/SRTP — no vendored C++ WebRTC stack, no OpenSSL build step. Same pion dependency the sibling [`picam-orchestrator-go`](../picam-orchestrator-go) already uses.
- **Go's standard `net/http`** for every proxied request (`/status.json`, `/osd`, `/annotate`, `/camera`, and the outbound WHEP offer to picam-orchestrator), instead of hand-rolled raw sockets. The C++ original's `connectWithTimeout`, manual HTTP request-line construction, and CRLF-injection sanitizing of every forwarded query value are all just... not needed: `net/http` resolves hostnames, applies timeouts via `context`, and safely percent-encodes query parameters on its own.

Both WebRTC legs remain a **raw RTP relay**, exactly like the C++ original: the upstream (picam-orchestrator-facing) track's RTP packets are fanned out verbatim to every downstream (browser-facing) track via `TrackLocalStaticRTP.WriteRTP` — no decode, no re-encode. This is pion's own documented SFU pattern (see its `examples/broadcast`), generalized from 1-to-1 to 1-to-N. The single-page web UI (`web/index.html`) is unchanged — it talks to the same JSON/WHEP endpoints either way.

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
curl -fsSL https://repo.aipicam.com/pubkey.asc | sudo gpg --dearmor -o /usr/share/keyrings/aipicam.gpg

# stable releases
echo "deb [signed-by=/usr/share/keyrings/aipicam.gpg] https://repo.aipicam.com main main" | sudo tee /etc/apt/sources.list.d/aipicam.list

# or nightly builds instead
echo "deb [signed-by=/usr/share/keyrings/aipicam.gpg] https://repo.aipicam.com nightly main" | sudo tee /etc/apt/sources.list.d/aipicam.list

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
| `POST /webrtc/offer?pi=X&stream=Y` | WHEP-style signaling for a browser viewer — body `{"sdp":"..."}` (SDP offer), response `{"sdp":"..."}` (SDP answer). Media then flows over the resulting WebRTC connection, relayed from Pi X. |
| `GET /status.json?pi=X` | Proxied telemetry JSON from Pi X |
| `GET /camera?pi=X&id=N` | Switch camera lens on Pi X |
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

`relay.Manager.Subscribe` (called from `POST /webrtc/offer`) gets-or-creates the upstream relay for the requested `(pi, stream)`, answers the browser's offer with a sendonly VP8 track, and registers a `viewer` against that upstream. The upstream's `OnTrack` read loop fans out every RTP packet it receives to each registered viewer's track (`upstream.fanOut`). When a viewer's `PeerConnection` disconnects, `upstream.removeViewer` unregisters it and — if it was the last viewer — tears down the upstream connection entirely, so picam-orchestrator stops encoding for nobody. A `sync.Mutex` on the `Manager` guards the upstream map (held across the first-establishment network round trip, same tradeoff the C++ original made); each `upstream`'s own mutex guards its viewer set and is never held during I/O, so the hot RTP fan-out path never contends with slow connection setup/teardown.

PeerConnection closes triggered from within a connection-state-change callback happen on their own goroutine rather than synchronously in that callback — the C++ original hit a real, reproducible deadlock this way (destroying a `shared_ptr` re-entered the same unsubscribe path on the same thread while its lock was still held); closing asynchronously here side-steps that whole class of bug regardless of whether pion's own callback dispatch has the same hazard.

## Systemd service

```bash
systemctl start   picam-frontend
systemctl stop    picam-frontend
systemctl status  picam-frontend
journalctl -u picam-frontend -f
```

The unit runs as an unprivileged user with `CAP_NET_BIND_SERVICE` (for port 80) and restarts automatically after 3 seconds on failure.
