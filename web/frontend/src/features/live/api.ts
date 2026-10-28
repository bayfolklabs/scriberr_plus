import { useAuthStore } from "@/features/auth/store/authStore";

// HTTP client for /api/v1/live (see LIVE_DESIGN.md, "HTTP API").
// Requests carry the same Bearer header as the rest of the app. The global
// fetch interceptor still refreshes the token and replays the request on a 401;
// Blob bodies are safe to replay.

const LIVE_API = "/api/v1/live";

const CHUNK_TIMEOUT_MS = 30_000;
const STATE_TIMEOUT_MS = 10_000;
const CONTROL_TIMEOUT_MS = 30_000;

export type LiveServerStatus = "none" | "recording" | "interrupted" | "finalizing" | "finalized" | "failed";
export type AsrState = "starting" | "running" | "restarting" | "stopped" | "failed" | "unavailable";

export interface LiveSegment {
	seq: number;
	start: number;
	end: number;
	text: string;
}

export interface LivePartial {
	text: string;
	start: number;
}

export interface LiveAsrInfo {
	state: AsrState;
	processed_sec?: number;
	lag_sec?: number;
	engine?: string;
	restarts?: number;
	error?: string;
}

export interface LiveStateResponse {
	id: string;
	title?: string;
	live_status: LiveServerStatus;
	status?: string;
	next_seq: number;
	bytes?: number;
	received_audio_sec?: number;
	asr?: LiveAsrInfo | null;
	segments?: LiveSegment[] | null;
	partial?: LivePartial | null;
	auto_retranscribe?: boolean;
}

export interface CreateSessionRequest {
	title: string;
	mime_type: string;
	auto_retranscribe: boolean;
}

export interface CreateSessionResponse {
	id: string;
	chunk_ms?: number;
}

export interface ChunkAckResponse {
	ack: number;
	next_seq: number;
	duplicate?: boolean;
}

export interface LiveServerSession {
	id: string;
	title?: string;
	live_status: LiveServerStatus;
	status?: string;
	created_at?: string;
	updated_at?: string;
	next_seq?: number;
	bytes?: number;
}

type JsonBody = Record<string, unknown>;

export class LiveApiError extends Error {
	readonly status: number;
	readonly body: JsonBody | null;

	constructor(status: number, message: string, body: JsonBody | null) {
		super(message);
		this.name = "LiveApiError";
		this.status = status;
		this.body = body;
	}

	/** Machine-readable error code from the body, e.g. "gap", "closed", "missing_chunks". */
	get code(): string | undefined {
		const value = this.body?.error;
		return typeof value === "string" ? value : undefined;
	}

	get nextSeq(): number | undefined {
		const value = this.body?.next_seq;
		return typeof value === "number" ? value : undefined;
	}
}

/** Network failures, timeouts and 5xx/408/429/401 responses are worth retrying. Other 4xx are not. */
export function isRetryable(error: unknown): boolean {
	if (!(error instanceof LiveApiError)) return true;
	return error.status >= 500 || error.status === 408 || error.status === 429 || error.status === 401;
}

/** True when the server could not be reached at all, or answered with a 5xx. */
export function isServerDown(error: unknown): boolean {
	return !(error instanceof LiveApiError) || error.status >= 500;
}

export function describeError(error: unknown): string {
	if (error instanceof LiveApiError) return error.message;
	if (error instanceof DOMException && (error.name === "AbortError" || error.name === "TimeoutError")) {
		return "The server did not answer in time";
	}
	if (error instanceof TypeError) return "Cannot reach the server";
	if (error instanceof Error && error.message) return error.message;
	return "Request failed";
}

function authHeaders(): Record<string, string> {
	const token = useAuthStore.getState().token;
	return token ? { Authorization: `Bearer ${token}` } : {};
}

async function readJson(response: Response): Promise<JsonBody | null> {
	try {
		const data: unknown = await response.json();
		return data !== null && typeof data === "object" ? (data as JsonBody) : null;
	} catch {
		return null;
	}
}

function messageFor(status: number, body: JsonBody | null): string {
	const message = body?.message ?? body?.error;
	if (typeof message === "string" && message) return `${message} (HTTP ${status})`;
	return `HTTP ${status}`;
}

async function request<T>(path: string, init: RequestInit, timeoutMs: number): Promise<T> {
	const controller = new AbortController();
	const timer = window.setTimeout(() => controller.abort(), timeoutMs);
	try {
		const headers = new Headers(init.headers);
		for (const [key, value] of Object.entries(authHeaders())) {
			if (!headers.has(key)) headers.set(key, value);
		}
		const response = await fetch(`${LIVE_API}${path}`, {
			...init,
			headers,
			cache: "no-store",
			signal: controller.signal,
		});
		const body = await readJson(response);
		if (!response.ok) {
			throw new LiveApiError(response.status, messageFor(response.status, body), body);
		}
		return (body ?? {}) as T;
	} finally {
		window.clearTimeout(timer);
	}
}

const jsonInit = (method: string, payload: unknown): RequestInit => ({
	method,
	headers: { "Content-Type": "application/json" },
	body: JSON.stringify(payload),
});

const sessionPath = (id: string) => `/sessions/${encodeURIComponent(id)}`;

export function createSession(payload: CreateSessionRequest): Promise<CreateSessionResponse> {
	return request<CreateSessionResponse>("/sessions", jsonInit("POST", payload), CONTROL_TIMEOUT_MS).then((res) => {
		if (!res.id) throw new Error("The server did not return a session id");
		return res;
	});
}

export function uploadChunk(id: string, seq: number, blob: Blob): Promise<ChunkAckResponse> {
	return request<ChunkAckResponse>(
		`${sessionPath(id)}/chunks?seq=${seq}`,
		{ method: "POST", headers: { "Content-Type": "application/octet-stream" }, body: blob },
		CHUNK_TIMEOUT_MS,
	);
}

export function getSessionState(id: string, after: number): Promise<LiveStateResponse> {
	return request<LiveStateResponse>(`${sessionPath(id)}/state?after=${after}`, { method: "GET" }, STATE_TIMEOUT_MS);
}

export function stopSession(id: string, lastSeq: number): Promise<{ live_status?: LiveServerStatus }> {
	return request(`${sessionPath(id)}/stop`, jsonInit("POST", { last_seq: lastSeq }), CONTROL_TIMEOUT_MS);
}

export function finalizeSession(id: string): Promise<{ live_status?: LiveServerStatus }> {
	return request(`${sessionPath(id)}/finalize`, { method: "POST" }, CONTROL_TIMEOUT_MS);
}

/**
 * Lists the user's live sessions that are not finalized yet. The design doc does not pin the
 * response shape, so both a bare array and an object wrapping the array are accepted.
 */
export async function listServerSessions(statuses: LiveServerStatus[]): Promise<LiveServerSession[]> {
	const data = await request<unknown>(`/sessions?status=${statuses.join(",")}`, { method: "GET" }, STATE_TIMEOUT_MS);
	const wrapped = data as { sessions?: unknown; items?: unknown; jobs?: unknown };
	const list = Array.isArray(data) ? data : (wrapped.sessions ?? wrapped.items ?? wrapped.jobs ?? []);
	if (!Array.isArray(list)) return [];
	return list.filter((item): item is LiveServerSession => !!item && typeof (item as LiveServerSession).id === "string");
}
