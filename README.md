# voicx

A TeamSpeak-like voice/video server written in Go. TCP/JSON control protocol, UDP media transport, Pion WebRTC engine, PostgreSQL persistence, TS3-style permissions.

## Architecture

- **TCP control channel** (`:10011`) — length-prefixed binary frames carrying JSON messages (`internal/netproto`). Handles authentication, channel management, chat, kick/ban/move, and keepalive. Handlers are wired to the backends via `server.Deps` with a permission middleware (`internal/server`).
- **UDP media channel** (`:9987`) — datagram listener with a bounded worker pool for voice/signaling packets (`internal/server/udp.go`). Media routing lands with the Pion integration.
- **WebRTC engine** (`internal/webrtc`) — Pion-based SFU engine (constructed at startup; wiring into the media path is a later phase).
- **PostgreSQL** — users, channels, groups, permissions, bans, tokens, offline messages (`internal/store`, migrations in `internal/store/migrations/`).
- **Permissions** (`internal/permissions`) — TeamSpeak 3 model: five evaluation tiers (server group → client → channel client → channel → channel group), power-vs-needed-power comparisons, skip/negate flags, DB-backed loader with a 5s cache.
- **State** (`internal/state`) — goroutine-safe in-memory tracking of clients, channels, membership, and speaking state.
- **Broadcast** (`internal/broadcast`) — per-client outbound channels, non-blocking sends, channel-tree snapshots, JSON event envelopes.
- **Redis** (`internal/redisx`) — optional; reserved for pub/sub fan-out and rate limiting (later phases). The server degrades gracefully when Redis is unreachable.
- **Health** (`internal/health`) — HTTP `/healthz` (liveness) and `/readyz` (Postgres ping) on `:9090`.

## Implemented features

- Authentication: Argon2id password and Ed25519 challenge-response (TS3-style unique IDs), with an in-memory positive-result cache.
- Ban enforcement at authenticate time (unique-ID and IP bans, expiry-aware).
- Channels: create/delete, temporary/semi-permanent/permanent types, automatic cleanup of empty temporary channels, channel passwords, per-channel needed-join-power, persisted channels loaded into state at startup.
- Permission-gated operations: channel create/delete, join (power and password/ignore-password), kick (channel/server), ban, moving other clients.
- Chat: channel, global, and direct messages; direct messages to offline users are spooled (`offline_messages`) and delivered on next login.
- Voice SFU: WebRTC signaling over the control channel (SDP offer/answer, trickle ICE), audio fan-out with per-subscriber tracks, whisper lists, talk-power gate, VAD speaking events (ssrc-audio-level header extension, 300ms hangover), 3D position relays.
- Video SFU: channel fan-out with per-subscriber tracks, simulcast layer selection per subscriber (`high`/`mid`/`low` → RID `f`/`h`/`q` with fallback), PLI/FIR keyframe relay to publishers and on subscriber join, video-publish permission gate.
- Server-side recording: optional ffmpeg-subprocess recorder for channel audio/video (`internal/recorder`), start/stop via control message, hardware-encoder-ready argument configuration.
- ServerQuery: TS3-style line-based admin protocol on `:10012` (`internal/query`) for headless administration and bots — admin-only login, client/channel listing, move/kick/ban, text injection, channel create/delete; connection cap, idle timeout, and login brute-force lockout built in.
- File transfer: token-authorized uploads/downloads on `:30033` (`internal/filetransfer`) — single-use short-lived tokens issued over the control channel, SHA-256 integrity, per-channel quotas, per-connection bandwidth caps, per-transfer size caps, on-disk storage per channel with a `files` metadata table.
- Server password: optional global password (`server_password`, hashed at startup), enforced at authenticate time.
- Avatars & channel icons: base64 upload with type/size validation (png/jpeg/gif/webp, 256 KiB), stored under the file root; `avatar_changed`/`channel_icon_changed` events; `has_icon` in channel snapshots.
- Complaints: file complaints against users (max 5 open per reporter); manage via ServerQuery (`complaintlist`/`complaintdel`/`complaintdelall`).
- Privilege tokens: TS3-style privilege keys (`tokenadd`/`tokenlist`/`tokendelete` via ServerQuery, `TokenUse` from clients) granting server-group membership or admin; first-run bootstrap logs a one-time admin token when no admin exists.
- Screen share: declared via `ScreenShare`, relayed to channel members as `screenshare_changed`.
- Client Info (TS3-style): per-client activity tracking (connect/idle times, bytes in/out, smoothed RTT from server-initiated 15s pings), `ClientInfoQuery`/`ClientInfoResponse` — self queries always full, others' IP/port gated by admin or `b_client_remoteaddress_view` (deny-on-unset).
- Prometheus metrics on `/metrics` (health port): `voicx_clients_connected`, `voicx_channels_active`, `voicx_udp_packets_total{kind}`, `voicx_udp_packets_dropped_total`, `voicx_tcp_connections_total`, `voicx_chat_messages_total{scope}`, `voicx_webrtc_peers`, `voicx_rtp_packets_forwarded_total{media}`, `voicx_file_transfers_total{direction,result}`, plus the Go collector.
- UDP DDoS mitigation: per-source-IP token-bucket rate limiting with idle-bucket eviction (`udp_rate_limit_pps`, `udp_rate_burst`; 0 disables).
- Ping/pong keepalive; graceful shutdown of all listeners.
- Health/readiness HTTP endpoints; optional Redis connectivity check at startup.

