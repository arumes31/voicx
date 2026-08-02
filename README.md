# voicx

A TeamSpeak-like voice/video server written in Go. TCP/JSON control protocol, Pion WebRTC media engine (SFU), PostgreSQL persistence, TS3-style permissions.

## Architecture

- **TCP control channel** (`:12333`) — length-prefixed binary frames carrying JSON messages (`internal/netproto`) over **TLS by default** (self-signed cert in `tls_dir`, TOFU fingerprint pinning on clients). Handles authentication, channel management, chat, kick/ban/move, and keepalive. Handlers are wired to the backends via `server.Deps` with a permission middleware (`internal/server`).
- **UDP keepalive channel** (`:12334`) — datagram listener with a bounded worker pool answering ping/pong connectivity probes (`internal/server/udp.go`). All media runs over WebRTC (DTLS-SRTP); the raw-UDP surface is deliberately limited to keepalive.
- **WebRTC engine** (`internal/webrtc`) — Pion-based SFU engine (constructed at startup; wiring into the media path is a later phase).
- **PostgreSQL** — users, channels, groups, permissions, bans, tokens, offline messages (`internal/store`, migrations in `internal/store/migrations/`).
- **Permissions** (`internal/permissions`) — TeamSpeak 3 model: five evaluation tiers (server group → client → channel client → channel → channel group), power-vs-needed-power comparisons, skip/negate flags, DB-backed loader with a 5s cache.
- **State** (`internal/state`) — goroutine-safe in-memory tracking of clients, channels, membership, and speaking state.
- **Broadcast** (`internal/broadcast`) — per-client outbound channels, non-blocking sends, channel-tree snapshots, JSON event envelopes.
- **Redis** (`internal/redisx`) — optional; reserved for pub/sub fan-out and rate limiting (later phases). The server degrades gracefully when Redis is unreachable.
- **Health** (`internal/health`) — HTTP `/healthz` (liveness) and `/readyz` (Postgres ping) on `:12337`.

## Implemented features

