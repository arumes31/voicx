export namespace main {
	
	export class Bookmark {
	    name: string;
	    addr: string;
	    nickname: string;
	
	    static createFrom(source: any = {}) {
	        return new Bookmark(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.addr = source["addr"];
	        this.nickname = source["nickname"];
	    }
	}
	export class Settings {
	    bookmarks: Bookmark[];
	    capture_device_id: string;
	    activation_mode: string;
	    vad_threshold: number;
	    echo_cancellation: boolean;
	    noise_suppression: boolean;
	    playback_device_id: string;
	    volume: number;
	    hotkey_ptt: string;
	    hotkey_mute: string;
	    chat_max_lines: number;
	    log_channel_chat: boolean;
	    log_private_chat: boolean;
	    log_server_chat: boolean;
	    notify_join_leave: boolean;
	    notify_connection: boolean;
	    play_sounds: boolean;
	    whisper_sound: boolean;
	    whisper_clients: string[];
	    whisper_channels: number[];
	    whisper_active: boolean;
	    download_folder: string;
	    reconnect_on_loss: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bookmarks = this.convertValues(source["bookmarks"], Bookmark);
	        this.capture_device_id = source["capture_device_id"];
	        this.activation_mode = source["activation_mode"];
	        this.vad_threshold = source["vad_threshold"];
	        this.echo_cancellation = source["echo_cancellation"];
	        this.noise_suppression = source["noise_suppression"];
	        this.playback_device_id = source["playback_device_id"];
	        this.volume = source["volume"];
	        this.hotkey_ptt = source["hotkey_ptt"];
	        this.hotkey_mute = source["hotkey_mute"];
	        this.chat_max_lines = source["chat_max_lines"];
	        this.log_channel_chat = source["log_channel_chat"];
	        this.log_private_chat = source["log_private_chat"];
	        this.log_server_chat = source["log_server_chat"];
	        this.notify_join_leave = source["notify_join_leave"];
	        this.notify_connection = source["notify_connection"];
	        this.play_sounds = source["play_sounds"];
	        this.whisper_sound = source["whisper_sound"];
	        this.whisper_clients = source["whisper_clients"];
	        this.whisper_channels = source["whisper_channels"];
	        this.whisper_active = source["whisper_active"];
	        this.download_folder = source["download_folder"];
	        this.reconnect_on_loss = source["reconnect_on_loss"];
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
	export class PermissionEntry {
	    key: string;
	    value: number;
	    grant: number;
	    skip?: boolean;
	    negate?: boolean;
	
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
	    }
	}

}

