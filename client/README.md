# voicx-client

Wails v2 desktop client for the voicx voice/chat server (see the repository
root README for the server). **Status: scaffold** — the core flows work end
to end (connect/auth, channel tree, chat, voice over WebRTC, permissions
grid); polish (recording UI, complaint UI) comes later.

## UI

A dark "voice ops console" (vanilla JS/CSS, fonts bundled locally via
fontsource — fully offline in WebView2):

- **Client Info** — right-click any user in the channel tree for a context
  menu (Client Info, private message, copy unique ID). The TS3-style dialog
  shows connection/idle time (ticking), ping (or `unknown`), client address
  (IP only for self or with `b_client_remoteaddress_view`), and transfer
  stats — live-refreshing every 2s.
- **Channel edit** — right-click a channel → Edit channel: topic, max
  clients, and a quality preset select (Voice 32 kbps / HQ Voice 64 kbps
  +FEC / Music 128 kbps stereo / Custom) pre-filling bitrate and the
  FEC/DTX/Stereo flags. Gated server-side by `b_channel_modify`; the result
  arrives as a `channel_updated` broadcast that refreshes the tree.
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
  when live, mute/screen-share/priority/low-bandwidth buttons, hotkey
  status), a full-width **● TALKING banner** whenever you are transmitting,
  the **video grid** (appears when publishers stream video; chat stays
  below), and chat with scope tags, timestamps, and centered system lines.
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

## Voice UX (wave 1)

- **Per-user volume & local mute** — right-click a user → Volume slider
  (0–200%) or Mute locally; persisted per unique ID in `settings.json`.
  The server emits one audio track per publisher (track ID = publisher
  client ID, carried in the MSID), and each track gets its own gain+mute
  node pair in the shared WebAudio chain, so per-sender volume/mute is
  audible.
- **Mic level meter** in the voice bar while voice is joined (green/amber/
  red gradient); **mic test with loopback playback** (Capture settings);
  **VAD auto-calibrate** (5 s ambient → noise floor → suggested threshold).
- **PTT release delay** slider (0–2000 ms) so sentence ends aren't clipped.
- **Own status icons** in the tree (muted / deafened / screen sharing) and a
  local-mute icon on users you muted.
- **Talking-while-muted warning** (amber banner "You're muted!",
  rate-limited, click to dismiss) and **talking-to-empty-channel hint** —
  both toggleable in Settings → Notifications.
- **Sound pack system** (Settings → Notifications): soft/bright/retro
  synthesized packs, per-event enable (join/leave/mention/DM/whisper/poke/
  mic on/off), master volume, "Test all sounds".
- **Whisper**: DM plays its sound and flashes the taskbar (Windows
  FlashWindow); **Ctrl+R** (rebindable) arms whisper-reply to the last
  whisperer.
- **Voice limiter** (compressor, default on) and **per-user gain
  normalization** (RMS target, 4x cap) toggles in Settings → Playback.
- **Client Info Voice section**: packet loss %, jitter, jitter buffer delay,
  packets received/lost, audio level — from `getStats()`, refreshed every
  2 s.
- **Priority speaker & ducking** — the PRIO button in the voice bar toggles
  your priority-speaker flag (server-gated by `b_client_priority_speaker`).
  While another priority speaker in your channel is talking, all
  non-priority publishers are ducked to 25% (−12 dB); priority speakers are
  exempt. Un-ducking is delayed 500 ms so sentence gaps don't pump the gain.
- **Per-channel Opus quality** — the current channel's Opus settings are
  applied to the outgoing audio: a `maxBitrate` cap on the sender encoding
  (`opus_bitrate` 0 = server default 32000 bits/s) and a `contentHint` of
  `music` on stereo channels (`speech` otherwise), re-applied on channel
  switch and `channel_updated`.
- **Server-provided ICE servers** — the server delivers STUN/TURN entries
  (with time-limited TURN credentials) in the auth response; a non-empty
  list is passed to `RTCPeerConnection`, empty means browser defaults.

