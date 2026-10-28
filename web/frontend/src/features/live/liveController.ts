import {
	LiveApiError,
	createSession,
	getSessionState,
	isServerDown,
	describeError,
	finalizeSession,
	stopSession,
	type LiveAsrInfo,
	type LivePartial,
	type LiveSegment,
	type LiveServerStatus,
	type LiveStateResponse,
} from "./api";
import * as store from "./chunkStore";
import { ChunkUploader, RETRY_MAX_MS, RETRY_START_MS, type UploaderStatus } from "./uploader";
import {
	AUDIO_BITS_PER_SECOND,
	AudioMix,
	TIMESLICE_MS,
	captureMicrophone,
	captureSystemAudio,
	checkSupport,
	pickMimeType,
	stopStream,
	usesMic,
	usesSystemAudio,
	type InputKind,
	type LiveSource,
} from "./media";
import { defaultMeetingTitle, errorMessage } from "./format";

// One live meeting at a time, owned by a module-level controller rather than by a
// component, so route changes, remounts and even a logout never stop the recorder.
// React reads it through useSyncExternalStore (see liveSessionContext.ts).
//
// Flow: capture -> AudioMix -> MediaRecorder (1 s chunks) -> IndexedDB -> ChunkUploader
// -> server. A poller fetches live text once per second. Stop drains the uploader,
// POSTs stop, then waits for live_status "finalized" before deleting local chunks.

const POLL_MS = 1000;
const LOCK_PREFIX = "scriberr-live:";

export type LivePhase =
	| "idle"
	| "starting"
	| "recording"
	| "draining" // recorder stopped, uploading what is left
	| "finalizing" // server accepted stop, waiting for live_status "finalized"
	| "finalized"
	| "failed";

export interface LiveStartOptions {
	title: string;
	source: LiveSource;
	micDeviceId: string;
	autoRetranscribe: boolean;
}

export interface LiveNotice {
	title: string;
	description?: string;
}

export interface LiveSnapshot {
	phase: LivePhase;
	localId: string | null;
	serverId: string | null;
	title: string;
	source: LiveSource | null;
	mimeType: string;
	autoRetranscribe: boolean;
	/** Continued from the recovery panel: there is no recorder, only an upload to finish. */
	resumed: boolean;
	startedAt: number | null;
	stoppedAt: number | null;
	recordedChunks: number;
	uploadedChunks: number;
	localSaveFailed: boolean;
	upload: UploaderStatus | null;
	serverReachable: boolean;
	pollError: string | null;
	segments: LiveSegment[];
	partial: LivePartial | null;
	asr: LiveAsrInfo | null;
	liveStatus: LiveServerStatus | null;
	receivedAudioSec: number | null;
	systemAudioLost: boolean;
	micLost: boolean;
	analyser: AnalyserNode | null;
	/** Start error or terminal failure, shown to the user. */
	error: string | null;
	/** Neutral message, e.g. after keeping a meeting in the browser for later. */
	info: string | null;
	/** The server finalized with fewer chunks than this browser holds; local copy kept. */
	incompleteOnServer: boolean;
	/** Unfinished meetings saved in this browser (not counting the active one). */
	recoverableCount: number;
}

const ACTIVE_PHASES: LivePhase[] = ["starting", "recording", "draining", "finalizing"];

export const isActivePhase = (phase: LivePhase) => ACTIVE_PHASES.includes(phase);
const isPollingPhase = (phase: LivePhase) => phase === "recording" || phase === "draining" || phase === "finalizing";
const noop = () => undefined;

function initialSnapshot(): LiveSnapshot {
	return {
		phase: "idle",
		localId: null,
		serverId: null,
		title: "",
		source: null,
		mimeType: "",
		autoRetranscribe: false,
		resumed: false,
		startedAt: null,
		stoppedAt: null,
		recordedChunks: 0,
		uploadedChunks: 0,
		localSaveFailed: false,
		upload: null,
		serverReachable: true,
		pollError: null,
		segments: [],
		partial: null,
		asr: null,
		liveStatus: null,
		receivedAudioSec: null,
		systemAudioLost: false,
		micLost: false,
		analyser: null,
		error: null,
		info: null,
		incompleteOnServer: false,
		recoverableCount: 0,
	};
}