## Ports

| Port  | Protocol | Purpose                                    |
|-------|----------|--------------------------------------------|
| 9987  | UDP      | Voice/media transport                      |
| 10011 | TCP      | Control channel (query, JSON frames)       |
| 10012 | TCP      | ServerQuery (admin/bot text protocol)      |
| 30033 | TCP      | File transfer (token-authorized)           |
| 9090  | TCP/HTTP | Health (`/healthz`), readiness (`/readyz`), metrics (`/metrics`) |
| 50051 | TCP      | Reserved for the gRPC API (future)         |

## Configuration

Loaded with viper. Precedence (highest first):

1. Environment variables prefixed `VOICX_` (e.g. `VOICX_TCP_ADDR`)
2. `config.yaml` — searched in the working directory first, then `/etc/voicx`
3. Built-in defaults ([`internal/config/config.go`](internal/config/config.go))

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `server_name` | `VOICX_SERVER_NAME` | `voicx` | Server display name |
| `server_password` | `VOICX_SERVER_PASSWORD` | `""` | Global server password (empty = open; hashed at startup) |
| `log_level` | `VOICX_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `dev_mode` | `VOICX_DEV_MODE` | `true` | Development (console) vs production (JSON) logger |
| `tcp_addr` | `VOICX_TCP_ADDR` | `:10011` | Control listener address |
| `udp_addr` | `VOICX_UDP_ADDR` | `:9987` | Media listener address |
| `grpc_addr` | `VOICX_GRPC_ADDR` | `:50051` | gRPC address (reserved) |
| `health_addr` | `VOICX_HEALTH_ADDR` | `:9090` | Health HTTP listener address |
| `query_addr` | `VOICX_QUERY_ADDR` | `:10012` | ServerQuery listener address |
| `file_addr` | `VOICX_FILE_ADDR` | `:30033` | File-transfer listener address |
| `file_root` | `VOICX_FILE_ROOT` | `./data/files` | File storage root (`<channel>/<name>`) |
| `file_max_kbps` | `VOICX_FILE_MAX_KBPS` | `0` | Per-connection bandwidth cap (KiB/s, 0=unlimited) |
| `file_channel_quota_mb` | `VOICX_FILE_CHANNEL_QUOTA_MB` | `0` | Per-channel file quota (MiB, 0=unlimited) |
| `file_max_size_mb` | `VOICX_FILE_MAX_SIZE_MB` | `100` | Per-transfer size cap (MiB, 0=unlimited) |
| `database_url` | `VOICX_DATABASE_URL` | `postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable` | Postgres DSN |
| `redis_addr` | `VOICX_REDIS_ADDR` | `localhost:6379` | Redis address |
| `redis_password` | `VOICX_REDIS_PASSWORD` | `""` | Redis password |
| `redis_enabled` | `VOICX_REDIS_ENABLED` | `true` | Set `false` to run without Redis |
| `max_clients` | `VOICX_MAX_CLIENTS` | `1024` | Maximum simultaneous clients |
| `db_max_open_conns` | `VOICX_DB_MAX_OPEN_CONNS` | `25` | Postgres pool size |
| `db_max_idle_conns` | `VOICX_DB_MAX_IDLE_CONNS` | `5` | Postgres idle pool size |
| `db_conn_max_lifetime` | `VOICX_DB_CONN_MAX_LIFETIME` | `30m` | Postgres connection lifetime |
| `webrtc.ice_servers` | `VOICX_WEBRTC_ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs |
| `webrtc.enable_av1` | `VOICX_WEBRTC_ENABLE_AV1` | `false` | Register the AV1 video codec |
| `udp_rate_limit_pps` | `VOICX_UDP_RATE_LIMIT_PPS` | `200` | Per-IP UDP packet rate (pkt/s, 0=disabled) |
| `udp_rate_burst` | `VOICX_UDP_RATE_BURST` | `400` | Per-IP UDP burst allowance |
| `recording.enabled` | `VOICX_RECORDING_ENABLED` | `false` | Enable server-side recording |
| `recording.dir` | `VOICX_RECORDING_DIR` | `recordings` | Recording output directory |
| `recording.ffmpeg_path` | `VOICX_RECORDING_FFMPEG_PATH` | `ffmpeg` | ffmpeg binary |
| `recording.format` | `VOICX_RECORDING_FORMAT` | `webm` | Output container (`webm`/`mp4`) |
| `recording.video_args` | — (YAML only) | `["-c:v", "copy"]` | ffmpeg video output options (hardware encoders go here) |
| `recording.audio_args` | — (YAML only) | `["-c:a", "copy"]` | ffmpeg audio output options |