## Video (wave 3)

- **Video grid** — one tile per publisher video track (per-publisher SFU
  tracks, keyed by track ID = publisher client ID) with nickname overlay,
  speaking ring, avatar fallback behind the video, and a best-effort layer
  badge (HD/MD/LD from `getStats().frameWidth`, refreshed every 3 s). Auto
  layout: 1 tile full width, 2 half, 3–4 in 2×2, more in a scrollable
  three-column grid. The grid sits above chat and only takes space when
  someone streams video.
- **Focus mode** — click a tile for the large view; the others keep playing
  in a filmstrip row below. Click again or press Esc to return to the grid.
  This is how you watch multiple simultaneous screen shares: every share is
  a tile (a single user's share replaces their camera slot — one video track
  per publisher).
- **Quality selector** — right-click a tile → Auto/High/Mid/Low, sent as
  `MsgVideoQuality` (the server routes the RID f/h/q simulcast layer). Note:
  the preference is per subscriber connection, so it applies to all incoming
  video. Auto heuristic: focused view → high, grid view → mid, low-bandwidth
  mode → low.
- **Screen share dialog** — source preference (Screen / Window via
  `displaySurface`; Chromium's picker ultimately decides), quality presets
  (Text 1080p15@2.5 Mbps, Balanced 720p30@1.5 Mbps, Motion 720p60@2.5 Mbps,
  applied via capture constraints + sender `maxBitrate`), and an "include
  system audio" toggle (`getDisplayMedia({audio: true})` — published as a
  second audio track after renegotiation; on Windows Chromium this works for
  screen/tab shares, and the client falls back to video-only when refused).
- **Region share** — when the Region Capture API (`CropTarget.fromElement`)
  is available, "Region of this app" offers a draggable/resizable crop box
  over the app window; the option is disabled with a note when the API is
  missing.
- **Stop-share confirm** — stopping a share while others are in the channel
  asks first ("N users may be watching").
- **Camera frame rate** — Capture settings: 15/30/60 fps applied to
  `getUserMedia` video constraints + `contentHint` (`motion` at 60).
- **Low-bandwidth mode** — 📶 toggle in the voice bar (persisted): outgoing
  video is capped to a single 150 kbps layer, incoming video requests the
  low simulcast layer, and tile rendering is paused with a LOW BANDWIDTH
  badge on the grid.

## Transport security (wave 4a)

- **TLS everywhere on the control channel** — the client dials TLS by
  default; media is encrypted by WebRTC's DTLS-SRTP as always. Plaintext is
  an explicit dev opt-in (Settings → Security → "Allow plaintext
  connections"); connecting to a plaintext-only server without it fails with
  a clear hint, and an active plaintext session is reported as "PLAINTEXT —
  traffic is NOT encrypted" in the connect info line.
- **TOFU fingerprint pinning** — the server's self-signed certificate is
  pinned in `<UserConfigDir>/voicx/known_servers.json` (addr → SHA-256
  fingerprint). First connect: accepted, pinned, and reported as "new server
  fingerprint pinned". Later **mismatch: the connect hard-fails** with a
  prominent warning dialog (possible MITM) and only proceeds after an
  explicit "Trust new fingerprint" click.
- New bindings: `ServerFingerprint` (presented cert fingerprint),
  `TrustServerFingerprint(addr, fp)` (explicit trust action),
  `ConnectionSecurity` (display string for the login flow).

## Chat encryption (wave 4b)

All chat payloads are encrypted in the Go backend (`client/e2e.go`) — the
frontend never touches ciphertext:

- **Direct messages are true E2EE** (🔒 in the chat log): sealed with
  nacl/box to the recipient's X25519 public key (generated alongside the
  Ed25519 identity in `identity.json`; older identity files are upgraded in
  place). Public keys resolve through the server directory with a local
  cache; the server only relays ciphertext — including offline-spooled DMs.