- Authentication: Argon2id password and Ed25519 challenge-response (TS3-style unique IDs), with an in-memory positive-result cache.
- Ban enforcement at authenticate time (unique-ID and IP bans, expiry-aware).
- Channels: create/delete, temporary/semi-permanent/permanent types, automatic cleanup of empty temporary channels, channel passwords, per-channel needed-join-power, persisted channels loaded into state at startup.
- Permission-gated operations: channel create/delete, join (power and password/ignore-password), kick (channel/server), ban, moving other clients.
- Chat: channel, global, and direct messages; direct messages to offline users are spooled (`offline_messages`) and delivered on next login.
- Server-side chat infrastructure (wave 5a): channel/global history (`chat_messages`, decrypted — the server holds scope keys; DMs are E2EE and never stored, their history is client-local), edit (`chat_edited`) and delete (own or `b_chat_delete_any`, tombstones), pinned messages (`b_channel_modify`-gated), reactions (toggle + full-map broadcasts), slow mode per channel (`slow_mode_seconds`, `b_chat_slowmode_bypass` skips), per-user rate limit (5/3s), anti-spam (identical ×3 in 30s), max length (4096 UTF-8 bytes by default), word/link filters (`chat_word_filter`, `chat_link_blacklist`, `chat_link_whitelist`; DMs exempt), @mention parsing (`@nickname`, `@channel`, `@here`/`@everyone` via `b_chat_mention_all`), typing indicators (relayed), DM delivery/read receipts (client refs), MOTD (auth response) + announcement (login + broadcast; set via ServerQuery `serverset`), custom emoji uploads (`b_emoji_manage`, `file_root/emojis/`).
- Voice SFU: WebRTC signaling over the control channel (SDP offer/answer, trickle ICE, server-initiated renegotiation on membership change), audio fan-out with per-publisher tracks (track ID = publisher client ID, mapped via MSID), whisper lists, talk-power gate, VAD speaking events (ssrc-audio-level header extension, 300ms hangover), 3D position relays.
- Priority speaker (TS3-style channel commander): `b_client_priority_speaker`-gated toggle (`MsgPrioritySpeaker`), `priority_speaker_changed` broadcast; clients duck other publishers −12 dB while a priority speaker talks (restore after 500 ms of silence).
- Echo test channel: `echo_channel_name` (default "Echo Test") is ensured to exist at startup; publishers in it hear their own audio routed back (the only channel with self-fan-out).
- Per-channel Opus quality: `opus_bitrate`/`opus_fec`/`opus_dtx`/`opus_stereo` columns (migration 005), set at create or via `MsgChannelEdit`/ServerQuery `channeledit` (`b_channel_modify`); enforced by rewriting the Opus fmtp line in SDP answers/offers per subscriber channel. Music channels (stereo + bitrate ≥ 96k) bypass the talk-power gate; the client ships Voice 32k / HQ Voice 64k / Music 128k-stereo presets.
- TURN for NAT traversal: optional `coturn` compose profile (`docker compose --profile turn up -d`); when `turn.secret` is set the server mints time-limited coturn REST credentials (`internal/turn`, 24h TTL) and delivers STUN+TURN ICE servers in the auth response.
- Video SFU: channel fan-out with per-publisher tracks, simulcast layer selection per subscriber (`high`/`mid`/`low` → RID `f`/`h`/`q` with fallback), PLI/FIR keyframe relay to publishers and on subscriber join, video-publish permission gate.
- Server-side recording: optional ffmpeg-subprocess recorder for channel audio/video (`internal/recorder`), start/stop via control message, hardware-encoder-ready argument configuration.
- Presence status: online/away/busy/invisible + status message (`MsgSetStatus`, `status_changed` broadcasts); invisible is admin-only to set and hides the user from non-admin snapshots and join/leave events (visible to admins and the user themself).
- ServerQuery: TS3-style line-based admin protocol on `:12335` (`internal/query`) for headless administration and bots — admin-only login, client/channel listing, move/kick/ban, text injection, channel create/delete/info/edit (incl. Opus quality); connection cap, idle timeout, and login brute-force lockout built in.
- File transfer: token-authorized uploads/downloads on `:12336` (`internal/filetransfer`) — single-use short-lived tokens issued over the control channel, SHA-256 integrity, per-channel quotas, per-connection bandwidth caps, per-transfer size caps, on-disk storage per channel with a `files` metadata table. Wave 7 adds: virtual folders (migration 010), rename/move and delete (`MsgFileRename`/`MsgFileDelete`, uploader or `b_ft_delete`), file versioning (N=3 rotation to `<name>.vN` on overwrite, `MsgFileVersions`), SHA-256 dedup (hard-link to the identical blob), quota state in the list response, expiring download links (`MsgFileLink` → `/dl/<token>` on the health port, 15 min), and a server icon (`MsgServerIconSet`/`MsgServerIconGet`, admin-only).
- Server password: optional global password (`server_password`, hashed at startup), enforced at authenticate time.
- Avatars & channel icons: base64 upload with type/size validation (png/jpeg/gif/webp, 256 KiB), stored under the file root; `avatar_changed`/`channel_icon_changed` events; `has_icon` in channel snapshots.
- Complaints: file complaints against users (max 5 open per reporter); manage via ServerQuery (`complaintlist`/`complaintdel`/`complaintdelall`).
- Permission/group management (wave 6a, server side): group CRUD + membership over the control channel (`GroupList`/`GroupCreate`/`GroupRename`/`GroupDelete`/`GroupAssign`/`GroupUnassign`, gated by admin or `b_server_group_manage`/`b_channel_group_manage`, deny-on-unset); permission writes on all five tiers (`PermSet`/`PermUnset` with value/grant/skip/negate, gated by `b_permission_manage`; non-admin grant cap: values ≤ own grant — TS3-lite); built-in Guest/Member/Moderator/Admin templates (`PermTemplateApply`); permission trace (`PermTrace` → winning tier + all tier entries); timed server-group memberships (`expires_in_seconds`, 60s reaper + `group_expired` event); default groups (`default_groups_enabled`: "Guest"/"Member" seeded at startup, registered users auto-join Member on first login, guests virtually hold Guest); group icons/color/hoist data; audit log (`audit_log` table; perm/group/kick/ban/token/channel actions; `AuditLog` paged, gated by admin or `b_audit_view`; ServerQuery `auditlog`). Client UI is a later wave.
- Privilege tokens: TS3-style privilege keys (`tokenadd`/`tokenlist`/`tokendelete` via ServerQuery, `TokenUse` from clients) granting server-group membership or admin; first-run bootstrap logs a one-time admin token when no admin exists.
- Screen share: declared via `ScreenShare`, relayed to channel members as `screenshare_changed`.
- Client Info (TS3-style): per-client activity tracking (connect/idle times, bytes in/out, smoothed RTT from server-initiated 15s pings), `ClientInfoQuery`/`ClientInfoResponse` — self queries always full, others' IP/port gated by admin or `b_client_remoteaddress_view` (deny-on-unset).
- Prometheus metrics on `/metrics` (health port): `voicx_clients_connected`, `voicx_channels_active`, `voicx_udp_packets_total{kind}`, `voicx_udp_packets_dropped_total`, `voicx_tcp_connections_total`, `voicx_chat_messages_total{scope}`, `voicx_webrtc_peers`, `voicx_rtp_packets_forwarded_total{media}`, `voicx_file_transfers_total{direction,result}`, plus the Go collector.
- UDP DDoS mitigation: per-source-IP token-bucket rate limiting with idle-bucket eviction (`udp_rate_limit_pps`, `udp_rate_burst`; 0 disables).
- Ping/pong keepalive; graceful shutdown of all listeners.
- Health/readiness HTTP endpoints; optional Redis connectivity check at startup.