## Development

Requires Go 1.25+ and PostgreSQL. Redis is optional.

```bash
make build        # compile to ./bin/voicx
make run          # go run ./cmd/server
make migrate      # apply database migrations
make test         # full test suite
make vet          # go vet
make tidy         # go mod tidy
```

DB-backed tests self-skip when no Postgres is reachable. To run them:

```bash
export VOICX_TEST_DATABASE_URL="postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable"
go test ./...
```

Full stack (server + Postgres + Redis) via Docker Compose:

```bash
make compose-up    # build & start detached
make compose-logs  # tail logs
make compose-down  # stop & remove
```

The compose stack configures the server purely through `VOICX_*` environment variables; no config file is mounted.

## Local e2e

Two helper binaries support live testing against a running server (e.g. `make compose-up`):

`cmd/adduser` registers users in the database (there is no protocol-level registration):

```bash
go run ./cmd/adduser -nickname alice -password alicepw
go run ./cmd/adduser -nickname bob -password bobpw
go run ./cmd/adduser -nickname admin -password adminpw -admin
# Each prints: unique_id: <uid>  (existing users print a notice and exit 0)
```

`cmd/e2e` runs a live checklist (health, UDP, auth, channels, chat, permissions, file transfer, ServerQuery) and prints `PASS`/`FAIL` per check plus a summary:

```bash
go run ./cmd/e2e \
  -alice-uid <alice uid> -alice-pass alicepw \
  -bob-uid <bob uid> -bob-pass bobpw \
  -admin-uid <admin uid> -admin-pass adminpw
# exit code 0 only when all checks pass
```

Note: authentication uses the **unique ID** printed by adduser, not the nickname. Ports default to localhost standard values; override with `-addr`, `-query-addr`, `-health-url`, `-udp-addr`, `-file-addr`, and `-server-password` if the server has a global password.

## Load testing

`cmd/loadtest` is a headless client simulator (TCP auth, channel join, chat, ping, optional UDP pings):