- **Channel and global chat use server-held keys** (🛡): secretbox with the
  scope key, delivered sealed (`box.SealAnonymous`) via `MsgChannelKey` and
  rotated when a member leaves. The server can read these scopes (history,
  search, moderation in wave 5).
- Decryption failures (missing key, e.g. history from before a rotation)
  render as `[encrypted message — missing key]` instead of garbage.
- A 🔒 indicator in the sidebar shows when the connection is TLS + chat
  encryption; the Chat settings page carries the trust-model note.

## Chat (wave 5b)

The chat UI (`frontend/src/chat-ui.js` + `frontend/src/markdown.js`) renders
per-message elements with full metadata instead of plain text lines:

- **Markdown (91-93)**: `**bold**`, `*italic*`, `__underline__`, `~~strike~~`,
  `` `inline code` ``, fenced code blocks with light js/go highlighting
  (strings/comments/keywords), autolinked URLs (open externally via
  `BrowserOpenURL`, tooltip = full URL), and `:shortcode:` emoji. Rendering is
  XSS-safe: text is HTML-escaped first, then markup is applied.
- **Link previews (94)**: hovering a link shows a card. It tries a client-side
  `fetch` to scrape the `<title>`, but CORS blocks most cross-origin pages in
  the webview, so it usually falls back to a domain-only card. No external
  preview services are used.
- **Emoji (95)**: picker next to the input (unicode categories + custom server
  emoji fetched via `EmojiList`/`EmojiGet`, cached as data URLs); `:shortcode:`
  in messages renders as emoji.
- **Reactions (97)**: hover → 😊 strip (8 common + custom), toggle via
  `ChatReact`; chips with counts under the message. The own-reaction highlight
  only tracks toggles made this session (history carries counts only).
- **Files (98-100)**: paste or drop files onto the chat → preview row → on
  send they upload via `UploadFile` and post as `[file:<name>]`. Images render
  inline (click for a lightbox); other types get a download chip.
- **Edit/delete (101/102)**: hover your own message → ✎ (inline edit, Enter
  saves, Esc cancels) / 🗑 (confirm). Edits show "(edited)"; deletes render a
  muted tombstone. Note: `chat_edited` bodies arrive re-sealed and are not
  decrypted by the backend, so the UI re-fetches the message from history.