## Ports

All product-owned listeners use the firewall-friendly `12333-12366` range.
Postgres, Redis, and public STUN endpoints keep their upstream ports.

| Port | Protocol | Purpose |
|------|----------|---------|
| 12333 | TCP+TLS | Control channel (binary JSON frames) |
| 12334 | UDP | Keepalive (ping/pong probes) |
| 12335 | TCP | ServerQuery (admin/bot text protocol) |
| 12336 | TCP | File transfer (token-authorized) |
| 12337 | TCP/HTTP | Health, readiness, metrics, download links |
| 12338 | TCP | gRPC API (loopback-only by default) |
| 12339 | TCP+SSH | ServerQuery over SSH (disabled by default) |
| 12340 | TCP/UDP | Bundled TURN listener |
| 12341 | TCP/UDP | Optional TURN TLS/DTLS listener |
| 12342-12366 | UDP | Bundled TURN relay pool |

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
| `tcp_addr` | `VOICX_TCP_ADDR` | `:12333` | Control listener address |
| `udp_addr` | `VOICX_UDP_ADDR` | `:12334` | UDP keepalive listener address |
| `grpc_addr` | `VOICX_GRPC_ADDR` | `127.0.0.1:12338` | plaintext gRPC address (loopback-only) |
| `health_addr` | `VOICX_HEALTH_ADDR` | `:12337` | Health HTTP listener address |
| `query_addr` | `VOICX_QUERY_ADDR` | `:12335` | ServerQuery listener address |
| `query_ssh_addr` | `VOICX_QUERY_SSH_ADDR` | `:12339` | ServerQuery-over-SSH listener address |
| `file_addr` | `VOICX_FILE_ADDR` | `:12336` | File-transfer listener address |

