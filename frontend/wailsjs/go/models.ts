export namespace main {
	
	export class Board {
	    id: string;
	    room_id: string;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new Board(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.room_id = source["room_id"];
	        this.name = source["name"];
	    }
	}
	export class Room {
	    id: string;
	    name: string;
	    is_private: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Room(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.is_private = source["is_private"];
	    }
	}
	export class SavedServer {
	    domain: string;
	    server_key: string;
	    display_name: string;
	    last_username: string;
	
	    static createFrom(source: any = {}) {
	        return new SavedServer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.domain = source["domain"];
	        this.server_key = source["server_key"];
	        this.display_name = source["display_name"];
	        this.last_username = source["last_username"];
	    }
	}
	export class ServerInfo {
	    name: string;
	    requires_key: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ServerInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.requires_key = source["requires_key"];
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

}