- **History (103)**: channel/global views page `ChatHistory` (50/page) — on
  channel join and when scrolling to the top (spinner + "beginning of
  history" marker).
- **Unread badges (104)** on tree channels (accent when they include a
  mention), **mention highlight + sound (106)**, `@nick` Tab-completion.
- **Reply-to (107)**: hover → ↩ quotes a message; sent as a `↪ <nick>: `
  prefix (the protocol has no reply field); clicking a quote jumps to the
  original. Threads (108) are descoped — reply-chains cover the basics.
- **Pins (109)**: 📌 in the channel header lists pinned messages
  (jump/unpin); pin via the message hover action.
- **Search (110)**: Ctrl+F filters loaded messages live (matches marked);
  "search server history" pages up to 10 history pages backwards.
- **Topic/description (111-113)**: the header shows the channel topic
  (tooltip = full text); ⓘ opens the markdown channel description panel
  (inline images supported).
- **Read state (121)**: last-read pointers persist in settings
  (`last_read_channels`); a "— new messages —" divider marks the first unread.
- **PM tabs (122-124)**: DMs open per-user tabs (unread dot, offline badge +
  toast for spooled messages). Delivery/read receipts: received DMs ack
  `SendChatDelivered`, focused tabs ack `SendChatRead`; outgoing DMs show
  ✓ (delivered) / ✓✓ (read).
- **Export (125)**: ⬇ in the header saves a plain-text transcript of the
  loaded messages via the native save dialog.
- **Display prefs (126-129)**: Settings → Chat: timestamps
  off/relative/absolute, density, font size (12-18), IRC-lines/bubbles —
  applied live.
- **System lines (130/131)**: join/leave and kick lines can be hidden;
  consecutive join/leave lines within 60s collapse into one.
- **Announcement (132)**: server announcements show a dismissible accent
  banner (dismissal persisted by content hash); the MOTD (133) appears as a
  system line once per connect (`App.MOTD`).
- **Scroll lock (134)**: auto-scroll only at the bottom; otherwise a
  "↓ N new messages" pill jumps down.
- **Quick switcher (135)**: Ctrl+K fuzzy-jumps to channels, online users
  (opens a PM tab), and open PM tabs.

## Permissions & Groups (wave 6b)

Permission/group administration lives in `frontend/src/perms-ui.js` (+ the new
bindings in `groups.go`). All views degrade gracefully: privileged menu items
are always visible, but without the required permission (or admin) the dialog
shows a "requires …" notice instead of controls; the server re-checks every
write and denials arrive as toasts (grant-cap errors included).

- **Permission Manager** (Permissions menu): tabs for Server Groups / Clients /
  Channel / Channel Groups. The right side is the editable permission grid
  (136) — click a row for the inline editor (value + grant inputs, skip/negate
  checkboxes with TS3 tooltips (152/153), Set/Unset). A filter box searches
  keys (154); ⬇ exports the target's grid as JSON (148). Current values are
  read via the `PermList` request; after each write the grid re-queries.
- **Trace** (137/155): on the Clients tab, each row's editor has a Trace
  button — a panel showing the effective value, the winning tier highlighted,
  and every tier's contribution in resolver order.
- **Group management** (138-141): create/rename/delete groups (deleting a
  non-empty group needs the force confirm), member lists with unassign,
  assign via online-user dropdown or unique-ID entry, an optional duration in
  minutes (timed memberships, 145), and drag & drop of users from the channel
  tree onto the members panel. Channel-group membership is channel-scoped
  (channel picker). Group icons upload from the group's action bar (177).
- **Templates** (142): "Template…" per group/user target — guest/member/
  moderator/admin picker, confirm, applied through the write path (audited).
- **Audit Log** (149/197, Tools menu): paged table (time, actor, action,
  target, detail), action filter, "Load older" paging.
- **Bans** (172, Tools menu): ban list with reason/issuer/expiry and lift
  buttons. The user context menu (right-click) gains Kick from channel/server
  (reason dialog, 170) and Ban (reason + 5m/1h/1d/permanent presets, 171),
  pre-gated by your resolved kick/ban powers.
- **Channel dialogs** (164/167): right-click a channel — full create dialog
  (name, parent, type, topic, max clients, password, needed join power, Opus
  preset; root create via the + button in the sidebar), edit dialog with
  description, and delete with a subtree-count warning. A "channel admin"
  chip shows when you hold `b_channel_modify` (156 is UI-only; the server has
  no creator auto-assignment). Re-parenting is not in this wave (server gap —
  `ChannelEdit` has no parent field).
- **Tree presentation** (177-179): hoisted server groups render as sections
  above the channels (with group icons), nickname colors come from the first
  applicable group by sort order, and the details card shows a user's group
  chips (143-145 display).

## Files & Media (wave 7)

The files UI (`frontend/src/files-ui.js`, bindings in `files.go`) adds a
**Files** center tab next to Chat:

- **Browser (256)**: the current channel's files (follows the channel you're
  in) with name/size/uploader/date/SHA-256 columns, download/delete/rename/
  refresh, and a breadcrumb for the virtual folders (261 — folders derive
  from file rows; empty folders do not persist). A quota bar shows channel
  usage (265).
- **Upload (257/260)**: drop files anywhere on the pane or use ⬆ — a
  sequential queue uploads them with per-file progress (258). Cancel from the
  transfers window; the server cleans up the partial file.
- **Versions (264)**: overwrites keep the last 3 versions (`<name>.vN`);
  expand a row to list and download old versions.
- **Links (267)**: 🔗 mints a 15-minute `/dl/<token>` URL (served on the
  health port) and copies it — LAN-friendly sharing without a client.
- **Integrity (279/280)**: each row shows the truncated SHA-256 (click to
  copy); ✓ re-downloads and compares (green/red result).
