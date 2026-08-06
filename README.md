<div align="center">

# 🎙️ VoicX

**Next-Generation High-Performance Real-Time Communication Platform**

*Ultra-low latency SFU voice & video engine, zero-trust E2EE chat messaging, PostgreSQL multi-tenant state persistence, and a 5-tier role-based permission system.*

[![CI](https://github.com/arumes31/voicx/actions/workflows/ci.yml/badge.svg)](https://github.com/arumes31/voicx/actions/workflows/ci.yml)
[![golangci-lint](https://github.com/arumes31/voicx/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/arumes31/voicx/actions/workflows/golangci-lint.yml)
[![Security Analysis](https://github.com/arumes31/voicx/actions/workflows/security.yml/badge.svg)](https://github.com/arumes31/voicx/actions/workflows/security.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/arumes31/voicx)](https://go.dev/)
[![Docker Image](https://img.shields.io/docker/v/arumes31/voicx?label=ghcr.io&logo=docker)](https://github.com/arumes31/voicx/pkgs/container/voicx)
[![License](https://img.shields.io/github/license/arumes31/voicx)](LICENSE)

[Architecture](#-system-architecture) • [Features](#-key-features) • [Quick Start](#-quick-start) • [Permissions](#-5-tier-permission-engine) • [ServerQuery API](#-serverquery-admin-protocol) • [Configuration](#-configuration-reference)

---

</div>

## 🌟 Overview

**VoicX** is an enterprise-grade, self-hosted real-time communication platform written in Go. Designed for high concurrency and operational clarity, VoicX couples a lightweight binary control protocol with a **Pion WebRTC SFU engine** for sub-100ms multi-party audio/video fan-out, end-to-end encrypted messaging, and granular administrative control.

> [!NOTE]
> **Zero-Trust Security**: Direct messages are fully E2EE using X25519 Double-Ratchet key agreements. The server stores only channel history under persisted scope keys; direct message bodies never hit the server database in plaintext or unwrapped ciphertext.

---

## 🏗️ System Architecture

```mermaid
graph TD
    subgraph Clients["Clients"]
        Wails["Wails Desktop Application\n(Windows / Linux / macOS)"]
        WebUI["Web Browser Client\n(HTML5 / WebRTC / ES6)"]
        Bot["ServerQuery Bot / CLI\n(TCP Telnet / SSH)"]
    end

    subgraph CoreServer["VoicX Server Core"]
        Control["TCP Control Listener\n:12333 (TLS / TOFU)"]
        Keepalive["UDP Keepalive Worker Pool\n:12334"]
        WebRTC["Pion WebRTC SFU Engine\n(DTLS-SRTP / ICE / Opus)"]
        Query["ServerQuery Admin Protocol\n127.0.0.1:12335 (Raw) / :12339 (SSH opt-in)"]
        FileXfer["File Transfer Service\n:12336 (TLS 1.3 / Token Authorized)"]
        Health["Health & Metrics Service\n:12337 (/healthz, /readyz)"]
    end

    subgraph DataStore["Persistence & Messaging"]
        Postgres[(PostgreSQL 16\nStore, State, Audit Logs)]
        Redis[(Redis 7\nPub/Sub & Rate Limiting)]
    end

    Wails <-->|TLS Control JSON| Control
    Wails <-->|UDP WebRTC Media| WebRTC
    WebUI <-->|WebSockets / WebRTC| WebRTC
    Bot <-->|TCP Text Commands| Query
    Wails <-->|Token Upload/Download| FileXfer

    Control --> Postgres
    Control --> Redis
    Query --> Postgres
    WebRTC --> Control
```

### Protocol Specifications

| Protocol Port | Service Component | Wire Format / Transport | Security & Auth |
| :--- | :--- | :--- | :--- |
| **`TCP :12333`** | Control Engine | Length-prefixed JSON frames over TLS 1.3 | Ed25519 Challenge / Argon2id / TOFU Pinning |
| **`UDP :12334`** | Connection Probes | Datagram Ping/Pong Keepalive | Session Token Verification |
| **`UDP Dynamic`** | WebRTC SFU Engine | DTLS-SRTP (Opus audio, H.264/VP8 video) | ICE candidate negotiation & SRTP encryption |
| **`TCP 127.0.0.1:12335`** | ServerQuery Protocol | Line-based ASCII / UTF-8 plaintext stream | Loopback by default; remote binding requires explicit opt-in, and SSH is preferred |
| **`TCP :12336`** | File Transfer Engine | Binary frames over TLS 1.3 | TOFU-pinned certificate plus an ephemeral single-use token |
| **`TCP :12337`** | Health & Prometheus | HTTP GET (`/healthz`, `/readyz`, `/metrics`) | Liveness/readiness follow the listener bind; metrics are loopback-only unless explicitly enabled |

---

## ✨ Key Features

### 🎙️ Sub-100ms Voice & Video SFU
* **Pion WebRTC SFU**: Zero-copy packet fan-out supporting hundreds of concurrent speakers.
* **Opus Codec Optimization**: Dynamic SDP fmtp line rewriting per channel for variable bitrate (16–128 kbps), Forward Error Correction (FEC), and Discontinuous Transmission (DTX).
* **Simulcast Video**: Dynamic quality tier selection (`high`, `mid`, `low` RID layers) based on subscriber network conditions.
* **Priority Commander**: Automatic audio ducking (−12 dB attenuation) across non-priority channels when a Priority Speaker talks.
* **Whisper Routing**: Point-to-point and cross-channel targeted voice transmission bypasses standard channel boundaries.

### 💬 End-to-End Encrypted & Scope-Keyed Messaging
* **True E2EE Direct Messaging**: Signal-style X25519 prekey bundles with Double-Ratchet forward secrecy.
* **Channel Scope Key Rotation**: Channel message bodies are sealed with scope keys; server stores ciphertext and manages scope key generations.
* **Rich Messaging Controls**: Channel history search, pinned messages, emoji reactions, typing indicators, read receipts, and `@mention` notifications.
* **Automated Moderation**: Regex link whitelisting/blacklisting, duplicate message suppression, rate limiting, and word filtering.

### 🛡️ 5-Tier Hierarchical Permission Engine

```mermaid
flowchart TD
    Tier1["Tier 1: Server Group (Lowest)"] --> Tier2["Tier 2: Client Permissions"]
    Tier2 --> Tier3["Tier 3: Channel Client Overrides"]
    Tier3 --> Tier4["Tier 4: Channel Permissions"]
    Tier4 --> Tier5["Tier 5: Channel Group (Highest)"]

    Tier5 --> Eval{"Evaluate Skip & Negate Flags"}
    Eval --> Result["Final Granted / Denied Power"]
```

* **5 Evaluation Tiers**: Server Group → Client → Channel Client → Channel → Channel Group.
* **Skip & Negate Semantics**: Prevent lower-level channel groups from overriding critical server-wide bans or moderation flags.
* **Non-Admin Grant Capping**: Delegated moderators can only assign permission values less than or equal to their own grant power.
* **Detailed Audit Logging**: Every group creation, assignment, permission mutation, kick, ban, and token redemption is appended to an immutable database audit log.

---

## ⚡ Quick Start

> [!TIP]
> The fastest way to run VoicX is using **Docker Compose**.

### Option 1: Docker Compose (Recommended)

1. Clone the repository:
   ```bash
   git clone https://github.com/arumes31/voicx.git
   cd voicx
   ```

2. Create an explicit local-development environment, then launch PostgreSQL,
   Redis, and the server:
   ```bash
   cp .env.example .env
   docker compose up -d
   ```

   The sample environment is for host-local development. Before exposing a
   deployment, set `VOICX_DEV_MODE=false`, replace the sample PostgreSQL
   credential, and set `VOICX_COMPOSE_DATABASE_URL` with `sslmode=require`,
   `verify-ca`, or `verify-full`. Production startup rejects the sample
   credential and plaintext database transport.

3. View initial startup log (includes the generated **Admin Privilege Token**):
   ```bash
   docker compose logs -f voicx
   ```

### Option 2: Building from Source

#### Prerequisites
* **Go**: `>= 1.25`
* **Node.js**: `>= 24`
* **Wails CLI**: `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
* **PostgreSQL**: `>= 16`

#### Build Backend Server
```bash
# Build the standalone server binary
go build -o bin/voicx-server ./cmd/server

# Run migrations and start server
VOICX_DATABASE_URL="postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable" ./bin/voicx-server
```

#### Build Desktop Client
```bash
cd client
wails build
```

---

## ⚙️ Configuration Reference

VoicX can be configured via environment variables or a YAML configuration file (`config.yaml`).

| Environment Variable | Default Value | Description |
| :--- | :--- | :--- |
| `VOICX_TCP_ADDR` | `:12333` | Primary control TCP listener address |
| `VOICX_UDP_ADDR` | `:12334` | UDP keepalive ping/pong listener address |
| `VOICX_GRPC_ADDR` | `127.0.0.1:12338` | Plaintext gRPC administration listener; loopback is mandatory |
| `VOICX_QUERY_ADDR` | `127.0.0.1:12335` | ServerQuery admin protocol binding address |
| `VOICX_QUERY_ALLOW_REMOTE` | `false` | Explicitly permit a non-loopback raw ServerQuery bind; prefer SSH instead |
| `VOICX_QUERY_SSH_ENABLED` | `false` | Enable the SSH-wrapped ServerQuery listener |
| `VOICX_QUERY_SSH_ADDR` | `:12339` | SSH ServerQuery listener address |
| `VOICX_FILE_ADDR` | `:12336` | File transfer upload/download listener address |
| `VOICX_HEALTH_ADDR` | `:12337` | Health/readiness and metrics HTTP listener |
| `VOICX_METRICS_ALLOW_REMOTE` | `false` | Permit remote `/metrics` requests; without this opt-in, only IPv4/IPv6 loopback is accepted |
| `VOICX_DATABASE_URL` | `postgres://...` | PostgreSQL connection URL |
| `VOICX_REDIS_ADDR` | `localhost:6379` | Optional Redis address for pub/sub fanout |
| `VOICX_TLS_ENABLED` | `true` | Enable TLS 1.3 encryption on control port |
| `VOICX_TLS_DIR` | `./data/tls` | Directory storing the generated TLS certificate and key |
| `VOICX_TLS_CERT_FILE` / `VOICX_TLS_KEY_FILE` | empty | Custom certificate and key; both must be configured together |
| `VOICX_FILE_TLS_ENABLED` | `true` | Enable TLS 1.3 on file transfers; disabling is development-only |
| `VOICX_FILE_ROOT` | `./data/files` | Root storage path for uploaded channel files & avatars |
| `VOICX_PII_KEY_FILE` | `./data/keys/pii.key` | AES-256-GCM master key file path for PII encryption |
| `VOICX_CHANNEL_TEMP_LIFETIME_SECONDS` | `60` | Grace period before an empty temporary channel is removed |
| `VOICX_CHAT_MASTER_KEY_FILE` | `./data/keys/chat_master.key` | KEK file used to wrap persisted chat scope keys; back it up with PostgreSQL |
| `VOICX_CHAT_LEGACY_HISTORY` | `encrypt` | One-time handling for legacy plaintext rows: `encrypt` or `purge` |
| `VOICX_CHAT_KEY_ROTATE_MIN_SECONDS` | `60` | Minimum interval used to coalesce scope-key rotations |
| `VOICX_CHAT_SEARCH_MAX_MESSAGES` | `2000` | Maximum history messages scanned by client-side search |
| `VOICX_CHAT_MAX_LENGTH` | `4096` | Maximum decrypted chat payload size in UTF-8 bytes |
| `VOICX_DEFAULT_GROUPS_ENABLED` | `true` | Auto-create and assign the built-in Guest and Member groups |
| `VOICX_TURN_CREDENTIALS_TTL` | `24h` | TURN credential lifetime; must be positive and at most 30 days |

### Certificate trust and rotation

The generated certificate under `VOICX_TLS_DIR` is the server's persistent
identity. Back up that directory with the server data volume; replacing or
losing it changes the fingerprint seen by every client.

On first connection, the desktop client pins the control certificate's SHA-256
fingerprint. Later changes fail closed. For a planned rotation:

1. Generate or install the new certificate and record the fingerprint printed
   by the server at startup.
2. Verify that fingerprint with users over a separate trusted channel.
3. Reconnect. In the certificate-changed warning, compare the presented value
   with the verified value and choose **Trust new fingerprint** only when they
   match.
4. Reconnect once more and confirm the connection-security message reports the
   expected fingerprint. If verification fails, abort and restore the previous
   certificate and key; do not delete `known_servers.json` to bypass the check.

---

## 💻 ServerQuery Admin Protocol

VoicX exposes a line-based administrative text interface on `127.0.0.1:12335`
for host-local automation. The raw protocol is plaintext: a non-loopback bind is
rejected unless `VOICX_QUERY_ALLOW_REMOTE=true` is set explicitly. For remote
administration, enable the SSH transport on port `12339` instead. Docker Compose
does not publish either administration port by default; publish `12339` when
enabling Query SSH.

### Key ServerQuery Commands

| Command | Arguments | Description |
| :--- | :--- | :--- |
| `login` | `<username> <password>` | Authenticate as ServerQuery administrator |
| `clientlist` | `[-uid] [-times] [-voice]` | List all connected clients with state metadata |
| `channellist` | `[-topic] [-flags] [-limits]` | List all active channels and configuration |
| `clientmove` | `clid=<id> cid=<target_cid>` | Move a connected client to another channel |
| `clientkick` | `clid=<id> reason=<text>` | Kick client from current channel or server |
| `banadd` | `[uid=<uid>] [ip=<ip>] time=<sec>` | Create an identity or IP address ban rule |
| `permset` | `permid=<key> val=<value>` | Modify permission value for a group or client |
| `tokenadd` | `tokentype=<0|1> id=<group_id>` | Generate a single-use privilege token |

<details>
<summary><b>Click to expand ServerQuery session example</b></summary>

```bash
$ telnet 127.0.0.1 12335
VoicX ServerQuery
welcome to VoicX ServerQuery
login admin secretpass
error id=0 msg=ok
channellist
cid=1 channel_name=Default\sChannel total_clients=3|cid=2 channel_name=Lounge total_clients=0
error id=0 msg=ok
clientmove clid=4 cid=2
error id=0 msg=ok
```

</details>

---

## 📂 Project Structure

```
voicx/
├── client/                     # Desktop Client (Wails v2 / Go + ES6 UI)
│   ├── desktop_windows.go      # Windows COM thread affinity & tray setup
│   ├── frontend/               # Single-page UI (Vite / ES6 / Modular CSS)
│   ├── hotkeys.go              # Global hotkey registration engine
│   └── ptt_windows.go          # Win32 Virtual Key low-level PTT observer
├── cmd/
│   ├── server/                 # Standalone VoicX Server entrypoint
│   └── migrate/                # Standalone DB migration utility
├── internal/
│   ├── auth/                   # Ed25519 challenge & Argon2id authentication
│   ├── broadcast/              # Outbound message fanout & snapshot engine
│   ├── channels/               # Active channel tree manager
│   ├── e2ee/                   # Signal-style X25519 Double-Ratchet crypto
│   ├── filetransfer/           # Token-authenticated file pipeline
│   ├── netproto/               # Binary frame codec & JSON message definitions
│   ├── permissions/            # 5-tier permission evaluation engine
│   ├── query/                  # ServerQuery line-based admin protocol
│   ├── store/                  # PostgreSQL data access layer & migrations
│   └── webrtc/                 # Pion WebRTC SFU engine & Opus mixer
├── .github/
│   └── workflows/              # GitHub Actions (CI, Security, Docker, Lint)
├── Dockerfile                  # Multi-stage production container image
├── docker-compose.yml          # Production Docker stack
└── README.md                   # System documentation
```

---

## 🔒 Security & Vulnerability Reporting

VoicX is engineered around a strict security posture:
- **Challenge Authentication**: Public key cryptography prevents password sniffing over untrusted networks.
- **Strict TOFU Certificate Pinning**: Clients pin self-signed TLS certificates on first connect.
- **PII Storage Protection**: Sensitive user metadata columns are encrypted at rest with AES-256-GCM authenticated data.
- **Privilege Capping**: Non-administrators cannot grant permissions exceeding their own delegated grant tier.

---

## 📄 License

This project is licensed under the **MIT License**. See the [LICENSE](LICENSE) file for complete details.