The gRPC administration API carries Basic credentials and therefore refuses
non-loopback bind addresses. Put a TLS-terminating proxy on the same host in
front of it if remote bot access is required.
| `file_root` | `VOICX_FILE_ROOT` | `./data/files` | File storage root (`<channel>/<name>`) |
| `file_max_kbps` | `VOICX_FILE_MAX_KBPS` | `0` | Per-connection bandwidth cap (KiB/s, 0=unlimited) |
| `file_channel_quota_mb` | `VOICX_FILE_CHANNEL_QUOTA_MB` | `0` | Per-channel file quota (MiB, 0=unlimited) |
| `file_max_size_mb` | `VOICX_FILE_MAX_SIZE_MB` | `100` | Per-transfer size cap (MiB, 0=unlimited) |
| `database_url` | `VOICX_DATABASE_URL` | `postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable` | Postgres DSN |
| `redis_addr` | `VOICX_REDIS_ADDR` | `localhost:6379` | Redis address |
| `redis_password` | `VOICX_REDIS_PASSWORD` | `""` | Redis password |
| `redis_enabled` | `VOICX_REDIS_ENABLED` | `true` | Set `false` to run without Redis |
| `max_clients` | `VOICX_MAX_CLIENTS` | `1024` | Maximum simultaneous clients |
| `echo_channel_name` | `VOICX_ECHO_CHANNEL_NAME` | `Echo Test` | Loopback test channel name (empty = disabled) |
| `tls_enabled` | `VOICX_TLS_ENABLED` | `true` | TLS on the control channel |
| `tls_dir` | `VOICX_TLS_DIR` | `./data/tls` | Auto-generated cert/key location |
| `tls_cert_file` | `VOICX_TLS_CERT_FILE` | `""` | Custom cert (empty = self-signed) |
| `tls_key_file` | `VOICX_TLS_KEY_FILE` | `""` | Custom key (empty = self-signed) |
| `chat_allow_plaintext` | `VOICX_CHAT_ALLOW_PLAINTEXT` | `false` | Allow unencrypted chat (dev escape hatch) |
| `chat_max_length` | `VOICX_CHAT_MAX_LENGTH` | `4096` | Max message length (UTF-8 bytes, post-decrypt) |
| `chat_rate_msgs` | `VOICX_CHAT_RATE_MSGS` | `5` | Per-user chat token bucket size |
| `chat_rate_window_seconds` | `VOICX_CHAT_RATE_WINDOW_SECONDS` | `3` | Per-user chat bucket window (s) |
| `chat_word_filter` | `VOICX_CHAT_WORD_FILTER` | `""` | Comma-separated banned substrings |
| `chat_link_blacklist` | `VOICX_CHAT_LINK_BLACKLIST` | `""` | Comma-separated blocked link substrings |
| `chat_link_whitelist` | `VOICX_CHAT_LINK_WHITELIST` | `""` | If set, only these domains may be linked |
| `default_groups_enabled` | `VOICX_DEFAULT_GROUPS_ENABLED` | `true` | Seed Guest/Member server groups and auto-assign them at login |
| `turn.secret` | `VOICX_TURN_SECRET` | `""` | coturn static-auth-secret (empty = TURN disabled) |
| `turn.realm` | `VOICX_TURN_REALM` | `voicx` | TURN realm |
| `turn.uris` | `VOICX_TURN_URIS` | `[]` | Client TURN URIs (comma-separated via env) |
| `turn.credentials_ttl` | `VOICX_TURN_CREDENTIALS_TTL` | `24h` | TURN credential lifetime |
| `db_max_open_conns` | `VOICX_DB_MAX_OPEN_CONNS` | `25` | Postgres pool size |
| `db_max_idle_conns` | `VOICX_DB_MAX_IDLE_CONNS` | `5` | Postgres idle pool size |
| `db_conn_max_lifetime` | `VOICX_DB_CONN_MAX_LIFETIME` | `30m` | Postgres connection lifetime |
| `pii_key_file` | `VOICX_PII_KEY_FILE` | `./data/keys/pii.key` | First-start AES-256-GCM key for protected PII columns; keep outside PostgreSQL |
| `client_timeout_seconds` | `VOICX_CLIENT_TIMEOUT_SECONDS` | `90` | Disconnect inactive control clients after this many seconds |
| `default_opus_bitrate` | `VOICX_DEFAULT_OPUS_BITRATE` | `32000` | Default bitrate for newly created channels |
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

Pool tuning is deliberately not capped: raise `db_max_open_conns` as the database is scaled up. Use the exported `voicx_db_pool_*` metrics to watch in-use connections and wait time, and run `DATABASE_URL=... make profile-db` to capture `EXPLAIN (ANALYZE, BUFFERS)` plans for the hot chat and permission queries before and after a change.

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
go run ./cmd/e2e -tls-insecure \
  -alice-uid <alice uid> -alice-pass alicepw \
  -bob-uid <bob uid> -bob-pass bobpw \
  -admin-uid <admin uid> -admin-pass adminpw