function newLocalId(): string {
	if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return crypto.randomUUID();
	return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

const lockName = (localId: string) => `${LOCK_PREFIX}${localId}`;

/**
 * Holds a Web Lock for the session while this tab works on it, so another tab's recovery
 * panel can tell an active meeting from an abandoned one. Resolves null if another tab holds it.
 */
function acquireSessionLock(localId: string): Promise<(() => void) | null> {
	const locks = typeof navigator !== "undefined" ? navigator.locks : undefined;
	if (!locks) return Promise.resolve(noop);
	return new Promise((resolve) => {
		locks
			.request(lockName(localId), { ifAvailable: true }, (lock) => {
				if (!lock) {
					resolve(null);
					return undefined;
				}
				return new Promise<void>((release) => resolve(() => release()));
			})
			.catch(() => resolve(noop));
	});
}

/** Names of live-session locks currently held by any tab of this origin. */
export async function heldSessionLocks(): Promise<Set<string>> {
	const locks = typeof navigator !== "undefined" ? navigator.locks : undefined;
	if (!locks?.query) return new Set();
	try {
		const state = await locks.query();
		return new Set(
			(state.held ?? [])
				.map((lock) => lock.name ?? "")
				.filter((name) => name.startsWith(LOCK_PREFIX))
				.map((name) => name.slice(LOCK_PREFIX.length)),
		);
	} catch {
		return new Set();
	}
}

function withTimeout(promise: Promise<void>, ms: number): Promise<void> {
	return new Promise((resolve) => {
		const timer = window.setTimeout(resolve, ms);
		const done = () => {
			window.clearTimeout(timer);
			resolve();
		};
		promise.then(done, done);
	});
}

export class LiveSessionController {
	private snapshot: LiveSnapshot = initialSnapshot();
	private readonly listeners = new Set<() => void>();
	private readonly noticeListeners = new Set<(notice: LiveNotice) => void>();
	/** Bumped for every new session; async work from an older session checks it and bails. */
	private runId = 0;

	private localId: string | null = null;
	private recorder: MediaRecorder | null = null;
	private recorderStopped: Promise<void> | null = null;
	private stopRequested = false;
	private mix: AudioMix | null = null;
	private nextSeq = 0;
	private lastSeq = -1;
	private persistChain: Promise<void> = Promise.resolve();
	/** Chunks that could not be written to IndexedDB, kept in memory until acknowledged. */
	private readonly memoryChunks = new Map<number, Blob>();

	private uploader: ChunkUploader | null = null;
	private pollTimer: number | null = null;
	private pollInFlight = false;
	private lastSegSeq = -1;
	private readonly segmentSeqs = new Set<number>();
	private finalHandled = false;
	private retryWaker: (() => void) | null = null;
	private ackToPersist: { localId: string; seq: number } | null = null;
	private ackPersisting = false;

	private releaseLock: (() => void) | null = null;
	private wakeLock: WakeLockSentinel | null = null;
	private wakeLockPending = false;
	private windowListeners = false;
	private originalTitle: string | null = null;

	// ----- store plumbing --------------------------------------------------------------

	subscribe = (listener: () => void): (() => void) => {
		this.listeners.add(listener);
		return () => {
			this.listeners.delete(listener);
		};
	};

	getSnapshot = (): LiveSnapshot => this.snapshot;

	onNotice = (listener: (notice: LiveNotice) => void): (() => void) => {
		this.noticeListeners.add(listener);
		return () => {
			this.noticeListeners.delete(listener);
		};
	};

	private set(patch: Partial<LiveSnapshot>): void {
		this.snapshot = { ...this.snapshot, ...patch };
		this.listeners.forEach((listener) => listener());
	}

	private notify(notice: LiveNotice): void {
		this.noticeListeners.forEach((listener) => listener(notice));
	}

	private resetSession(patch: Partial<LiveSnapshot>): void {
		this.localId = null;
		this.recorder = null;
		this.recorderStopped = null;
		this.stopRequested = false;
		this.nextSeq = 0;
		this.lastSeq = -1;
		this.persistChain = Promise.resolve();
		this.memoryChunks.clear();
		this.lastSegSeq = -1;
		this.segmentSeqs.clear();
		this.finalHandled = false;
		this.snapshot = { ...initialSnapshot(), recoverableCount: this.snapshot.recoverableCount, ...patch };
		this.listeners.forEach((listener) => listener());
	}

	// ----- public actions ------------------------------------------------------------

	start = async (options: LiveStartOptions): Promise<void> => {
		if (isActivePhase(this.snapshot.phase)) return;
		const support = checkSupport(options.source);
		if (support.error) {
			this.set({ error: support.error, info: null });
			return;
		}

		const run = ++this.runId;
		this.teardown();
		const title = options.title.trim() || defaultMeetingTitle();
		const autoRetranscribe = options.autoRetranscribe;
		this.resetSession({ phase: "starting", title, source: options.source, autoRetranscribe });

		const acquired: MediaStream[] = [];
		let mix: AudioMix | null = null;
		let localId: string | null = null;
		let releaseLock: (() => void) | null = null;
		try {
			// Computer audio first: getDisplayMedia needs the user activation of the click.
			let systemStream: MediaStream | null = null;
			if (usesSystemAudio(options.source)) {
				systemStream = await captureSystemAudio();
				acquired.push(systemStream);
			}
			let micStream: MediaStream | null = null;
			if (usesMic(options.source)) {
				try {
					micStream = await captureMicrophone(options.micDeviceId, systemStream !== null);
					acquired.push(micStream);
				} catch (error) {
					if (!systemStream) throw error;
					this.notify({ title: "Microphone unavailable", description: `${errorMessage(error)} Recording computer audio only.` });
				}
			}
			if (run !== this.runId) throw new Error("Start was cancelled.");

			mix = new AudioMix();
			this.mix = mix;
			if (systemStream) this.attachInput(run, "system", systemStream);
			if (micStream) this.attachInput(run, "mic", micStream);
			acquired.length = 0; // the mix owns the streams now
			mix.context.onstatechange = () => this.handleContextState(run);
			await mix.resume();

			const preferred = pickMimeType();
			const recorder = new MediaRecorder(
				mix.stream,
				preferred
					? { mimeType: preferred, audioBitsPerSecond: AUDIO_BITS_PER_SECOND }
					: { audioBitsPerSecond: AUDIO_BITS_PER_SECOND },
			);
			const mimeType = recorder.mimeType || preferred || "audio/webm";

			localId = newLocalId();
			releaseLock = await acquireSessionLock(localId);
			let localSaveFailed = false;
			try {
				await store.putSession({
					id: localId,
					title,
					mimeType,
					createdAt: Date.now(),
					state: "recording",
					lastSeq: -1,
					ackedSeq: -1,
					serverId: null,
					autoRetranscribe,
				});
			} catch (error) {
				console.error("Live: could not create the local session", error);
				localSaveFailed = true;
			}
			void store.requestPersistentStorage();
			if (run !== this.runId) throw new Error("Start was cancelled.");

			this.localId = localId;
			this.releaseLock = releaseLock;
			this.recorder = recorder;
			recorder.ondataavailable = (event) => this.handleChunk(run, event.data);
			recorder.onerror = (event) => console.error("Live: MediaRecorder error", event);
			this.recorderStopped = new Promise((resolve) => {
				recorder.addEventListener("stop", () => resolve(), { once: true });
			});
			recorder.addEventListener("stop", () => this.handleRecorderStopped(run));
			// Recording starts before the server is involved: the meeting never waits on the network.
			recorder.start(TIMESLICE_MS);

			const source: LiveSource = systemStream && micStream ? "mic+system" : systemStream ? "system" : "mic";
			this.set({
				phase: "recording",
				localId,
				mimeType,
				source,
				startedAt: Date.now(),
				localSaveFailed,
				analyser: mix.analyser,
			});
			this.activateEnvironment();

			this.uploader = this.createUploader(run, {
				localId,
				serverId: null,
				startSeq: 0,
				availableSeq: -1,
				title,
				mimeType,
				autoRetranscribe,
			});
			this.uploader.start();
			this.startPolling(run);
		} catch (error) {
			acquired.forEach(stopStream);
			if (mix) {
				mix.context.onstatechange = null;
				mix.close();
				if (this.mix === mix) this.mix = null;
			}
			if (releaseLock) {
				releaseLock();
				if (this.releaseLock === releaseLock) this.releaseLock = null;
			}
			if (localId) await store.deleteSession(localId).catch(noop);
			if (run === this.runId) {
				this.uploader?.dispose();
				this.uploader = null;
				this.stopPolling();
				this.recorder = null;
				this.localId = null;
				this.deactivateEnvironment();
				this.set({ phase: "idle", localId: null, error: errorMessage(error), analyser: null });
			}
		}
	};

	stop = async (): Promise<void> => {
		if (this.snapshot.phase !== "recording") return;
		const run = this.runId;
		this.stopRequested = true;
		this.set({ phase: "draining", stoppedAt: Date.now() });
		if (this.localId) void store.patchSession(this.localId, { state: "stopping" }).catch(noop);
		const recorder = this.recorder;
		if (recorder && recorder.state !== "inactive") {
			try {
				recorder.stop();
			} catch (error) {
				console.error("Live: could not stop the recorder", error);
			}
		}
		await this.finishRecording(run);
	};

	/** Continues an unfinished meeting saved in this browser: upload the rest, then finalize. */
	resume = async (localId: string): Promise<void> => {
		if (isActivePhase(this.snapshot.phase)) return;
		const session = await store.getSession(localId).catch(() => undefined);
		if (!session) {
			this.notify({ title: "Recording not found", description: "It may have been discarded in another tab." });
			return;
		}
		const releaseLock = await acquireSessionLock(localId);
		if (!releaseLock) {
			this.notify({ title: "Meeting is open in another tab", description: "Finish or close it there first." });
			return;
		}

		const run = ++this.runId;
		this.teardown();
		this.resetSession({
			phase: "draining",
			localId,
			serverId: session.serverId,
			title: session.title,
			mimeType: session.mimeType,
			autoRetranscribe: !!session.autoRetranscribe,
			resumed: true,
			recordedChunks: session.lastSeq + 1,
			uploadedChunks: Math.max(0, session.ackedSeq + 1),
		});
		this.localId = localId;
		this.releaseLock = releaseLock;
		this.lastSeq = session.lastSeq;
		this.nextSeq = session.lastSeq + 1;
		this.activateEnvironment();

		let startSeq = session.ackedSeq + 1;
		if (session.serverId) {
			try {
				const state = await getSessionState(session.serverId, -1);
				if (run !== this.runId) return;
				this.applyState(run, state);
				if (state.live_status === "failed") {
					// An earlier server-side finalize failed; the server accepts finalize again
					// (the recording and live segments are still on disk).
					await finalizeSession(session.serverId);
					if (run !== this.runId) return;
					await store.patchSession(localId, { state: "finalizing", error: undefined }).catch(noop);
					this.set({ phase: "finalizing", error: null });
					this.startPolling(run);
					return;
				}
				if (state.live_status === "finalizing" || state.live_status === "finalized") {
					await store.patchSession(localId, { state: "finalizing" }).catch(noop);
					this.set({ phase: "finalizing" });
					if (state.live_status === "finalizing") this.startPolling(run);
					else void this.handleServerFinal(run, state);
					return;
				}
				if (typeof state.next_seq === "number") startSeq = state.next_seq;
			} catch (error) {
				if (run !== this.runId) return;
				if (error instanceof LiveApiError && error.status === 404) {
					await this.failSession(run, "The server no longer has this meeting. Download the recording from this browser instead.");
					return;
				}
				this.set({ serverReachable: !isServerDown(error), pollError: describeError(error) });
			}
		}

		await store.patchSession(localId, { state: "uploading", error: undefined }).catch(noop);
		this.uploader = this.createUploader(run, {
			localId,
			serverId: session.serverId,
			startSeq,
			availableSeq: session.lastSeq,
			title: session.title,
			mimeType: session.mimeType,
			autoRetranscribe: !!session.autoRetranscribe,
		});
		this.uploader.finish(session.lastSeq);
		this.uploader.start();
		this.startPolling(run);
		await this.completeUpload(run, session.lastSeq);
	};

	/** Retry a failed meeting (re-runs the resume path). */
	retry = async (): Promise<void> => {
		const localId = this.snapshot.localId;
		if (this.snapshot.phase !== "failed" || !localId) return;
		await this.resume(localId);
	};

	/** Stop waiting for the server; the meeting stays in this browser for "Resume upload". */
	keepLocally = (): void => {
		const phase = this.snapshot.phase;
		if (phase !== "draining" && phase !== "finalizing") return;
		this.runId++;
		this.teardown();
		this.resetSession({
			phase: "idle",
			info: "The meeting is saved in this browser. Use \"Resume upload\" below when the server is reachable.",
		});
		void this.refreshRecoverable();
	};

	/** Back to the start screen after a finished or failed meeting. */
	reset = (): void => {
		if (isActivePhase(this.snapshot.phase)) return;
		this.runId++;
		this.teardown();
		this.resetSession({ phase: "idle" });
		void this.refreshRecoverable();
	};

	dismissMessage = (): void => {
		if (this.snapshot.error || this.snapshot.info) this.set({ error: null, info: null });
	};

	/** Skip the current backoff wait for uploads and stop requests. */
	retryNow = (): void => {
		this.uploader?.retryNow();
		this.retryWaker?.();
		this.pollNow(this.runId);
	};

	/** Share computer audio again after the user ended sharing (or add it to a mic-only meeting). */
	reshareSystemAudio = async (): Promise<void> => {
		if (this.snapshot.phase !== "recording" || !this.mix) return;
		const run = this.runId;
		try {
			const stream = await captureSystemAudio();
			if (run !== this.runId || this.snapshot.phase !== "recording" || !this.mix) {
				stopStream(stream);
				return;
			}
			this.attachInput(run, "system", stream);
			this.set({ systemAudioLost: false, source: this.mix.isLive("mic") ? "mic+system" : "system" });
		} catch (error) {
			this.notify({ title: "Computer audio not shared", description: errorMessage(error) });
		}
	};

	reconnectMicrophone = async (deviceId = ""): Promise<void> => {
		if (this.snapshot.phase !== "recording" || !this.mix) return;
		const run = this.runId;
		try {
			const stream = await captureMicrophone(deviceId, this.mix.isLive("system"));
			if (run !== this.runId || this.snapshot.phase !== "recording" || !this.mix) {
				stopStream(stream);
				return;
			}
			this.attachInput(run, "mic", stream);
			this.set({ micLost: false, source: this.mix.isLive("system") ? "mic+system" : "mic" });
		} catch (error) {
			this.notify({ title: "Microphone not reconnected", description: errorMessage(error) });
		}
	};

	/** Counts unfinished meetings in this browser that no tab is working on. */
	refreshRecoverable = async (): Promise<void> => {
		try {
			const [sessions, held] = await Promise.all([store.listSessions(), heldSessionLocks()]);
			const activeId = isActivePhase(this.snapshot.phase) ? this.localId : null;
			const count = sessions.filter((s) => s.state !== "finalized" && s.id !== activeId && !held.has(s.id)).length;
			if (count !== this.snapshot.recoverableCount) this.set({ recoverableCount: count });
		} catch {
			/* IndexedDB unavailable: nothing to recover */
		}
	};

	// ----- recording -------------------------------------------------------------------

	private attachInput(run: number, kind: InputKind, stream: MediaStream): void {
		if (!this.mix) return;
		this.mix.setInput(kind, stream);
		for (const track of stream.getAudioTracks()) {
			// Refs, not React state: this fires long after the closure was created.
			track.addEventListener("ended", () => this.handleTrackEnded(run, kind, stream));
		}
	}

	private handleTrackEnded(run: number, kind: InputKind, stream: MediaStream): void {
		if (run !== this.runId || this.snapshot.phase !== "recording" || this.mix?.streamFor(kind) !== stream) return;
		if (stream.getAudioTracks().some((track) => track.readyState === "live")) return;
		if (kind === "system") {
			this.set({ systemAudioLost: true });
			this.notify({
				title: "Computer audio sharing ended",
				description: this.mix.isLive("mic")
					? "Recording continues with the microphone. Share again to capture the call."
					: "Share again to keep capturing the meeting.",
			});
		} else {
			this.set({ micLost: true });
			this.notify({ title: "Microphone disconnected", description: "Reconnect it to keep capturing your voice." });
		}
	}

	private handleContextState(run: number): void {
		const mix = this.mix;
		if (run !== this.runId || !mix || this.snapshot.phase !== "recording") return;
		const state = mix.context.state as string;
		if (state !== "running" && state !== "closed") {
			// A suspended mix records silence; try to get it going again.
			void mix.context.resume().catch((error) => console.warn("Live: could not resume audio", error));
		}
	}

	private handleChunk(run: number, blob: Blob): void {
		if (run !== this.runId || !this.localId || blob.size === 0) return;
		const seq = this.nextSeq++;
		const localId = this.localId;
		// Strictly ordered: persist locally first, then let the uploader send it.
		this.persistChain = this.persistChain.then(async () => {
			let saved = true;
			try {
				await store.appendChunk({ sessionId: localId, seq, blob, ts: Date.now() });
			} catch (error) {
				saved = false;
				console.error("Live: could not save chunk locally", seq, error);
			}
			if (!saved) this.memoryChunks.set(seq, blob);
			if (run !== this.runId) return;
			this.set({ recordedChunks: seq + 1, localSaveFailed: !saved });
			this.uploader?.push(seq);
		});
	}

	private handleRecorderStopped(run: number): void {
		if (run !== this.runId || this.stopRequested || this.snapshot.phase !== "recording") return;
		this.stopRequested = true;
		this.notify({ title: "Recording stopped unexpectedly", description: "Saving and uploading what was recorded." });
		this.set({ phase: "draining", stoppedAt: Date.now() });
		void this.finishRecording(run);
	}

	private flushRecorder = (): void => {
		const recorder = this.recorder;
		if (recorder && recorder.state === "recording") {
			try {
				recorder.requestData();
			} catch {
				/* recorder is stopping */
			}
		}
	};

	private async finishRecording(run: number): Promise<void> {
		if (this.recorderStopped) await withTimeout(this.recorderStopped, 5000);
		// The final dataavailable fires before "stop"; wait until it is persisted.
		await this.persistChain;
		if (run !== this.runId) return;
		this.releaseMedia();
		const lastSeq = this.nextSeq - 1;
		this.lastSeq = lastSeq;
		if (this.localId) await store.patchSession(this.localId, { state: "uploading" }).catch(noop);
		if (run !== this.runId) return;
		this.uploader?.finish(lastSeq);
		await this.completeUpload(run, lastSeq);
	}

	private releaseMedia(): void {
		if (this.mix) {
			this.mix.context.onstatechange = null;
			this.mix.close();
			this.mix = null;
		}
		this.recorder = null;
		if (this.snapshot.analyser) this.set({ analyser: null });
	}

	// ----- upload and finalize ---------------------------------------------------------

	private createUploader(
		run: number,
		options: {
			localId: string;
			serverId: string | null;
			startSeq: number;
			availableSeq: number;
			title: string;
			mimeType: string;
			autoRetranscribe: boolean;
		},
	): ChunkUploader {
		const { localId } = options;
		return new ChunkUploader({
			serverId: options.serverId,
			startSeq: options.startSeq,
			availableSeq: options.availableSeq,
			readChunk: async (seq) => this.memoryChunks.get(seq) ?? (await store.getChunk(localId, seq)),
			createServerSession: async () => {
				const created = await createSession({
					title: options.title,
					mime_type: options.mimeType,
					auto_retranscribe: options.autoRetranscribe,
				});
				return created.id;
			},
			onServerSession: (serverId) => {
				if (run !== this.runId) return;
				this.set({ serverId });
				void store.patchSession(localId, { serverId }).catch(noop);
				this.pollNow(run);
			},
			onAck: (ackedSeq) => {
				if (run !== this.runId) return;
				for (const seq of Array.from(this.memoryChunks.keys())) {
					if (seq <= ackedSeq) this.memoryChunks.delete(seq);
				}
				this.set({ uploadedChunks: ackedSeq + 1 });
				this.persistAck(localId, ackedSeq);
			},
			onStatus: (status) => {
				if (run !== this.runId) return;
				this.set({ upload: status, serverReachable: status.serverReachable });
			},
		});
	}

	/** Coalesced write of the acknowledged seq; only the latest value matters. */
	private persistAck(localId: string, seq: number): void {
		this.ackToPersist = { localId, seq };
		if (this.ackPersisting) return;
		this.ackPersisting = true;
		void (async () => {
			while (this.ackToPersist) {
				const next = this.ackToPersist;
				this.ackToPersist = null;
				await store.patchSession(next.localId, { ackedSeq: next.seq }).catch(noop);
			}
			this.ackPersisting = false;
		})();
	}

	/** Drain the uploader, then POST stop (resending on missing_chunks), then wait for finalize. */
	private async completeUpload(run: number, lastSeq: number): Promise<void> {
		let delay = RETRY_START_MS;
		for (;;) {
			const uploader = this.uploader;
			if (!uploader || run !== this.runId) return;
			const result = await uploader.waitForDrain();
			if (run !== this.runId || result === "cancelled") return;
			if (result === "fatal") {
				await this.failSession(run, `${uploader.status.fatal ?? "The upload failed."} The recording is still saved in this browser.`);
				return;
			}
			if (result === "closed") break; // the server is already finalizing or finalized

			const serverId = this.snapshot.serverId;
			if (!serverId) return;
			try {
				await stopSession(serverId, lastSeq);
				break;
			} catch (error) {
				if (run !== this.runId) return;
				if (error instanceof LiveApiError && error.status === 409) {
					if (error.code === "missing_chunks" && error.nextSeq !== undefined) {
						uploader.rewind(error.nextSeq);
						delay = RETRY_START_MS;
						continue;
					}
					break; // closed: a previous stop already went through
				}
				if (error instanceof LiveApiError && error.status === 404) {
					await this.failSession(run, "The server no longer has this meeting. The recording is still saved in this browser.");
					return;
				}
				this.set({ serverReachable: !isServerDown(error), pollError: describeError(error) });
				await this.sleepForRetry(delay);
				delay = Math.min(delay * 2, RETRY_MAX_MS);
			}
		}

		if (run !== this.runId) return;
		if (this.localId) await store.patchSession(this.localId, { state: "finalizing" }).catch(noop);
		this.set({ phase: "finalizing", serverReachable: true });
		this.pollNow(run);
	}

	private sleepForRetry(ms: number): Promise<void> {
		return new Promise((resolve) => {
			const finish = () => {
				window.clearTimeout(timer);
				if (this.retryWaker === finish) this.retryWaker = null;
				resolve();
			};
			const timer = window.setTimeout(finish, ms);
			this.retryWaker = finish;
		});
	}

	private async handleServerFinal(run: number, state: LiveStateResponse): Promise<void> {
		if (this.finalHandled || run !== this.runId) return;
		this.finalHandled = true;
		const localId = this.localId;

		if (state.live_status === "failed") {
			await this.failSession(
				run,
				"The server could not finalize this meeting. Its recording is kept on the server and in this browser.",
			);
			return;
		}

		// Delete the local copy only if the server holds every chunk this browser recorded.
		const serverNext = typeof state.next_seq === "number" ? state.next_seq : this.snapshot.uploadedChunks;
		const complete = serverNext > this.lastSeq;
		if (localId) {
			if (complete) {
				await store.patchSession(localId, { state: "finalized", ackedSeq: this.lastSeq, error: undefined }).catch(noop);
				await store.deleteChunks(localId).catch((error) => console.error("Live: could not delete local chunks", error));
			} else {
				await store
					.patchSession(localId, {
						state: "failed",
						error: "The server finalized this meeting before every chunk arrived. The full recording is kept in this browser.",
					})
					.catch(noop);
			}
		}
		if (run !== this.runId) return;
		this.teardown();
		this.set({ phase: "finalized", partial: null, incompleteOnServer: !complete, serverReachable: true });
		void this.refreshRecoverable();
	}

	private async failSession(run: number, message: string): Promise<void> {
		if (run !== this.runId) return;
		if (this.localId) await store.patchSession(this.localId, { state: "failed", error: message }).catch(noop);
		if (run !== this.runId) return;
		this.teardown();
		this.set({ phase: "failed", error: message, partial: null });
		void this.refreshRecoverable();
	}

	// ----- live text polling -----------------------------------------------------------

	private startPolling(run: number): void {
		if (this.pollTimer !== null || this.pollInFlight) return;
		this.pollTimer = window.setTimeout(() => void this.pollTick(run), 0);
	}

	private stopPolling(): void {
		if (this.pollTimer !== null) window.clearTimeout(this.pollTimer);
		this.pollTimer = null;
	}

	private pollNow(run: number): void {
		if (this.pollInFlight) return;
		this.stopPolling();
		void this.pollTick(run);
	}

	private async pollTick(run: number): Promise<void> {
		this.pollTimer = null;
		if (run !== this.runId || !isPollingPhase(this.snapshot.phase)) return;
		const serverId = this.snapshot.serverId;
		if (serverId) {
			this.pollInFlight = true;
			try {
				const state = await getSessionState(serverId, this.lastSegSeq);
				this.applyState(run, state);
			} catch (error) {
				if (run === this.runId) {
					this.set({ pollError: describeError(error), serverReachable: !isServerDown(error) });
				}
			} finally {
				this.pollInFlight = false;
			}
		}
		if (run !== this.runId || !isPollingPhase(this.snapshot.phase) || this.pollTimer !== null) return;
		this.pollTimer = window.setTimeout(() => void this.pollTick(run), POLL_MS);
	}

	private applyState(run: number, state: LiveStateResponse): void {
		if (run !== this.runId) return;
		let segments = this.snapshot.segments;
		const fresh = (state.segments ?? []).filter(
			(segment) => typeof segment.seq === "number" && !this.segmentSeqs.has(segment.seq),
		);
		if (fresh.length > 0) {
			for (const segment of fresh) {
				this.segmentSeqs.add(segment.seq);
				this.lastSegSeq = Math.max(this.lastSegSeq, segment.seq);
			}
			segments = [...segments, ...fresh].sort((a, b) => a.seq - b.seq);
		}
		const partial = state.partial && state.partial.text?.trim() ? state.partial : null;
		this.set({
			segments,
			partial: this.snapshot.phase === "recording" || this.snapshot.phase === "draining" ? partial : null,
			asr: state.asr ?? null,
			liveStatus: state.live_status ?? null,
			receivedAudioSec: typeof state.received_audio_sec === "number" ? state.received_audio_sec : null,
			serverReachable: true,
			pollError: null,
		});
		if (
			this.snapshot.phase === "finalizing" &&
			(state.live_status === "finalized" || state.live_status === "failed")
		) {
			void this.handleServerFinal(run, state);
		}
	}

	// ----- environment: wake lock, unload guard, flush on hide, document title ---------

	private activateEnvironment(): void {
		if (!this.windowListeners) {
			window.addEventListener("beforeunload", this.handleBeforeUnload);
			window.addEventListener("pagehide", this.flushRecorder);
			document.addEventListener("visibilitychange", this.handleVisibility);
			window.addEventListener("online", this.handleOnline);
			this.windowListeners = true;
		}
		if (this.originalTitle === null) {
			this.originalTitle = document.title;
			document.title = `● Live meeting - ${this.originalTitle}`;
		}
		void this.acquireWakeLock();
	}

	private deactivateEnvironment(): void {
		if (this.windowListeners) {
			window.removeEventListener("beforeunload", this.handleBeforeUnload);
			window.removeEventListener("pagehide", this.flushRecorder);
			document.removeEventListener("visibilitychange", this.handleVisibility);
			window.removeEventListener("online", this.handleOnline);
			this.windowListeners = false;
		}
		if (this.originalTitle !== null) {
			document.title = this.originalTitle;
			this.originalTitle = null;
		}
		const lock = this.wakeLock;
		this.wakeLock = null;
		if (lock) void lock.release().catch(noop);
	}

	/** Stops everything the current session holds, except the snapshot. */
	private teardown(): void {
		this.uploader?.dispose();
		this.uploader = null;
		this.stopPolling();
		this.retryWaker?.();
		this.releaseMedia();
		this.releaseLock?.();
		this.releaseLock = null;
		this.deactivateEnvironment();
	}

	private handleBeforeUnload = (event: BeforeUnloadEvent): void => {
		const phase = this.snapshot.phase;
		if (phase === "starting" || phase === "recording" || phase === "draining") {
			event.preventDefault();
			event.returnValue = "A live meeting is still recording or uploading.";
		}
	};

	private handleVisibility = (): void => {
		if (document.visibilityState === "hidden") {
			// Persist the audio buffered so far in case the tab is discarded while hidden.
			this.flushRecorder();
			return;
		}
		if (!isActivePhase(this.snapshot.phase)) return;
		void this.acquireWakeLock();
		this.uploader?.retryNow();
		this.pollNow(this.runId);
	};

	private handleOnline = (): void => {
		this.retryNow();
	};

	private async acquireWakeLock(): Promise<void> {
		if (this.wakeLock || this.wakeLockPending || !("wakeLock" in navigator)) return;
		if (document.visibilityState !== "visible") return;
		this.wakeLockPending = true;
		try {
			const lock = await navigator.wakeLock.request("screen");
			if (!this.windowListeners) {
				void lock.release().catch(noop);
				return;
			}
			this.wakeLock = lock;
			lock.addEventListener("release", () => {
				if (this.wakeLock === lock) this.wakeLock = null;
			});
		} catch (error) {
			console.warn("Live: screen wake lock unavailable", error);
		} finally {
			this.wakeLockPending = false;
		}
	}
}

let controller: LiveSessionController | null = null;

/** The app-wide controller. It outlives every component, so recordings survive navigation. */
export function getLiveController(): LiveSessionController {
	if (!controller) controller = new LiveSessionController();
	return controller;
}