- **Transfers window (277/278)**: ⇅ button — active + recent transfers with
  direction, progress, speed, ETA, status, cancel, and a live aggregate
  throughput sparkline.
- **Images (268/269/274)**: the avatar dialog crops/zooms on a canvas and
  outputs 256×256 PNG; icons (server/channel/group) are downscaled to 1024px
  and recompressed JPEG q0.85; animated GIF/WebP always pass through
  untouched so animation survives. The server icon (admin, Self menu) renders
  in the sidebar; the channel edit dialog can upload an icon or reuse one
  from another channel (271).

## Window & System Integration (wave 8a)

- **Multi-server tabs (281)**: TS3-style server tab bar above the panes.
  Architecture (documented in `tabs.go`): the backend keeps one connManager
  per tab; all bindings operate on the *active* tab, so the bound API is
  unchanged. Events from background tabs are journaled in Go and replayed on
  activation (after a `tab_reset` that clears chat/tree state), keeping the
  frontend single-state. Tabs show addr + nickname, unread/mention badges,
  and a close button; "+" opens the connect dialog for a new tab. Voice is
  active-tab only this wave (torn down on switch).
- **Connect dialog (282)**: recent-servers list (last 10, no passwords) with
  a bookmark star toggle.
- **Bookmarks (283/284)**: folders (grouped menu + manager), manual ordering,
  color dots, per-bookmark hotkey profile and auto-connect flag.
- **Quick connect (285)**: rebindable hotkey (default Ctrl+Shift+C) connects
  the last-used bookmark/recent in a new tab (guest direct, accounts prefill
  — passwords are never stored).
- **Auto-connect (286)**: bookmark flag; on startup flagged bookmarks connect
  as guests or prefill the login dialog (same password limitation).
- **Tray (287-290)**: `getlantern/systray` (new client dep — Wails v2 has no
  tray API). Threading: systray owns the main thread, wails.Run a goroutine.
  Menu: show/hide, mute, deafen, disconnect, quit; title shows PTT state;
  mentions bump the tooltip badge and flash the taskbar. Close-to-tray via
  `OnBeforeClose`. Minimize-to-tray is not possible (Wails v2 has no minimize
  event) and window opacity is not supported by the WebView2 backend — both
  are honestly disabled/marked.
- **Compact mode (293)**: View menu / hotkey / setting — voice bar only.
- **Themes & fonts (294-297)**: dark/light/high-contrast variable sets,
  accent color picker, scoped user-CSS textarea, UI font family + size, all
  live-applied.
- **Keyboard navigation (298)**: Esc closes dialogs/menus, `:focus-visible`
  outlines, arrow-key channel-tree navigation.
- **Hotkey map (299-301)**: settings page lists all six bindable actions
  with rebind/unbind, named per-server profiles (assigned via the bookmark),
  and registration-conflict errors naming the action.

## Client UX & Social (wave 8b)

- **Tree polish (302-306, 310, 319)**: collapse/expand-all buttons +
  double-click toggle, client counts `[n/max]`, password lock icons (new
  `has_password` snapshot flag), group icons next to names, live filter box,
  drag users onto channels (self joins, others via the new `MoveClient`
  binding), ctrl/shift multi-select with a batch context menu.
- **Presence (307-309)**: `MsgSetStatus` (online/away/busy + message)
  broadcasts `status_changed`; tree icons, auto-away after N idle minutes
  (setting, default 15) restoring on input; Self → Set status.
- **Social (316-318, 321/322)**: contacts page (online status from presence,
  nickname history), block list (chat hidden + voice locally muted), poke
  dialog → toast + sound + flash on receipt (server: `MsgPoke` with
  permission check and a 30 s per-target cooldown).
- **Information (313-315, 323-325)**: server news pane
  (`MsgServerInfoQuery`: name/version/uptime/counts/MOTD), client-info
  additions (avatar lightbox, groups, local per-user notes persisted in
  settings), 500 ms hover cards for users and channels, copy helpers
  (UID / channel ID / server address).