# exit code 0 only when all checks pass
```

Note: authentication uses the **unique ID** printed by adduser, not the nickname. Ports default to localhost standard values; override with `-addr`, `-query-addr`, `-health-url`, `-udp-addr`, `-file-addr`, and `-server-password` if the server has a global password. Since the control channel is TLS by default (self-signed cert), e2e and loadtest need `-tls-insecure` (skips certificate verification, logs the presented fingerprint once); omit it only against a `tls_enabled: false` server.

### Database chaos drill (467)

`-chaos` adds one extra, deliberately destructive check to the end of the checklist: it stops PostgreSQL while traffic is in flight and verifies the server degrades and recovers instead of falling over. It needs Docker (or whatever command you point it at) and is never part of a normal run.

```bash
export E2E_ALICE_UID=<alice uid> E2E_ALICE_PASS=alicepw
export E2E_BOB_UID=<bob uid>     E2E_BOB_PASS=bobpw
export E2E_ADMIN_UID=<admin uid> E2E_ADMIN_PASS=adminpw
make chaos                       # or: ./scripts/chaos-db.sh
# equivalent direct invocation:
go run ./cmd/e2e -chaos
```

The whole checklist runs first, because the drill uses the channel it creates. With two authenticated sessions live and a third generating traffic (a ping every 200 ms, an encrypted channel message every second — slower than the pings so the 5-per-3s chat rate limit cannot be mistaken for a backend failure), it asserts:

1. `/readyz` stops reporting ready within 10s while `/healthz` stays 200 — liveness must not follow the database. (The server answers **500**, not the more conventional 503; the check accepts either.)
2. The established TCP sessions stay open and their pings keep being answered throughout the outage.
3. A DB-backed request (channel history) comes back as an **error frame** on a live connection — not a dropped connection, a hang, or a panic. `handleChatHistory` and the chat-send store path both answer `errCodeUnavailable`, so the failure is visible to the client and the session survives.
4. After the restart, `/readyz` returns 200 within 30s, a fresh `dialAuth` succeeds, and a message sent afterwards is retrievable through a history query — i.e. writes reach storage again, not just the relay.

Override the disruption with `-chaos-stop-cmd` / `-chaos-start-cmd` (or `CHAOS_STOP_CMD` / `CHAOS_START_CMD`) to target a non-compose database; the command line is split on whitespace and run **without a shell**, so quoting and shell operators are not supported. The drill always tries to restart the database, including when a step fails.

## Load testing

`cmd/loadtest` is a headless client simulator (TCP auth, channel join, chat, ping, optional UDP pings, and real Pion Opus publishers):

```bash
go run ./cmd/loadtest -addr 127.0.0.1:12333 -clients 50 -duration 30s -ramp 5s \
    -unique-id <uid> -password <pw> [-channel 1] [-udp -udp-addr 127.0.0.1:12334]
