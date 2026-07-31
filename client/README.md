# voicx-client

Wails v2 desktop client for the voicx voice/chat server (see the repository
root README for the server). **Status: scaffold** — the core flows work end
to end (connect/auth, channel tree, chat, voice over WebRTC, permissions
grid); polish (recording UI, complaint UI) comes later.

## UI

A dark "voice ops console" (vanilla JS/CSS, fonts bundled locally via
fontsource — fully offline in WebView2):

- **Menu bar** (TS3-style): Connections (Connect/Disconnect/Quit), Bookmarks
  (save/connect/manage server bookmarks — passwords are never stored), Self
  (nickname, avatar, mute/deafen), Permissions, Tools (Settings, Whisper
  lists), Help (About, Open log folder).
- **Settings dialog** (Tools → Settings…, left icon nav): Application (chat
  lines, toasts, reconnect-on-loss), Capture (input device, PTT/VAD/
  continuous activation, VAD threshold, EC/NS, live mic test meter),
  Playback (output device, volume, test sound), Hotkeys (key capture fields
  + rebind at runtime), Whisper (list editor), Downloads (folder picker —
  download UI itself is future work), Chat (max lines, per-scope file
  logging), Security (identity manager: UID display/copy, export/import,
  regenerate), Notifications (toast + synthesized sound toggles).
- **Left** — wordmark + connection pill, channel tree with nesting indent
  lines, users with avatar circles (initials fallback) and a pulsing
  speaking glow ring.
- **Center** — voice control bar (large hold-to-talk mic button with glow
  when live, mute/screen-share buttons, hotkey status), a full-width
  **● TALKING banner** whenever you are transmitting, and chat with scope
  tags, timestamps, and centered system lines.
- **Right** — selected-user card and the resolved-permissions grid.
- Signal green (`#2ee6a8`) is used *only* for voice activity so talk state
  is scannable at a glance; warnings are amber.

Settings persist to `<UserConfigDir>/voicx/settings.json` (identity to
`identity.json`, logs to `client.log` / `chat.log` in the same folder).

**No-mic machines**: `getUserMedia` failures degrade to a video-only voice
join with an inline "No microphone found" / "Mic access denied" state (PTT
disabled), one system chat line — screen sharing still works.

Toasts (top-right) fire for channel join/leave, kicks, hotkey registration
failures, and connection loss.

## Architecture

- **Go backend** (`app.go`, `conn.go`, `hotkeys.go`) owns all protocol
  state. It speaks the voicx control protocol (`voicx/internal/netproto` —
  this module imports it via `require voicx v0.0.0` + `replace voicx => ../`)
  and exposes a bound API to the frontend. Server traffic (snapshot, events,
  chat, ICE) is pushed to the UI as Wails runtime events.
- **Frontend** (`frontend/`) is deliberately vanilla JS + CSS (no React —
  no build risk, no runtime deps). Built with Vite.
- **WebRTC** runs in the WebView2 (Chromium) page: `getUserMedia` +
  `RTCPeerConnection`, with SDP offer/answer and ICE candidates bridged
  through the Go backend over the control channel. Hardware encode/decode is
  browser-managed (Chromium picks HW acceleration automatically when
  available; there is no JS flag to force it).

## Bound API (Go backend)

`Connect`, `Disconnect`, `Connected`, `JoinChannel`, `SendChat`
(global/channel/direct), `WhisperSet`, `GetPermissions` (resolved permission
set via `MsgPermissionsQuery`), `WebRTCOffer`/`WebRTCAnswer`/
`SendICECandidate`, `SetScreenShare`, `SetMuted`, `SetPTT`, `SetAvatar`.

## Hotkeys

- **Space** — push-to-talk (held = talk; ignored while the chat input is
  focused).
- **Ctrl+M** — mute toggle.

Registration failures (platform limits, conflicts) are logged and the hotkey
is disabled; the app keeps working. Change the bindings in `hotkeys.go`.

## Run

Prereqs: Go 1.25+, Node 24+, Wails CLI v2 (`go install
github.com/wailsapp/wails/v2/cmd/wails@latest`).

```bash
# Start a local server first (repo root): postgres required
#   go run ./cmd/server

# Dev mode with live frontend reload
wails dev

# Production build (frontend + binary)
wails build        # output: build/bin/voicx-client.exe
```

Connect from the login dialog: server address (`127.0.0.1:10011`), your
unique ID, account password, and the server password if the server has one
set.

## Test account

There is no protocol-level registration. Create a user directly (from the
repo root):

```go
// one-off: go run ./cmd/migrate first, then a tiny snippet or psql INSERT
// using auth.RegisterUser equivalent. Easiest: psql.
```

Or insert via psql (Argon2id hash from `auth.HashPassword`), or redeem the
first-run admin token the server logs at WARN on an empty database.

## Identity & login

TS3-style identity: on first run the client generates an Ed25519 key pair
and persists it to `<UserConfigDir>/voicx/identity.json` (0600). The unique
ID is derived from the public key — you never type one.

The login dialog asks for:

- **Server** — host:port of the control channel.
- **Nickname** — your account nickname (or unique ID). With a **password**
  this is an account login: the server resolves the nickname, returns the
  account's canonical unique ID, and binds your identity key to the account
  so future logins work passwordless (challenge auth with the same key).
- **Password** — optional. Empty = guest login with your own identity
  (key-derived unique ID, TS3 guest semantics).
- **Server password** — only if the server has a global password set.

## Hotkeys & troubleshooting

- **Space** — push-to-talk (works in the background; while the chat input is
  focused in the voicx window, Space types instead). Also hold the big mic
  button in the voice bar for mouse PTT. Rebindable in Settings → Hotkeys
  (any key spec like `F5`, `Ctrl+M`, `Ctrl+Shift+F5`).
- **Ctrl+M** — mute toggle (also rebindable).

The big mic button glows and the **● TALKING** banner appears while
push-to-talk is active. The keyboard icon in the voice bar shows global
hotkey status (`⌨ ptt` = registered, `⌨ off` = failed). If a hotkey shows
off: check `<UserConfigDir>/voicx/client.log` — the usual cause is a second
client instance already holding the global hotkey. Debug lines like
`hotkey ptt_down fired` are logged there for every captured event.

## Headless backend test

`conn_live_test.go` verifies the Go backend (connect/auth, snapshot, channel
join + `user_moved`, channel chat, `GetPermissions`) against a **live**
server — no Wails runtime needed (the backend's events go through an
`eventSink` seam; tests install a recorder). It skips unless
`VOICX_LIVE_ADDR` is set:

```bash
# server must be running (e.g. docker compose up)
cd client
VOICX_LIVE_ADDR=127.0.0.1:10011 go test -run Live -v ./... -count=1
VOICX_LIVE_ADDR=127.0.0.1:10011 go test -race -run Live ./... -count=1
```

Optional env: `VOICX_LIVE_QUERY_ADDR` (ServerQuery port, default
`<host>:10012`). The test creates a throwaway permanent channel via
ServerQuery (admin account) so channel-dependent assertions are real. Note
`GetPermissions` returns an empty set on a fresh server — registered users
have no granted permissions until seeded; the test asserts the
request/response round-trip works.

## Notes / limitations

- The frontend is a scaffold: channel icons and avatars are stored/flagged
  but not rendered yet; permission editing is read-only.
- Screen sharing replaces the camera track (one video track per peer —
  server limitation, documented in the root README).
- `wails build` was verified on Windows 11 + WebView2 runtime. On other
  platforms install the Wails platform prerequisites (see
  https://wails.io/docs/gettingstarted/installation).
