// Durable browser-side copy of every live meeting (IndexedDB "scriberr-live").
// Each MediaRecorder chunk is written here before it is uploaded, and the chunks
// are deleted only after the server reports the meeting finalized. A crashed tab,
// a closed laptop or a dead network therefore never costs the recording.

export const LIVE_DB_NAME = "scriberr-live";
const DB_VERSION = 1;
const SESSIONS = "sessions";
const CHUNKS = "chunks";

/** Finalized session records are kept for a while (their chunks are already gone), then pruned. */
const FINALIZED_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

export type LocalSessionState = "recording" | "stopping" | "uploading" | "finalizing" | "finalized" | "failed";

export interface LocalSession {
	id: string;
	title: string;
	mimeType: string;
	createdAt: number;
	state: LocalSessionState;
	/** Highest seq stored in the chunks store, -1 when none. */
	lastSeq: number;
	/** Highest seq the server acknowledged, -1 when none. */
	ackedSeq: number;
	/** Server job id, null until the server session exists. */
	serverId: string | null;
	autoRetranscribe?: boolean;
	error?: string;
	updatedAt?: number;
}

export interface LocalChunk {
	sessionId: string;
	seq: number;
	blob: Blob;
	ts: number;
}

let dbPromise: Promise<IDBDatabase> | null = null;

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
	return new Promise((resolve, reject) => {
		request.onsuccess = () => resolve(request.result);
		request.onerror = () => reject(request.error ?? new Error("IndexedDB request failed"));
	});
}

function transactionDone(tx: IDBTransaction): Promise<void> {
	return new Promise((resolve, reject) => {
		tx.oncomplete = () => resolve();
		tx.onerror = () => reject(tx.error ?? new Error("IndexedDB transaction failed"));
		tx.onabort = () => reject(tx.error ?? new Error("IndexedDB transaction aborted"));
	});
}

function openDb(): Promise<IDBDatabase> {
	if (dbPromise) return dbPromise;
	const opening = new Promise<IDBDatabase>((resolve, reject) => {
		if (typeof indexedDB === "undefined") {
			reject(new Error("IndexedDB is not available in this browser"));
			return;
		}
		const request = indexedDB.open(LIVE_DB_NAME, DB_VERSION);
		request.onupgradeneeded = () => {
			const db = request.result;
			if (!db.objectStoreNames.contains(SESSIONS)) {
				db.createObjectStore(SESSIONS, { keyPath: "id" });
			}
			if (!db.objectStoreNames.contains(CHUNKS)) {
				db.createObjectStore(CHUNKS, { keyPath: ["sessionId", "seq"] });
			}
		};
		request.onsuccess = () => {
			const db = request.result;
			// Let a future schema upgrade in another tab proceed; the next call reopens.
			db.onversionchange = () => {
				db.close();
				dbPromise = null;
			};
			db.onclose = () => {
				dbPromise = null;
			};
			resolve(db);
		};
		request.onerror = () => reject(request.error ?? new Error("Could not open IndexedDB"));
	});
	dbPromise = opening;
	opening.catch(() => {
		if (dbPromise === opening) dbPromise = null;
	});
	return opening;
}

function chunkRange(sessionId: string): IDBKeyRange {
	return IDBKeyRange.bound([sessionId, 0], [sessionId, Number.MAX_SAFE_INTEGER]);
}

/** Ask the browser not to evict our data under storage pressure. Best effort. */
export async function requestPersistentStorage(): Promise<boolean> {
	try {
		if (navigator.storage?.persist) return await navigator.storage.persist();
	} catch {
		/* not supported or refused */
	}
	return false;
}

export async function putSession(session: LocalSession): Promise<void> {
	const db = await openDb();
	const tx = db.transaction(SESSIONS, "readwrite");
	tx.objectStore(SESSIONS).put({ ...session, updatedAt: Date.now() });
	await transactionDone(tx);
}

export async function getSession(id: string): Promise<LocalSession | undefined> {
	const db = await openDb();
	const tx = db.transaction(SESSIONS, "readonly");
	return requestResult(tx.objectStore(SESSIONS).get(id) as IDBRequest<LocalSession | undefined>);
}

export async function listSessions(): Promise<LocalSession[]> {
	const db = await openDb();
	const tx = db.transaction(SESSIONS, "readonly");
	const sessions = await requestResult(tx.objectStore(SESSIONS).getAll() as IDBRequest<LocalSession[]>);
	return sessions.sort((a, b) => b.createdAt - a.createdAt);
}