- **Diagnostics (326-328, 332/333)**: Help → Export logs (zip), Tools →
  Debug console (live protocol frame viewer with filter/pause/clear),
  Connection stats (1 s RTT/loss charts from ping samples + getStats),
  reconnect countdown in the status pill, connection-quality pill (good
  <100 ms, fair <250 ms RTT).
- **Lifecycle (329-331)**: first-run onboarding wizard (identity, mic test,
  connect), What's New dialog on version change (bundled offline notes),
  crash guard writing `crash.log` (offered at next start, exportable — no
  upload).
- **Per-server overrides (334/335)**: bookmark nickname override (applied at
  connect) and avatar override (uploaded after connect).

## Polish & Accessibility (wave 8c)

- **i18n (336)**: `frontend/src/i18n.js` — message catalogs (en/de) with a
  `t(key)` helper; settings language dropdown (system/en/de) applies live
  (menus, settings dialog, login card). Adding strings: add the key to both
  catalogs (a Go parity test keeps them in sync; missing keys warn in the
  debug console). Full string coverage is intentionally future work.
- **Motion (337/344)**: 150 ms fade/slide on dialogs/toasts/tabs, disabled
  by the reduce-motion setting and `prefers-reduced-motion`.
- **Layout (338)**: drag handles between sidebar/center/details, widths
  persisted, double-click resets.
- **Windows (339-341)**: pop-out chat as a floating draggable panel (a true
  second OS window needs Wails v3 — documented), fullscreen video tiles,
  zen mode (channels + voice only; View menu, rebindable Ctrl+Shift+Z, exit
  indicator).
- **Idle video (342)**: unfocused >60 s pauses remote video and drops to
  low quality; resumes on focus (setting).
- **Accessibility (343)**: tree/treeitem/log/dialog ARIA roles, a visually
  hidden aria-live region announcing speaking events.
- **Notifications (345-348)**: native Windows balloon notifications for
  unfocused mentions/pokes (PowerShell WinForms balloon — verified working
  on the dev machine; WinRT toast is unreliable for unpackaged apps), a
  bell-icon notification center (50 entries, click-through, clear-all), and
  DND with a quiet-hours schedule suppressing toasts/sounds/flashes while
  badges keep counting.
- **Performance (349)**: windowed tree above 500 rows — DOM stays
  O(channels); branches expand on double-click; `window.__voicxFakeTree(n)`
  in the console measures render time.
- **Settings search (350)**: cross-page search with jump + highlight.

## Notifications & Presence (wave 9)

- **Invisible status (381)**: a 4th presence value, admin-only to set.
  Invisible users are hidden from non-admin snapshots (visible to admins and
  themselves; they still see everyone). Going invisible looks like a leave
  to non-admins, returning looks like a join; further status changes while
  invisible reach only admins. The 👻 marker shows in admin trees.
- **Notification matrix (385)**: central `notify()` dispatch
  (`frontend/src/notifications.js`) — rows mention/keyword/DM/poke/
  join-leave/buddy-online/kick/announcement/channel-watch, columns
  toast/sound/flash/native, edited on the Notifications settings page. All
  dispatch sites (chat, DMs, pokes, joins, kicks, announcements, buddy
  alerts, keyword hits, channel watch) route through it; DND still records
  silently.
- **Per-channel overrides (386/391) + mute (387)**: channel context menu →
  Notifications… — inherit/on/off for messages, mentions, and joins; full
  channel mute (🔕 in the tree; messages still visible when selected);
  channel watch: toast when the user count crosses a threshold (389).
- **Buddy alerts (383)**: per-contact "notify when online" bell, fired once
  per connect from snapshots.
- **Sounds (384)**: per-event custom beep (frequency + duration + preview)
  overriding the sound-pack preset.
- **Keywords (388)**: per-server keyword list (Chat settings); whole-word
  matches get mention-style highlight + the matrix's keyword row.