```bash
go run ./cmd/loadtest -addr 127.0.0.1:10011 -clients 50 -duration 30s -ramp 5s \
    -unique-id <uid> -password <pw> [-channel 1] [-udp -udp-addr 127.0.0.1:9987]
```

All simulated clients share one account (voicx allows multiple connections per unique ID). Registration is not exposed over the protocol — create the test user directly (e.g. a one-off snippet calling `auth.RegisterUser`, or via psql). The report prints connect/auth counts, failures, an auth-latency histogram, and message counts.

## CI/CD

`.github/workflows/ci.yml` runs on every push/PR: gofmt gate, `go vet`, `go build`, and `go test -race` against Postgres and Redis service containers (DB-backed tests run via `VOICX_TEST_DATABASE_URL`). Tags additionally build and push the Docker image to GHCR via `docker/build-push-action` (default `GITHUB_TOKEN`).

## Security notes

- The container image runs as a non-root user (uid 10001); the compose service additionally sets `no-new-privileges`, drops all capabilities, mounts the root filesystem read-only, and keeps writable data (uploads, avatars, icons, recordings) on a named volume at `/data`.
- Passwords are Argon2id-hashed; challenge-response auth uses Ed25519; the server password is hashed at startup.
- The UDP listener applies per-source-IP token-bucket rate limiting with idle-bucket eviction (`udp_rate_limit_pps`/`udp_rate_burst`).
- ServerQuery is admin-only with connection caps, idle timeouts, and login lockout; file transfers use single-use short-lived tokens.

## Recording and hardware acceleration

Server-side recording (`internal/recorder`) pipes a channel's RTP into an `ffmpeg` subprocess over loopback UDP (stream layout via a generated SDP file) and writes WebM/MP4. It is disabled by default; enable it in `config.yaml`:

```yaml
recording:
  enabled: true
  dir: recordings
  ffmpeg_path: ffmpeg
  format: webm
  video_args: ["-c:v", "copy"]   # remux; see below for hardware encoders
  audio_args: ["-c:a", "copy"]
```

To transcode with a hardware encoder, set `recording.video_args` accordingly (and install matching drivers/ffmpeg build):

- NVIDIA: `["-c:v", "h264_nvenc"]` — GPU access in Docker via the commented `deploy.resources.reservations.devices` block in `docker-compose.yml` (requires the NVIDIA Container Toolkit).
- Intel QuickSync: `["-c:v", "h264_qsv"]`; AMD/Intel Linux VAAPI: `["-c:v", "h264_vaapi"]` — GPU access via the commented `devices: [/dev/dri:/dev/dri]` block.

Note: recording currently expects the publisher codecs the generated SDP advertises (Opus audio, VP8 video); validate against a live client before relying on it.

Client-side hardware encode/decode is negotiated by the WebRTC client, not the server: the Phase 8 client should prefer hardware-backed codecs in `RTCPeerConnection` (platform default on most browsers; on native clients pick HW-accelerated encoder implementations and match the codec list the server advertises — VP8/VP9/H.264, optional AV1 via `webrtc.enable_av1`).

## Client

A Wails v2 desktop client lives in [`client/`](client/README.md) (separate Go module `voicx/client`, importing `voicx/internal/netproto` for protocol compatibility). **Status: scaffold** — connect/auth, channel tree with speaking indicators, chat (global/channel/direct), WebRTC voice (Opus audio, VP8 video with simulcast), whisper, screen share, read-only permission grid, global PTT/mute hotkeys. Build with `cd client && wails build` (or `wails dev` for development); see the client README for details.

## Guest / anonymous login (TS3-style)

Registered accounts are only needed for permissions and persistence (admin, groups, offline spool, avatars by unique ID). Like TeamSpeak 3, anyone can connect as a guest:

- **Ephemeral guest**: `Authenticate{anonymous: true, nickname}` — no account, no key pair. The server assigns an ephemeral `guest:<random>` unique ID for the session.
- **Identity guest**: the client derives its unique ID from its own Ed25519 key pair (TS3 semantics: identity *is* the key pair) and completes the challenge handshake, presenting `public_key` in `AuthSignature`. No users row is required; the unique ID is stable across reconnects. If a users row exists for that ID, it becomes a normal registered login.