/** Read-modify-write of one session record inside a single transaction. */
export async function patchSession(id: string, patch: Partial<Omit<LocalSession, "id">>): Promise<void> {
	const db = await openDb();
	const tx = db.transaction(SESSIONS, "readwrite");
	const sessions = tx.objectStore(SESSIONS);
	const read = sessions.get(id) as IDBRequest<LocalSession | undefined>;
	read.onsuccess = () => {
		const current = read.result;
		if (current) sessions.put({ ...current, ...patch, id, updatedAt: Date.now() });
	};
	await transactionDone(tx);
}

/** Stores a chunk and advances the session's lastSeq atomically. */
export async function appendChunk(chunk: LocalChunk): Promise<void> {
	const db = await openDb();
	const tx = db.transaction([CHUNKS, SESSIONS], "readwrite");
	tx.objectStore(CHUNKS).put(chunk);
	const sessions = tx.objectStore(SESSIONS);
	const read = sessions.get(chunk.sessionId) as IDBRequest<LocalSession | undefined>;
	read.onsuccess = () => {
		const current = read.result;
		if (current && chunk.seq > current.lastSeq) {
			sessions.put({ ...current, lastSeq: chunk.seq, updatedAt: Date.now() });
		}
	};
	await transactionDone(tx);
}

export async function getChunk(sessionId: string, seq: number): Promise<Blob | null> {
	const db = await openDb();
	const tx = db.transaction(CHUNKS, "readonly");
	const chunk = await requestResult(tx.objectStore(CHUNKS).get([sessionId, seq]) as IDBRequest<LocalChunk | undefined>);
	return chunk?.blob ?? null;
}

/** All chunk blobs of a session in seq order. */
export async function readChunks(sessionId: string): Promise<Blob[]> {
	const db = await openDb();
	const tx = db.transaction(CHUNKS, "readonly");
	const chunks = await requestResult(tx.objectStore(CHUNKS).getAll(chunkRange(sessionId)) as IDBRequest<LocalChunk[]>);
	return chunks.sort((a, b) => a.seq - b.seq).map((chunk) => chunk.blob);
}

export async function deleteChunks(sessionId: string): Promise<void> {
	const db = await openDb();
	const tx = db.transaction(CHUNKS, "readwrite");
	tx.objectStore(CHUNKS).delete(chunkRange(sessionId));
	await transactionDone(tx);
}

export async function deleteSession(id: string): Promise<void> {
	const db = await openDb();
	const tx = db.transaction([CHUNKS, SESSIONS], "readwrite");
	tx.objectStore(CHUNKS).delete(chunkRange(id));
	tx.objectStore(SESSIONS).delete(id);
	await transactionDone(tx);
}

/** Removes finalized session records older than the retention window. */
export async function pruneFinalizedSessions(): Promise<void> {
	const cutoff = Date.now() - FINALIZED_RETENTION_MS;
	const sessions = await listSessions();
	const stale = sessions.filter((s) => s.state === "finalized" && (s.updatedAt ?? s.createdAt) < cutoff);
	for (const session of stale) {
		await deleteSession(session.id);
	}
}

function extensionFor(mimeType: string): string {
	if (mimeType.includes("ogg")) return "ogg";
	if (mimeType.includes("mp4") || mimeType.includes("aac")) return "m4a";
	return "webm";
}

function safeFileName(title: string): string {
	const cleaned = title.replace(/[\\/:*?"<>|]+/g, " ").replace(/\s+/g, " ").trim();
	return cleaned.slice(0, 120) || "live-meeting";
}

/**
 * Concatenates the stored chunks into one file and hands it to the browser as a download.
 * MediaRecorder chunks joined in order form a valid (if unseekable) WebM file.
 */
export async function downloadLocalRecording(session: Pick<LocalSession, "id" | "title" | "mimeType">): Promise<void> {
	const blobs = await readChunks(session.id);
	if (blobs.length === 0) throw new Error("No audio is saved in this browser for this meeting");
	const type = session.mimeType.split(";")[0] || "audio/webm";
	const recording = new Blob(blobs, { type });
	const url = URL.createObjectURL(recording);
	const link = document.createElement("a");
	link.href = url;
	link.download = `${safeFileName(session.title)}.${extensionFor(type)}`;
	document.body.appendChild(link);
	link.click();
	link.remove();
	window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
