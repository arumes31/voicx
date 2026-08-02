export namespace main {

	export class BeepSpec {
	    freq: number;
	    duration_ms: number;

	    static createFrom(source: any = {}) {
	        return new BeepSpec(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.freq = source["freq"];
	        this.duration_ms = source["duration_ms"];
	    }
	}
	export class Bookmark {
	    name: string;
	    addr: string;
	    nickname: string;
	    folder?: string;
	    order?: number;
	    color?: string;
	    auto_connect?: boolean;
	    profile?: string;
	    nickname_override?: string;
	    avatar_override_b64?: string;

	    static createFrom(source: any = {}) {
	        return new Bookmark(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.addr = source["addr"];
	        this.nickname = source["nickname"];
	        this.folder = source["folder"];
	        this.order = source["order"];
	        this.color = source["color"];
	        this.auto_connect = source["auto_connect"];
	        this.profile = source["profile"];
	        this.nickname_override = source["nickname_override"];
	        this.avatar_override_b64 = source["avatar_override_b64"];
	    }
	}
	export class ChannelOverride {
	    messages?: string;
	    mentions?: string;
	    joins?: string;
	    muted?: boolean;
	    watch_threshold?: number;

	    static createFrom(source: any = {}) {
	        return new ChannelOverride(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.messages = source["messages"];
	        this.mentions = source["mentions"];
	        this.joins = source["joins"];
	        this.muted = source["muted"];
	        this.watch_threshold = source["watch_threshold"];
	    }
	}
	export class ChatExportResult {
	    text: string;
	    messages: number;
	    undecryptable: number;
	    complete: boolean;

	    static createFrom(source: any = {}) {
	        return new ChatExportResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.text = source["text"];
	        this.messages = source["messages"];
	        this.undecryptable = source["undecryptable"];
	        this.complete = source["complete"];
	    }
	}
	export class ChatSearchResult {
	    messages: netproto.ChatHistoryEntry[];
	    scanned: number;
	    undecryptable: number;

	    static createFrom(source: any = {}) {
	        return new ChatSearchResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.messages = this.convertValues(source["messages"], netproto.ChatHistoryEntry);
	        this.scanned = source["scanned"];
	        this.undecryptable = source["undecryptable"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Contact {
	    unique_id: string;
	    label?: string;
	    nick_history?: string[];
	    notify_online?: boolean;

	    static createFrom(source: any = {}) {
	        return new Contact(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unique_id = source["unique_id"];
	        this.label = source["label"];
	        this.nick_history = source["nick_history"];
	        this.notify_online = source["notify_online"];
	    }
	}
	export class DMEntry {
	    seq: number;
	    from_unique_id: string;
	    from_nickname: string;
	    body: string;
	    sent_at: number;
	    self?: boolean;
	    client_msg_id?: string;
	    offline?: boolean;

	    static createFrom(source: any = {}) {
	        return new DMEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.from_unique_id = source["from_unique_id"];
	        this.from_nickname = source["from_nickname"];
	        this.body = source["body"];
	        this.sent_at = source["sent_at"];
	        this.self = source["self"];
	        this.client_msg_id = source["client_msg_id"];
	        this.offline = source["offline"];
	    }
	}
	export class DMPeer {
	    unique_id: string;
	    nickname?: string;
	    messages: number;
	    last_at: number;

	    static createFrom(source: any = {}) {
	        return new DMPeer(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unique_id = source["unique_id"];
	        this.nickname = source["nickname"];
	        this.messages = source["messages"];
	        this.last_at = source["last_at"];
	    }
	}
	export class E2EEDiagnostics {
	    cipher: string;
	    peer_unique_id?: string;
	    safety_number?: string;
	    peer_key_available: boolean;
	    cached_peers: number;
	    scope_keys: number;
	    refused_keys: number;
	    pending_key_pulls: number;

	    static createFrom(source: any = {}) {
	        return new E2EEDiagnostics(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.cipher = source["cipher"];
	        this.peer_unique_id = source["peer_unique_id"];
	        this.safety_number = source["safety_number"];
	        this.peer_key_available = source["peer_key_available"];
	        this.cached_peers = source["cached_peers"];
	        this.scope_keys = source["scope_keys"];
	        this.refused_keys = source["refused_keys"];
	        this.pending_key_pulls = source["pending_key_pulls"];
	    }
	}
	export class HotkeyProfile {
	    ptt?: string;
	    mute_toggle?: string;
	    whisper_reply?: string;
	    quick_connect?: string;
	    compact_toggle?: string;
	    zen_toggle?: string;
	    deafen_toggle?: string;

	    static createFrom(source: any = {}) {
	        return new HotkeyProfile(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ptt = source["ptt"];
	        this.mute_toggle = source["mute_toggle"];
	        this.whisper_reply = source["whisper_reply"];
	        this.quick_connect = source["quick_connect"];
	        this.compact_toggle = source["compact_toggle"];
	        this.zen_toggle = source["zen_toggle"];
	        this.deafen_toggle = source["deafen_toggle"];
	    }
	}
	export class IdentityEntry {
	    id: string;
	    name: string;
	    unique_id: string;
	    active: boolean;
	    created_at?: string;
	    exported_at?: string;
	    security_level: number;
	    protection: string;
	    path: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new IdentityEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.unique_id = source["unique_id"];
	        this.active = source["active"];
	        this.created_at = source["created_at"];
	        this.exported_at = source["exported_at"];
	        this.security_level = source["security_level"];
	        this.protection = source["protection"];
	        this.path = source["path"];
	        this.error = source["error"];
	    }
	}
	export class IdentityLevelResult {
	    level: number;
	    counter: number;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new IdentityLevelResult(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.level = source["level"];
	        this.counter = source["counter"];
	        this.error = source["error"];
	    }
	}
	export class NotifyChannels {
	    toast: boolean;
	    sound: boolean;
	    flash: boolean;
	    native: boolean;

	    static createFrom(source: any = {}) {
	        return new NotifyChannels(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.toast = source["toast"];
	        this.sound = source["sound"];
	        this.flash = source["flash"];
	        this.native = source["native"];
	    }
	}
	export class RecentServer {
	    addr: string;
	    nickname: string;
	    last_used: number;

	    static createFrom(source: any = {}) {
	        return new RecentServer(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.addr = source["addr"];
	        this.nickname = source["nickname"];
	        this.last_used = source["last_used"];
	    }
	}
	export class Settings {
	    settings_version: number;
	    bookmarks: Bookmark[];
	    recents: RecentServer[];
	    always_on_top: boolean;
	    minimize_to_tray: boolean;
	    close_to_tray: boolean;
	    compact_mode: boolean;
	    theme: string;
	    accent_color: string;
	    user_css: string;
	    ui_font: string;
	    ui_font_size: number;
	    window_opacity: number;
	    language: string;
	    reduce_motion: boolean;
	    sidebar_width: number;
	    details_width: number;
	    idle_video_pause: boolean;
	    dnd_enabled: boolean;
	    dnd_from: string;
	    dnd_to: string;
	    capture_device_id: string;
	    activation_mode: string;
	    vad_threshold: number;
	    echo_cancellation: boolean;
	    noise_suppression: boolean;
	    playback_device_id: string;
	    volume: number;
	    hotkey_ptt: string;
	    hotkey_mute: string;
	    hotkey_deafen: string;
	    hotkey_quick_connect: string;
	    hotkey_zen: string;
	    hotkey_compact: string;
	    hotkey_profiles?: Record<string, HotkeyProfile>;
	    chat_max_lines: number;
	    log_channel_chat: boolean;
	    log_private_chat: boolean;
	    log_server_chat: boolean;
	    notify_join_leave: boolean;
	    notify_connection: boolean;
	    play_sounds: boolean;
	    whisper_sound: boolean;
	    chat_notification_level: string;
	    whisper_clients: string[];
	    whisper_channels: number[];
	    whisper_active: boolean;
	    download_folder: string;
	    reconnect_on_loss: boolean;
	    updates_auto_check: boolean;
	    user_volumes: Record<string, number>;
	    muted_users: string[];
	    ptt_release_delay_ms: number;
	    warn_muted_talking: boolean;
	    warn_empty_channel: boolean;
	    sound_pack: string;
	    sound_volume: number;
	    event_sounds: Record<string, boolean>;
	    whisper_reply_hotkey: string;
	    voice_limiter: boolean;
	    gain_normalize: boolean;
	    camera_fps: number;
	    low_bandwidth: boolean;
	    allow_plaintext: boolean;
	    e2ee_verified?: Record<string, string>;
	    chat_timestamps: string;
	    chat_density: string;
	    chat_font_size: number;
	    chat_layout: string;
	    sys_join_leave: boolean;
	    sys_kick: boolean;
	    last_read_channels: Record<string, number>;
	    dismissed_announcement: string;
	    contacts?: Contact[];
	    blocked_users?: string[];
	    user_notes?: Record<string, string>;
	    recent_channels?: Record<string, Array<number>>;
	    auto_away_minutes: number;
	    auto_away_message: string;
	    onboarding_done: boolean;
	    last_seen_version: string;
	    active_identity?: string;
	    identity_key_protection?: string;
	    notify_matrix?: Record<string, NotifyChannels>;
	    custom_sounds?: Record<string, BeepSpec>;
	    channel_notify?: Record<string, ChannelOverride>;
	    keywords?: Record<string, Array<string>>;
	    alpha_dismissed: string;

	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.settings_version = source["settings_version"];
	        this.bookmarks = this.convertValues(source["bookmarks"], Bookmark);
	        this.recents = this.convertValues(source["recents"], RecentServer);
	        this.always_on_top = source["always_on_top"];
	        this.minimize_to_tray = source["minimize_to_tray"];
	        this.close_to_tray = source["close_to_tray"];
	        this.compact_mode = source["compact_mode"];
	        this.theme = source["theme"];
	        this.accent_color = source["accent_color"];
	        this.user_css = source["user_css"];
	        this.ui_font = source["ui_font"];
	        this.ui_font_size = source["ui_font_size"];
	        this.window_opacity = source["window_opacity"];
	        this.language = source["language"];
	        this.reduce_motion = source["reduce_motion"];
	        this.sidebar_width = source["sidebar_width"];
	        this.details_width = source["details_width"];
	        this.idle_video_pause = source["idle_video_pause"];
	        this.dnd_enabled = source["dnd_enabled"];
	        this.dnd_from = source["dnd_from"];
	        this.dnd_to = source["dnd_to"];
	        this.capture_device_id = source["capture_device_id"];
	        this.activation_mode = source["activation_mode"];
	        this.vad_threshold = source["vad_threshold"];
	        this.echo_cancellation = source["echo_cancellation"];
	        this.noise_suppression = source["noise_suppression"];
	        this.playback_device_id = source["playback_device_id"];
	        this.volume = source["volume"];
	        this.hotkey_ptt = source["hotkey_ptt"];
	        this.hotkey_mute = source["hotkey_mute"];
	        this.hotkey_deafen = source["hotkey_deafen"];
	        this.hotkey_quick_connect = source["hotkey_quick_connect"];
	        this.hotkey_zen = source["hotkey_zen"];
	        this.hotkey_compact = source["hotkey_compact"];
	        this.hotkey_profiles = this.convertValues(source["hotkey_profiles"], HotkeyProfile, true);
	        this.chat_max_lines = source["chat_max_lines"];
	        this.log_channel_chat = source["log_channel_chat"];
	        this.log_private_chat = source["log_private_chat"];
	        this.log_server_chat = source["log_server_chat"];
	        this.notify_join_leave = source["notify_join_leave"];
	        this.notify_connection = source["notify_connection"];
	        this.play_sounds = source["play_sounds"];
	        this.whisper_sound = source["whisper_sound"];
	        this.chat_notification_level = source["chat_notification_level"];
	        this.whisper_clients = source["whisper_clients"];
	        this.whisper_channels = source["whisper_channels"];
	        this.whisper_active = source["whisper_active"];
	        this.download_folder = source["download_folder"];
	        this.reconnect_on_loss = source["reconnect_on_loss"];
	        this.updates_auto_check = source["updates_auto_check"];
	        this.user_volumes = source["user_volumes"];
	        this.muted_users = source["muted_users"];
	        this.ptt_release_delay_ms = source["ptt_release_delay_ms"];
	        this.warn_muted_talking = source["warn_muted_talking"];
	        this.warn_empty_channel = source["warn_empty_channel"];
	        this.sound_pack = source["sound_pack"];
	        this.sound_volume = source["sound_volume"];
	        this.event_sounds = source["event_sounds"];
	        this.whisper_reply_hotkey = source["whisper_reply_hotkey"];
	        this.voice_limiter = source["voice_limiter"];
	        this.gain_normalize = source["gain_normalize"];
	        this.camera_fps = source["camera_fps"];
	        this.low_bandwidth = source["low_bandwidth"];
	        this.allow_plaintext = source["allow_plaintext"];
	        this.e2ee_verified = source["e2ee_verified"];
	        this.chat_timestamps = source["chat_timestamps"];
	        this.chat_density = source["chat_density"];
	        this.chat_font_size = source["chat_font_size"];
	        this.chat_layout = source["chat_layout"];
	        this.sys_join_leave = source["sys_join_leave"];
	        this.sys_kick = source["sys_kick"];
	        this.last_read_channels = source["last_read_channels"];
	        this.dismissed_announcement = source["dismissed_announcement"];
	        this.contacts = this.convertValues(source["contacts"], Contact);
	        this.blocked_users = source["blocked_users"];
	        this.user_notes = source["user_notes"];
	        this.recent_channels = source["recent_channels"];
	        this.auto_away_minutes = source["auto_away_minutes"];
	        this.auto_away_message = source["auto_away_message"];
	        this.onboarding_done = source["onboarding_done"];
	        this.last_seen_version = source["last_seen_version"];
	        this.active_identity = source["active_identity"];
	        this.identity_key_protection = source["identity_key_protection"];
	        this.notify_matrix = this.convertValues(source["notify_matrix"], NotifyChannels, true);
	        this.custom_sounds = this.convertValues(source["custom_sounds"], BeepSpec, true);
	        this.channel_notify = this.convertValues(source["channel_notify"], ChannelOverride, true);
	        this.keywords = source["keywords"];
	        this.alpha_dismissed = source["alpha_dismissed"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TabInfo {
	    id: string;
	    addr: string;
	    nickname: string;
	    connected: boolean;
	    active: boolean;
	    unread: number;
	    mentions: number;

	    static createFrom(source: any = {}) {
	        return new TabInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.addr = source["addr"];
	        this.nickname = source["nickname"];
	        this.connected = source["connected"];
	        this.active = source["active"];
	        this.unread = source["unread"];
	        this.mentions = source["mentions"];
	    }
	}
	export class UpdateInfo {
	    available: boolean;
	    version: string;
	    url: string;
	    sha256url: string;
	    size: number;

	    static createFrom(source: any = {}) {
	        return new UpdateInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.version = source["version"];
	        this.url = source["url"];
	        this.sha256url = source["sha256url"];
	        this.size = source["size"];
	    }
	}
	export class identityInfo {
	    unique_id: string;
	    created_at?: string;
	    path: string;

	    static createFrom(source: any = {}) {
	        return new identityInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unique_id = source["unique_id"];
	        this.created_at = source["created_at"];
	        this.path = source["path"];
	    }
	}

}

export namespace netproto {

	export class AuditEntry {
	    id: number;
	    actor: string;
	    action: string;
	    target: string;
	    detail: string;
	    created_at: number;

	    static createFrom(source: any = {}) {
	        return new AuditEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.actor = source["actor"];
	        this.action = source["action"];
	        this.target = source["target"];
	        this.detail = source["detail"];
	        this.created_at = source["created_at"];
	    }
	}
	export class AuditLogResponse {
	    entries: AuditEntry[];

	    static createFrom(source: any = {}) {
	        return new AuditLogResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], AuditEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class AvatarData {
	    unique_id: string;
	    data_base64: string;
	    content_type: string;

	    static createFrom(source: any = {}) {
	        return new AvatarData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unique_id = source["unique_id"];
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class BanEntry {
	    id: number;
	    type: number;
	    value: string;
	    reason?: string;
	    banned_by?: string;
	    created_at: number;
	    expires_at?: number;

	    static createFrom(source: any = {}) {
	        return new BanEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.value = source["value"];
	        this.reason = source["reason"];
	        this.banned_by = source["banned_by"];
	        this.created_at = source["created_at"];
	        this.expires_at = source["expires_at"];
	    }
	}
	export class BanListResponse {
	    bans: BanEntry[];

	    static createFrom(source: any = {}) {
	        return new BanListResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bans = this.convertValues(source["bans"], BanEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ChannelIconData {
	    channel_id: number;
	    data_base64: string;
	    content_type?: string;

	    static createFrom(source: any = {}) {
	        return new ChannelIconData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.channel_id = source["channel_id"];
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class ChannelKey {
	    channel_id: number;
	    key_id: number;
	    sealed_key: string;

	    static createFrom(source: any = {}) {
	        return new ChannelKey(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.channel_id = source["channel_id"];
	        this.key_id = source["key_id"];
	        this.sealed_key = source["sealed_key"];
	    }
	}
	export class ChatFilterResponse {
	    word_filter: string;
	    link_blacklist: string;
	    link_whitelist: string;
	    from_config?: boolean;

	    static createFrom(source: any = {}) {
	        return new ChatFilterResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.word_filter = source["word_filter"];
	        this.link_blacklist = source["link_blacklist"];
	        this.link_whitelist = source["link_whitelist"];
	        this.from_config = source["from_config"];
	    }
	}
	export class ChatHistoryEntry {
	    id: number;
	    from_unique_id: string;
	    from_nickname: string;
	    reply_to_id?: number;
	    version: number;
	    body_enc?: string;
	    key_id?: number;
	    body?: string;
	    enc_verified?: boolean;
	    sent_at: number;
	    edited_at?: number;
	    deleted?: boolean;
	    reactions?: Record<string, number>;

	    static createFrom(source: any = {}) {
	        return new ChatHistoryEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.from_unique_id = source["from_unique_id"];
	        this.from_nickname = source["from_nickname"];
	        this.reply_to_id = source["reply_to_id"];
	        this.version = source["version"];
	        this.body_enc = source["body_enc"];
	        this.key_id = source["key_id"];
	        this.body = source["body"];
	        this.enc_verified = source["enc_verified"];
	        this.sent_at = source["sent_at"];
	        this.edited_at = source["edited_at"];
	        this.deleted = source["deleted"];
	        this.reactions = source["reactions"];
	    }
	}
	export class ChatHistoryResponse {
	    channel_id: number;
	    messages: ChatHistoryEntry[];
	    keys?: ChannelKey[];
	    refused?: number[];
	    truncated?: boolean;

	    static createFrom(source: any = {}) {
	        return new ChatHistoryResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.channel_id = source["channel_id"];
	        this.messages = this.convertValues(source["messages"], ChatHistoryEntry);
	        this.keys = this.convertValues(source["keys"], ChannelKey);
	        this.refused = source["refused"];
	        this.truncated = source["truncated"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ChatPinEntry {
	    message_id: number;
	    pinned_by: string;
	    pinned_at: number;
	    message?: ChatHistoryEntry;

	    static createFrom(source: any = {}) {
	        return new ChatPinEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.message_id = source["message_id"];
	        this.pinned_by = source["pinned_by"];
	        this.pinned_at = source["pinned_at"];
	        this.message = this.convertValues(source["message"], ChatHistoryEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ChatPinsResponse {
	    channel_id: number;
	    pins: ChatPinEntry[];
	    keys?: ChannelKey[];
	    refused?: number[];
	    truncated?: boolean;

	    static createFrom(source: any = {}) {
	        return new ChatPinsResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.channel_id = source["channel_id"];
	        this.pins = this.convertValues(source["pins"], ChatPinEntry);
	        this.keys = this.convertValues(source["keys"], ChannelKey);
	        this.refused = source["refused"];
	        this.truncated = source["truncated"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ClientInfoResponse {
	    client_id: string;
	    unique_id: string;
	    nickname: string;
	    channel_id: number;
	    connected_at: number;
	    idle_seconds: number;
	    ping_ms: number;
	    ip?: string;
	    port?: number;
	    bytes_in: number;
	    bytes_out: number;

	    static createFrom(source: any = {}) {
	        return new ClientInfoResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.client_id = source["client_id"];
	        this.unique_id = source["unique_id"];
	        this.nickname = source["nickname"];
	        this.channel_id = source["channel_id"];
	        this.connected_at = source["connected_at"];
	        this.idle_seconds = source["idle_seconds"];
	        this.ping_ms = source["ping_ms"];
	        this.ip = source["ip"];
	        this.port = source["port"];
	        this.bytes_in = source["bytes_in"];
	        this.bytes_out = source["bytes_out"];
	    }
	}
	export class ComplaintEntry {
	    target_unique_id: string;
	    target_nickname?: string;
	    from_unique_id: string;
	    from_nickname?: string;
	    reason: string;
	    created_at: number;

	    static createFrom(source: any = {}) {
	        return new ComplaintEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.target_unique_id = source["target_unique_id"];
	        this.target_nickname = source["target_nickname"];
	        this.from_unique_id = source["from_unique_id"];
	        this.from_nickname = source["from_nickname"];
	        this.reason = source["reason"];
	        this.created_at = source["created_at"];
	    }
	}
	export class Complaints {
	    entries: ComplaintEntry[];

	    static createFrom(source: any = {}) {
	        return new Complaints(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], ComplaintEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class EmojiData {
	    name: string;
	    data_base64: string;
	    content_type: string;

	    static createFrom(source: any = {}) {
	        return new EmojiData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class EmojiEntry {
	    name: string;
	    file_name: string;

	    static createFrom(source: any = {}) {
	        return new EmojiEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.file_name = source["file_name"];
	    }
	}
	export class EmojiListResponse {
	    emojis: EmojiEntry[];

	    static createFrom(source: any = {}) {
	        return new EmojiListResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.emojis = this.convertValues(source["emojis"], EmojiEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FileEntry {
	    name: string;
	    folder?: string;
	    size: number;
	    sha256: string;
	    uploader?: string;
	    // Go type: time
	    uploaded_at: any;
	    encrypted?: boolean;

	    static createFrom(source: any = {}) {
	        return new FileEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.folder = source["folder"];
	        this.size = source["size"];
	        this.sha256 = source["sha256"];
	        this.uploader = source["uploader"];
	        this.uploaded_at = this.convertValues(source["uploaded_at"], null);
	        this.encrypted = source["encrypted"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FileLinkResponse {
	    path: string;
	    health_port: number;
	    expires_at: number;

	    static createFrom(source: any = {}) {
	        return new FileLinkResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.health_port = source["health_port"];
	        this.expires_at = source["expires_at"];
	    }
	}
	export class FileListResponse {
	    entries: FileEntry[];
	    folders?: string[];
	    used_bytes: number;
	    quota_bytes: number;

	    static createFrom(source: any = {}) {
	        return new FileListResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], FileEntry);
	        this.folders = source["folders"];
	        this.used_bytes = source["used_bytes"];
	        this.quota_bytes = source["quota_bytes"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class FileVersionsResponse {
	    entries: FileEntry[];

	    static createFrom(source: any = {}) {
	        return new FileVersionsResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], FileEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GroupEntry {
	    id: number;
	    name: string;
	    sort_id: number;
	    member_count: number;
	    icon?: string;
	    color?: string;
	    hoist?: boolean;

	    static createFrom(source: any = {}) {
	        return new GroupEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.sort_id = source["sort_id"];
	        this.member_count = source["member_count"];
	        this.icon = source["icon"];
	        this.color = source["color"];
	        this.hoist = source["hoist"];
	    }
	}
	export class GroupIconData {
	    group_id: number;
	    data_base64?: string;
	    content_type?: string;

	    static createFrom(source: any = {}) {
	        return new GroupIconData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.group_id = source["group_id"];
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class GroupListResponse {
	    type: string;
	    groups: GroupEntry[];

	    static createFrom(source: any = {}) {
	        return new GroupListResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.groups = this.convertValues(source["groups"], GroupEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GroupMemberEntry {
	    unique_id: string;
	    nickname?: string;
	    expires_at?: number;

	    static createFrom(source: any = {}) {
	        return new GroupMemberEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.unique_id = source["unique_id"];
	        this.nickname = source["nickname"];
	        this.expires_at = source["expires_at"];
	    }
	}
	export class GroupMembersResponse {
	    type: string;
	    group_id: number;
	    members: GroupMemberEntry[];

	    static createFrom(source: any = {}) {
	        return new GroupMembersResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.group_id = source["group_id"];
	        this.members = this.convertValues(source["members"], GroupMemberEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ICEServer {
	    urls: string[];
	    username?: string;
	    credential?: string;

	    static createFrom(source: any = {}) {
	        return new ICEServer(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.urls = source["urls"];
	        this.username = source["username"];
	        this.credential = source["credential"];
	    }
	}
	export class PermissionEntry {
	    key: string;
	    value: number;
	    grant: number;
	    skip?: boolean;
	    negate?: boolean;
	    source_tier?: string;
	    inherited?: boolean;

	    static createFrom(source: any = {}) {
	        return new PermissionEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.value = source["value"];
	        this.grant = source["grant"];
	        this.skip = source["skip"];
	        this.negate = source["negate"];
	        this.source_tier = source["source_tier"];
	        this.inherited = source["inherited"];
	    }
	}
	export class PermListResponse {
	    tier: string;
	    entries: PermissionEntry[];

	    static createFrom(source: any = {}) {
	        return new PermListResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.tier = source["tier"];
	        this.entries = this.convertValues(source["entries"], PermissionEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PermTraceEntry {
	    tier: string;
	    present: boolean;
	    value: number;
	    grant: number;
	    skip: boolean;
	    negate: boolean;
	    winning?: boolean;

	    static createFrom(source: any = {}) {
	        return new PermTraceEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.tier = source["tier"];
	        this.present = source["present"];
	        this.value = source["value"];
	        this.grant = source["grant"];
	        this.skip = source["skip"];
	        this.negate = source["negate"];
	        this.winning = source["winning"];
	    }
	}
	export class PermTraceResponse {
	    key: string;
	    effective: number;
	    effective_tier: string;
	    entries: PermTraceEntry[];

	    static createFrom(source: any = {}) {
	        return new PermTraceResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.effective = source["effective"];
	        this.effective_tier = source["effective_tier"];
	        this.entries = this.convertValues(source["entries"], PermTraceEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class ServerBannerData {
	    data_base64: string;
	    content_type?: string;

	    static createFrom(source: any = {}) {
	        return new ServerBannerData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class ServerConfig {
	    max_clients: number;
	    client_timeout_seconds: number;
	    opus_bitrate: number;
	    opus_fec: boolean;
	    opus_dtx: boolean;
	    opus_stereo: boolean;

	    static createFrom(source: any = {}) {
	        return new ServerConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.max_clients = source["max_clients"];
	        this.client_timeout_seconds = source["client_timeout_seconds"];
	        this.opus_bitrate = source["opus_bitrate"];
	        this.opus_fec = source["opus_fec"];
	        this.opus_dtx = source["opus_dtx"];
	        this.opus_stereo = source["opus_stereo"];
	    }
	}
	export class ServerIconData {
	    data_base64?: string;
	    content_type?: string;

	    static createFrom(source: any = {}) {
	        return new ServerIconData(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.data_base64 = source["data_base64"];
	        this.content_type = source["content_type"];
	    }
	}
	export class ServerInfoResponse {
	    name: string;
	    version: string;
	    uptime_seconds: number;
	    clients_online: number;
	    channels_online: number;
	    max_clients: number;
	    motd?: string;

	    static createFrom(source: any = {}) {
	        return new ServerInfoResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.version = source["version"];
	        this.uptime_seconds = source["uptime_seconds"];
	        this.clients_online = source["clients_online"];
	        this.channels_online = source["channels_online"];
	        this.max_clients = source["max_clients"];
	        this.motd = source["motd"];
	    }
	}
	export class TokenEntry {
	    token: string;
	    group_id: number;
	    group_name?: string;
	    channel_id?: number;
	    description?: string;
	    created_at: number;
	    used_by?: string;

	    static createFrom(source: any = {}) {
	        return new TokenEntry(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.token = source["token"];
	        this.group_id = source["group_id"];
	        this.group_name = source["group_name"];
	        this.channel_id = source["channel_id"];
	        this.description = source["description"];
	        this.created_at = source["created_at"];
	        this.used_by = source["used_by"];
	    }
	}
	export class Tokens {
	    entries: TokenEntry[];

	    static createFrom(source: any = {}) {
	        return new Tokens(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.entries = this.convertValues(source["entries"], TokenEntry);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TrackSlot {
	    track_id: string;
	    slot: string;

	    static createFrom(source: any = {}) {
	        return new TrackSlot(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.track_id = source["track_id"];
	        this.slot = source["slot"];
	    }
	}

}

