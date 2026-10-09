export namespace main {
	
	export class AccountSummary {
	    slug: string;
	    username: string;
	    has_avatar: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AccountSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.slug = source["slug"];
	        this.username = source["username"];
	        this.has_avatar = source["has_avatar"];
	    }
	}
	export class AccountView {
	    username: string;
	    fingerprint: string;
	    has_avatar: boolean;
	    imported_settings?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AccountView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.username = source["username"];
	        this.fingerprint = source["fingerprint"];
	        this.has_avatar = source["has_avatar"];
	        this.imported_settings = source["imported_settings"];
	    }
	}
	export class Attachment {
	    url: string;
	    name: string;
	    mime: string;
	    spoiler?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Attachment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.name = source["name"];
	        this.mime = source["mime"];
	        this.spoiler = source["spoiler"];
	    }
	}
	export class Board {
	    id: string;
	    room_id: string;
	    name: string;
	    is_private: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Board(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.room_id = source["room_id"];
	        this.name = source["name"];
	        this.is_private = source["is_private"];
	    }
	}
	export class CameraDevice {
	    id: string;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new CameraDevice(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	    }
	}
	export class CameraMode {
	    w: number;
	    h: number;
	    fps: number;
	    format: string;
	
	    static createFrom(source: any = {}) {
	        return new CameraMode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.w = source["w"];
	        this.h = source["h"];
	        this.fps = source["fps"];
	        this.format = source["format"];
	    }
	}
	export class CameraStart {
	    id: string;
	    w: number;
	    h: number;
	    fps: number;
	    modeFps: number;
	    format: string;
	    send: boolean;
	    preview: boolean;
	    encoder: string;
	
	    static createFrom(source: any = {}) {
	        return new CameraStart(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.w = source["w"];
	        this.h = source["h"];
	        this.fps = source["fps"];
	        this.modeFps = source["modeFps"];
	        this.format = source["format"];
	        this.send = source["send"];
	        this.preview = source["preview"];
	        this.encoder = source["encoder"];
	    }
	}
	export class MessageZip {
	    url: string;
	    name: string;
	    size: number;
	
	    static createFrom(source: any = {}) {
	        return new MessageZip(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.name = source["name"];
	        this.size = source["size"];
	    }
	}
	export class ReplyPreview {
	    id: string;
	    username: string;
	    content: string;
	    attachments: number;
	    deleted: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ReplyPreview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.username = source["username"];
	        this.content = source["content"];
	        this.attachments = source["attachments"];
	        this.deleted = source["deleted"];
	    }
	}
	export class Reaction {
	    emoji: string;
	    count: number;
	    users: string[];
	
	    static createFrom(source: any = {}) {
	        return new Reaction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.emoji = source["emoji"];
	        this.count = source["count"];
	        this.users = source["users"];
	    }
	}
	export class ChatMessage {
	    id: string;
	    board_id: string;
	    username: string;
	    content: string;
	    attachments: Attachment[];
	    edited: boolean;
	    created_at: string;
	    pinned: boolean;
	    reactions: Reaction[];
	    reply_to?: string;
	    reply?: ReplyPreview;
	    kind?: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.board_id = source["board_id"];
	        this.username = source["username"];
	        this.content = source["content"];
	        this.attachments = this.convertValues(source["attachments"], Attachment);
	        this.edited = source["edited"];
	        this.created_at = source["created_at"];
	        this.pinned = source["pinned"];
	        this.reactions = this.convertValues(source["reactions"], Reaction);
	        this.reply_to = source["reply_to"];
	        this.reply = this.convertValues(source["reply"], ReplyPreview);
	        this.kind = source["kind"];
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
	export class ConnectionSecurity {
	    https: boolean;
	    tls_version?: string;
	    issuer?: string;
	    subject?: string;
	    expires_at?: string;
	
	    static createFrom(source: any = {}) {
	        return new ConnectionSecurity(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.https = source["https"];
	        this.tls_version = source["tls_version"];
	        this.issuer = source["issuer"];
	        this.subject = source["subject"];
	        this.expires_at = source["expires_at"];
	    }
	}
	export class CustomEmoji {
	    id: string;
	    name: string;
	    animated: boolean;
	    hidden: boolean;
	    created_by: string;
	
	    static createFrom(source: any = {}) {
	        return new CustomEmoji(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.animated = source["animated"];
	        this.hidden = source["hidden"];
	        this.created_by = source["created_by"];
	    }
	}
	export class EmojiFile {
	    path: string;
	    file: string;
	    name: string;
	    size: number;
	    preview: string;
	
	    static createFrom(source: any = {}) {
	        return new EmojiFile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.file = source["file"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.preview = source["preview"];
	    }
	}
	export class EncoderChoice {
	    id: string;
	    label: string;
	    available: boolean;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new EncoderChoice(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.available = source["available"];
	        this.reason = source["reason"];
	    }
	}
	export class InviteInfo {
	    code: string;
	    expires_at: string;
	    minutes: number;
	
	    static createFrom(source: any = {}) {
	        return new InviteInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.expires_at = source["expires_at"];
	        this.minutes = source["minutes"];
	    }
	}
	export class LinkPreview {
	    url: string;
	    title?: string;
	    description?: string;
	    image?: string;
	    site_name?: string;
	    kind?: string;
	    video?: string;
	
	    static createFrom(source: any = {}) {
	        return new LinkPreview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.image = source["image"];
	        this.site_name = source["site_name"];
	        this.kind = source["kind"];
	        this.video = source["video"];
	    }
	}
	export class OwnProfile {
	    nickname: string;
	    bio: string;
	    banner: string;
	    tint: string;
	
	    static createFrom(source: any = {}) {
	        return new OwnProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.nickname = source["nickname"];
	        this.bio = source["bio"];
	        this.banner = source["banner"];
	        this.tint = source["tint"];
	    }
	}
	
	
	export class Room {
	    id: string;
	    name: string;
	    is_private: boolean;
	    room_type: string;
	
	    static createFrom(source: any = {}) {
	        return new Room(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.is_private = source["is_private"];
	        this.room_type = source["room_type"];
	    }
	}
	export class SavedServer {
	    domain: string;
	    server_key: string;
	    display_name: string;
	    last_username: string;
	    custom_name?: string;
	
	    static createFrom(source: any = {}) {
	        return new SavedServer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.domain = source["domain"];
	        this.server_key = source["server_key"];
	        this.display_name = source["display_name"];
	        this.last_username = source["last_username"];
	        this.custom_name = source["custom_name"];
	    }
	}
	export class ScreenStart {
	    id: string;
	    height: number;
	    fps: number;
	    encoder: string;
	    preview: boolean;
	    audio: boolean;
	    hideBorder: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ScreenStart(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.height = source["height"];
	        this.fps = source["fps"];
	        this.encoder = source["encoder"];
	        this.preview = source["preview"];
	        this.audio = source["audio"];
	        this.hideBorder = source["hideBorder"];
	    }
	}
	export class SearchResult {
	    id: string;
	    board_id: string;
	    username: string;
	    content: string;
	    attachments: Attachment[];
	    edited: boolean;
	    created_at: string;
	    pinned: boolean;
	    reactions: Reaction[];
	    reply_to?: string;
	    reply?: ReplyPreview;
	    kind?: string;
	    board_name: string;
	    room_id: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.board_id = source["board_id"];
	        this.username = source["username"];
	        this.content = source["content"];
	        this.attachments = this.convertValues(source["attachments"], Attachment);
	        this.edited = source["edited"];
	        this.created_at = source["created_at"];
	        this.pinned = source["pinned"];
	        this.reactions = this.convertValues(source["reactions"], Reaction);
	        this.reply_to = source["reply_to"];
	        this.reply = this.convertValues(source["reply"], ReplyPreview);
	        this.kind = source["kind"];
	        this.board_name = source["board_name"];
	        this.room_id = source["room_id"];
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
	export class ServerTheme {
	    base: string;
	    accent: string;
	    blur: number;
	    dim: number;
	    bg_updated_at: number;
	
	    static createFrom(source: any = {}) {
	        return new ServerTheme(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.base = source["base"];
	        this.accent = source["accent"];
	        this.blur = source["blur"];
	        this.dim = source["dim"];
	        this.bg_updated_at = source["bg_updated_at"];
	    }
	}
	export class ServerInfo {
	    name: string;
	    requires_key: boolean;
	    description: string;
	    banner_updated_at: number;
	    icon_updated_at: number;
	    theme?: ServerTheme;
	    owner: string;
	
	    static createFrom(source: any = {}) {
	        return new ServerInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.requires_key = source["requires_key"];
	        this.description = source["description"];
	        this.banner_updated_at = source["banner_updated_at"];
	        this.icon_updated_at = source["icon_updated_at"];
	        this.theme = this.convertValues(source["theme"], ServerTheme);
	        this.owner = source["owner"];
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
	
	export class ShareSource {
	    id: string;
	    kind: string;
	    name: string;
	    app: string;
	    w: number;
	    h: number;
	    primary: boolean;
	    index: number;
	    thumb: string;
	    note: string;
	
	    static createFrom(source: any = {}) {
	        return new ShareSource(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.app = source["app"];
	        this.w = source["w"];
	        this.h = source["h"];
	        this.primary = source["primary"];
	        this.index = source["index"];
	        this.thumb = source["thumb"];
	        this.note = source["note"];
	    }
	}
	export class Sticker {
	    id: string;
	    name: string;
	    description: string;
	    animated: boolean;
	    hidden: boolean;
	    created_by: string;
	
	    static createFrom(source: any = {}) {
	        return new Sticker(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.animated = source["animated"];
	        this.hidden = source["hidden"];
	        this.created_by = source["created_by"];
	    }
	}
	export class UpdateStatus {
	    available: boolean;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new UpdateStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.version = source["version"];
	    }
	}
	export class UploadResult {
	    url: string;
	    filename: string;
	    mime: string;
	
	    static createFrom(source: any = {}) {
	        return new UploadResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.filename = source["filename"];
	        this.mime = source["mime"];
	    }
	}
	export class UserProfile {
	    username: string;
	    nickname: string;
	    global_nickname: string;
	    bio: string;
	    tint: string;
	    banner: string;
	    member_since: string;
	
	    static createFrom(source: any = {}) {
	        return new UserProfile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.username = source["username"];
	        this.nickname = source["nickname"];
	        this.global_nickname = source["global_nickname"];
	        this.bio = source["bio"];
	        this.tint = source["tint"];
	        this.banner = source["banner"];
	        this.member_since = source["member_since"];
	    }
	}

}