Guests are never admin, resolve to an empty permission set (all defaults: join/chat/voice allowed, create/kick/etc. denied), get no offline spool, and are never written to the users table. Ban checks (unique ID and IP) and the global server password apply to guests exactly as to registered users. Duplicate online nicknames get a `#N` suffix.

**Nickname login & key binding**: password auth accepts a nickname in place of a unique ID — the server falls back to a nickname lookup when the unique ID isn't found, returns the account's canonical unique ID, and binds the client's presented identity public key to the account (`public_key` column). Future challenge logins with that key resolve the account via the bound key even though the key-derived UID differs from the account's canonical one.

## Guest / anonymous login is also used by the tooling: `cmd/loadtest -anonymous` connects N guests with `loadtest-N` nicknames (no provisioning needed), and `cmd/e2e` covers the guest paths in its checklist.

## Versioning & releases

Every binary embeds build metadata (`internal/version`, injected via
`-ldflags -X`): **base semver + commit count + short SHA**, e.g.
`0.4.0+87.abc1234` (or `-dirty` for uncommitted changes). The base semver
lives in the root `VERSION` file; the build number is
`git rev-list --count HEAD`, so **every commit bumps the version**
automatically.

- `make build` / `make run` / `make client-build` inject it locally
  (`VERSION_FLAGS` in the Makefile).
- Docker builds take it via `--build-arg` (`.git` is excluded from the
  context; `make docker-build` and compose `build.args` pass it).
- The server logs it at startup, ServerQuery `version` returns it,
  `/healthz` reports `{"status":"ok","version":"..."}`, and Prometheus
  exports `voicx_build_info{version,commit} 1`.

**Releasing**: tag `vX.Y.Z` and push — the CI `release` job
(`.github/workflows/ci.yml`) builds the Windows client (`wails build`) and a
Linux server binary with the tag as the embedded version, generates
`checksums.txt` (SHA-256), and uploads all three to a GitHub Release. The
client's auto-updater compares against that release feed, so tag names
decide availability: a higher base semver or a higher `+build` number wins.

**Update source**: the client reads the repo slug from
`version.UpdateRepo` (ldflags `-X voicx/internal/version.UpdateRepo=<owner/repo>`;
CI sets it to `github.repository` automatically). With the placeholder
default, update checks report "no update source" and stay quiet.

## Status / roadmap

Done: control protocol end-to-end (auth, channels, permissions, chat, moderation), persistence, health endpoints, voice SFU (audio + simulcast video), recording hooks.

Later phases: SVC layer-dropping for VP9/AV1 (currently pass-through), per-codec video output tracks with renegotiation (currently a single VP8 output; publishers should send VP8), gRPC API (`:50051`, see `proto/`), file-transfer subdirectories, Redis pub/sub fan-out and rate limiting, ServerQuery compatibility layer.

## Layout

```
cmd/server/        server entrypoint
cmd/migrate/       migration runner
internal/auth/     password (Argon2id) + challenge (Ed25519) auth, ban lookup
internal/broadcast/ client fan-out, channel-tree snapshots
internal/channels/ channel lifecycle, temp-channel cleanup
internal/config/   viper-based configuration
internal/filetransfer/ token-authorized file transfer server
internal/health/   /healthz + /readyz HTTP endpoints
internal/logging/  zap logger factory
internal/netproto/ wire framing + JSON message codec
internal/permissions/ TS3-style permission model, resolver, DB loader
internal/query/    TS3-style ServerQuery admin/bot text protocol
internal/recorder/ ffmpeg-subprocess channel recording
internal/redisx/   Redis client wrapper (optional)
internal/server/   TCP control + UDP media listeners
internal/state/    in-memory client/channel/membership state
internal/store/    Postgres store + embedded migrations
internal/webrtc/   Pion WebRTC engine
proto/             protobuf definitions (package voicx.v1, future gRPC)
```