```

All simulated clients share one account (voicx allows multiple connections per unique ID). Registration is not exposed over the protocol — create the test user directly (e.g. a one-off snippet calling `auth.RegisterUser`, or via psql). The report prints connect/auth counts, failures, an auth-latency histogram, and message counts.

`make webrtc-load LOADTEST_ARGS='-anonymous -tls-insecure -channel 1'` runs the 100-publisher SFU profile. `make chaos-webrtc` forces the same media through TURN/TCP and Toxiproxy, adding latency, jitter, and periodic transport-loss bursts; it requires an explicit `TURN_SECRET` and `LOADTEST_ARGS`. `make query-load` drives persistent ServerQuery sessions at a target 5,000 requests/sec. `make canary` runs key-rotation, skipped-key, signature-tamper, and plaintext-leakage canaries.

## CI/CD

`.github/workflows/ci.yml` runs on every push/PR: gofmt gate, `go vet`, `go build`, and `go test -race` against Postgres and Redis service containers (DB-backed tests run via `VOICX_TEST_DATABASE_URL`). Tags additionally build and push the Docker image to GHCR via `docker/build-push-action` (default `GITHUB_TOKEN`).

## Security notes

- **Encryption at a glance**: the TCP control channel is TLS by default (`tls_enabled`; self-signed ECDSA P-256 cert auto-generated into `tls_dir` on first start, 10y validity, SANs localhost + server name; SHA-256 fingerprint logged at startup, sent in the auth response, and shown in ServerQuery `serverinfo`). The Wails client pins the fingerprint TOFU-style in `known_servers.json` — a later mismatch hard-fails with a warning dialog until the user explicitly trusts the new fingerprint. Voice/video media is encrypted by WebRTC's DTLS-SRTP (mandated by the spec, not optional). **Still plaintext this wave**: ServerQuery (`:12335`, backlog 225) and file transfer (`:12336`, token-authorized) — keep them on trusted networks.
- **Chat payload encryption (wave 4b) — exact trust model**:
  - **Direct messages: true E2EE.** The live compatibility envelope remains X25519 + XSalsa20-Poly1305 while `internal/e2ee` supplies the versioned X3DH, signed/one-time prekey, AES-256-GCM per-message HKDF chain, Double Ratchet and 2,000 skipped-key machinery for negotiated sessions. **The server relays and spools ciphertext it cannot read.** Session diagnostics state which envelope a peer actually negotiated; capability must not be mistaken for an active ratchet.
  - **Channel + global messages: encrypted with server-held symmetric keys** (nacl/secretbox, XSalsa20-Poly1305). Scope generations are persisted wrapped under the external chat master key and distributed **sealed** to each member's X25519 key. The server CAN read these scopes; that's deliberate: history, search, and moderation require it. `internal/e2ee.SenderKey` provides the O(N) sender-key construction for private group-session negotiation.
  - **Rotation**: a channel's key rotates (bumped `key_id`) when a member leaves, and is redistributed to remaining members — ex-members can't read *new* messages. They keep access to history from their membership period (documented limitation). The global key does not rotate (it would re-key every client on every disconnect).
  - **Plaintext chat is rejected** by default (`chat_allow_plaintext: false`, dev escape hatch). The server validates scope `key_id` and ciphertext size but never inspects DM bodies.
- The container image runs as a non-root user (uid 10001); the compose service additionally sets `no-new-privileges`, drops all capabilities, mounts the root filesystem read-only, and keeps writable data (uploads, avatars, icons, recordings, TLS keys) on a named volume at `/data`.
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

## TURN (NAT traversal)

TURN is only needed for clients behind restrictive NATs (symmetric NAT, UDP-blocking firewalls); LAN and direct-WAN deployments can skip it entirely. To enable:

1. Put **all four** variables in `.env` — `TURN_SECRET`/`TURN_REALM` configure the coturn container, `VOICX_TURN_SECRET`/`VOICX_TURN_REALM` configure the server, and they must match. Setting only one pair leaves coturn on its placeholder secret while the server has TURN disabled, and the mismatch is silent (445).
2. Set `TURN_EXTERNAL_IP` to the public IP clients reach the host on. **This is required for every client outside the host**: coturn runs on the `voicx-net` bridge, so without `--external-ip` it discovers only its `172.x` container address and advertises relay candidates nobody can reach — the ports being published does not help, because the address in the candidate is wrong. Leave it empty only for host-local testing.
3. Start coturn: `docker compose --profile turn up -d` (publishes 12340 tcp/udp and the relay range 12342-12366/udp).
4. Point the server at it: `VOICX_TURN_URIS=turn:<host>:12340?transport=udp,turn:<host>:12340?transport=tcp`.

The server then mints time-limited credentials per client (`internal/turn`, coturn REST API: `username = <expiry>:<uid>`, `credential = base64(HMAC-SHA1(secret, username))`, TTL `turn.credentials_ttl`, default 24h) and delivers them together with the STUN defaults in the auth response; clients merge them into their `RTCPeerConnection` automatically.

**`turns:` (TLS/DTLS) is off by default** and the compose file does not advertise port 12341, because a working `turns:` needs a real certificate for the TURN hostname that this repo cannot ship — advertising the port without one gives clients an endpoint that always fails. To enable it: obtain a certificate for the TURN hostname (e.g. certbot), place `fullchain.pem`/`privkey.pem` in `./data/turn-certs`, uncomment the cert volume and the 12341 port mappings in the coturn service, replace `--no-tls`/`--no-dtls` with `--tls-listening-port=12341 --cert=/etc/coturn/certs/fullchain.pem --pkey=/etc/coturn/certs/privkey.pem`, and append `turns:<host>:12341?transport=tcp` to `VOICX_TURN_URIS`.

**Known gap (445):** TURN credentials are minted once during authentication and never refreshed for a live session. A session that outlives `turn.credentials_ttl` (default 24h) presents expired credentials on a later ICE restart, and relayed media fails to re-establish until the client reconnects. Shortening the TTL makes this *more* likely, not less.

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

Later phases: SVC layer-dropping for VP9/AV1 (currently pass-through), per-codec video output tracks with renegotiation (currently a single VP8 output; publishers should send VP8), file-transfer subdirectories, Redis pub/sub fan-out and rate limiting, ServerQuery compatibility layer.

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
internal/server/   TCP control + UDP keepalive listeners
internal/state/    in-memory client/channel/membership state
internal/store/    Postgres store + embedded migrations
internal/webrtc/   Pion WebRTC engine
proto/             protobuf definitions (package voicx.v1, future gRPC)
```