- **Alpha notice (215)**: per-version alpha dialog with a dismiss checkbox,
  plus a permanent α badge next to the wordmark that re-opens it.

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
  available; there is no JS flag to force it). The server emits one audio and
  one video track per publisher (track ID = publisher client ID via MSID) and
  renegotiates over the control channel when membership changes; on ICE
  failure the client re-offers with `iceRestart` and a 1s/2s/5s/15s backoff
  ladder before warning.

## Bound API (Go backend)

`Connect`, `Disconnect`, `Connected`, `ClientID`, `JoinChannel`, `SendChat`
(global/channel/direct), `WhisperSet`, `GetPermissions` (resolved permission
set via `MsgPermissionsQuery`), `WebRTCOffer`/`WebRTCAnswer`/
`SendICECandidate`, `GetICEServers` (STUN/TURN from the auth response),
`SetPrioritySpeaker`, `ChannelEdit`, `SetScreenShare`, `SetVideoQuality`
(simulcast layer high/mid/low), `SetMuted`, `SetPTT`, `SetAvatar`,
`ServerFingerprint`, `TrustServerFingerprint`, `ConnectionSecurity` (TOFU).

Wave 5b adds `ChatHistory`, `ChatEditMessage`, `ChatDeleteMessage`,
`ChatPinMessage`, `ChatPins`, `ChatReact`, `SendTyping`, `SendChatDelivered`,
`SendChatRead`, `EmojiList`, `EmojiGet`, `UploadFile`, `DownloadFile`,
`ExportChat` and `MOTD` (message of the day from the auth response).

Wave 6b adds `IsAdmin`, `GroupList`, `GroupCreate`, `GroupRename`,
`GroupDelete`, `GroupAssign`, `GroupUnassign`, `GroupMembers`,
`GroupIconSet`, `GroupIconGet`, `PermList`, `PermSet`, `PermUnset`,
`PermTemplateApply`, `PermTrace`, `AuditLog`, `BanList`, `BanRemove`,
`KickClient`, `CreateChannel`, `DeleteChannel`; `ChannelEdit` gained the
`description` argument.

Wave 7 adds `FileList`, `FileDelete`, `FileRename`, `FileVersions`,
`FileLink`, `VerifyFile`, `ServerIconSet`, `ServerIconGet`,
`ChannelIconSet`, `UploadFileProgress`/`DownloadFileProgress` (async,
`ft_progress` events), `CancelTransfer`, and `PickSavePath`.

Wave 8a adds `ConnectTab`, `ConnectGuestTab`, `CloseTab`, `SetActiveTab`,
`ListTabs`, `RecordRecent`, `SetHotkey`, `ApplyHotkeyProfile`, and
`SetAlwaysOnTop` (plus the `tab_update`/`tab_reset` events and the systray
integration).

Wave 8b adds `SetStatus`, `Poke`, `ServerInfo`, `MoveClient`, `ExportLogs`,
`SetDebugFrames` (+ `debug_frame` event), `LastCrash`, and `WhatsNew`.

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

## Auto-update

The client can update itself from GitHub Releases (Help → **Check for
updates…**): it queries the latest release of the configured repo, compares
base semver + build number against the embedded version, downloads the
Windows asset with a progress bar, verifies its SHA-256 against the
release's `checksums.txt`, and self-applies via `minio/selfupdate` (which
handles the running-exe rename dance on Windows). Nothing applies without
explicit confirmation — a final "Restart now" button relaunches the new
binary.

At startup (after login) it auto-checks quietly and shows a toast when an
update exists; disable it in Settings → Application → "Check for updates at
startup".

The update source is the `UpdateRepo` ldflags variable
(`-X voicx/internal/version.UpdateRepo=<owner/repo>` — CI sets it to the
repo automatically). With the placeholder default the check reports "no
update source" and stays silent.

**Security note**: SHA-256 verification guards download corruption/tampering
on the mirror path, **not authenticity** — whoever can publish to the repo
controls the binary. Signed releases (sigstore/minisign) are future work.

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
