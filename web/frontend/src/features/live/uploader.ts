import { LiveApiError, describeError, isRetryable, isServerDown, uploadChunk } from "./api";
import { errorMessage } from "./format";

// Strictly sequential chunk uploader. One request in flight at a time, in seq order,
// retrying forever with exponential backoff until it is disposed. The server is the
// authority on where to continue: a 409 "gap" rewinds to its next_seq, a duplicate
// counts as acknowledged, and a 409 "closed" ends the upload.

export const RETRY_START_MS = 250;
export const RETRY_MAX_MS = 10_000;
/** Extra reads of a chunk that came back missing before the upload gives up on it. */
const MISSING_REREADS = 2;

export interface UploaderStatus {
	/** Highest seq the server confirmed; everything up to it is safe on the server. */
	ackedSeq: number;
	/** Highest seq saved locally and ready to send. */
	availableSeq: number;
	/** Chunks recorded but not yet confirmed by the server. */
	pending: number;
	retrying: boolean;
	serverReachable: boolean;
	lastError: string | null;
	/** Set when uploading cannot continue. The recording stays in this browser. */
	fatal: string | null;
	/** The server reported the session closed (finalizing, finalized or failed). */
	closed: boolean;
}

export type DrainResult = "drained" | "closed" | "fatal" | "cancelled";

export interface ChunkUploaderOptions {
	serverId: string | null;
	/** First seq to send. */
	startSeq: number;
	/** Highest seq already available locally, -1 when none. */
	availableSeq: number;
	readChunk: (seq: number) => Promise<Blob | null>;
	/** Creates the server session when serverId is null (the server was down at start). */
	createServerSession: () => Promise<string>;
	onServerSession: (serverId: string) => void;
	onAck: (ackedSeq: number) => void;
	onStatus: (status: UploaderStatus) => void;
}

export class ChunkUploader {
	private readonly options: ChunkUploaderOptions;
	private serverId: string | null;
	private nextSeq: number;
	private availableSeq: number;
	/** Last seq of the recording, set once the recorder has stopped. */
	private finalSeq: number | null = null;
	private delay = RETRY_START_MS;
	private missingReads = 0;
	private running = false;
	private disposed = false;
	private outcome: DrainResult | null = null;
	private wakeWork: (() => void) | null = null;
	private wakeSleep: (() => void) | null = null;
	private drainWaiters: Array<(result: DrainResult) => void> = [];
	private current: UploaderStatus;

	constructor(options: ChunkUploaderOptions) {
		this.options = options;
		this.serverId = options.serverId;
		this.nextSeq = options.startSeq;
		this.availableSeq = options.availableSeq;
		this.current = {
			ackedSeq: options.startSeq - 1,
			availableSeq: options.availableSeq,
			pending: Math.max(0, options.availableSeq - (options.startSeq - 1)),
			retrying: false,
			serverReachable: true,
			lastError: null,
			fatal: null,
			closed: false,
		};
	}

	get status(): UploaderStatus {
		return this.current;
	}

	start(): void {
		if (this.running || this.disposed) return;
		this.running = true;
		void this.run();
	}

	/** A new chunk is persisted locally and can be sent. */
	push(seq: number): void {
		if (seq > this.availableSeq) {
			this.availableSeq = seq;
			this.update({ availableSeq: seq });
		}
		this.wakeWork?.();
	}

	/** The recorder has stopped; lastSeq is the final chunk (-1 when nothing was recorded). */
	finish(lastSeq: number): void {
		this.finalSeq = lastSeq;
		if (lastSeq > this.availableSeq) {
			this.availableSeq = lastSeq;
			this.update({ availableSeq: lastSeq });
		}
		this.wakeWork?.();
		this.settleDrainedIfDone();
	}

	/** Resend from seq, e.g. after the server answered stop with missing_chunks. */
	rewind(seq: number): void {
		if (seq < this.nextSeq) {
			this.nextSeq = Math.max(0, seq);
			this.update({ ackedSeq: this.nextSeq - 1 });
			this.options.onAck(this.nextSeq - 1);
		}
		this.wakeWork?.();
		this.wakeSleep?.();
	}

	/** Skip the current backoff wait, e.g. when the browser comes back online. */
	retryNow(): void {
		this.wakeSleep?.();
	}

	dispose(): void {
		if (this.disposed) return;
		this.disposed = true;
		this.settle("cancelled");
		this.wakeWork?.();
		this.wakeSleep?.();
	}

	/** Resolves once every chunk up to the final seq is acknowledged, or the upload ends otherwise. */
	waitForDrain(): Promise<DrainResult> {
		if (this.outcome) return Promise.resolve(this.outcome);
		if (this.isDrained()) return Promise.resolve("drained");
		return new Promise((resolve) => this.drainWaiters.push(resolve));
	}

	private isDrained(): boolean {
		return this.finalSeq !== null && this.serverId !== null && this.nextSeq > this.finalSeq;
	}

	private async run(): Promise<void> {
		while (!this.disposed && !this.outcome) {
			if (!this.serverId) {
				try {
					const serverId = await this.options.createServerSession();
					if (this.disposed) return;
					this.serverId = serverId;
					this.options.onServerSession(serverId);
					this.succeeded(false);
				} catch (error) {
					if (this.disposed) return;
					if (!isRetryable(error)) {
						this.fail(`The server refused to start the meeting: ${describeError(error)}`);
						return;
					}
					await this.backoff(error);
				}
				continue;
			}

			if (this.nextSeq > this.availableSeq) {
				this.settleDrainedIfDone();
				await this.waitForWork();
				continue;
			}

			const seq = this.nextSeq;
			let blob: Blob | null;
			try {
				blob = await this.options.readChunk(seq);
			} catch (error) {
				// A read can fail transiently (e.g. the IndexedDB connection was closed); the chunk
				// is still stored, so read it again later instead of ending the upload.
				console.error("Live: could not read chunk", seq, error);
				if (this.disposed) return;
				this.update({ lastError: `Could not read chunk ${seq} from this browser's storage: ${errorMessage(error)}` });
				await this.pause();
				continue;
			}
			if (this.disposed) return;
			if (!blob) {
				if (this.missingReads < MISSING_REREADS) {
					this.missingReads++;
					await this.pause();
					continue;
				}
				this.fail(`Chunk ${seq} is missing from this browser's storage, so the upload cannot continue.`);
				return;
			}
			this.missingReads = 0;

			try {
				const ack = await uploadChunk(this.serverId, seq, blob);
				if (this.disposed) return;
				if (this.nextSeq !== seq) continue; // rewound while the request was in flight
				const next = typeof ack.next_seq === "number" ? ack.next_seq : seq + 1;
				this.nextSeq = Math.max(next, seq + 1);
				this.succeeded(true);
			} catch (error) {
				if (this.disposed) return;
				if (error instanceof LiveApiError && error.status === 409) {
					if (error.code === "gap" && error.nextSeq !== undefined) {
						this.nextSeq = Math.max(0, error.nextSeq);
						this.succeeded(true);
						continue;
					}
					if (error.code === "closed") {
						this.update({ closed: true, retrying: false });
						this.settle("closed");
						return;
					}
				}
				if (error instanceof LiveApiError && error.status === 404) {
					this.fail("The server no longer has this meeting.");
					return;
				}
				if (!isRetryable(error)) {
					// e.g. 400/413: resending the same bytes would fail forever
					this.fail(`The server rejected chunk ${seq}: ${describeError(error)}`);
					return;
				}
				await this.backoff(error);
			}
		}
	}

	private succeeded(acked: boolean): void {
		this.delay = RETRY_START_MS;
		this.update({ ackedSeq: this.nextSeq - 1, retrying: false, serverReachable: true, lastError: null });
		if (acked) this.options.onAck(this.nextSeq - 1);
	}

	private async backoff(error: unknown): Promise<void> {
		this.update({ retrying: true, serverReachable: !isServerDown(error), lastError: describeError(error) });
		await this.pause();
	}

	/** Waits out the current retry delay and doubles it for the next failure. */
	private async pause(): Promise<void> {
		const wait = this.delay;
		this.delay = Math.min(this.delay * 2, RETRY_MAX_MS);
		await this.sleep(wait);
	}

	private fail(message: string): void {
		this.update({ fatal: message, retrying: false });
		this.settle("fatal");
	}

	private settle(result: DrainResult): void {
		if (this.outcome) return;
		this.outcome = result;
		const waiters = this.drainWaiters;
		this.drainWaiters = [];
		waiters.forEach((resolve) => resolve(result));
	}

	private settleDrainedIfDone(): void {
		if (this.outcome || !this.isDrained()) return;
		const waiters = this.drainWaiters;
		this.drainWaiters = [];
		waiters.forEach((resolve) => resolve("drained"));
	}

	private waitForWork(): Promise<void> {
		return new Promise((resolve) => {
			this.wakeWork = () => {
				this.wakeWork = null;
				resolve();
			};
		});
	}

	private sleep(ms: number): Promise<void> {
		return new Promise((resolve) => {
			const finish = () => {
				window.clearTimeout(timer);
				if (this.wakeSleep === finish) this.wakeSleep = null;
				resolve();
			};
			const timer = window.setTimeout(finish, ms);
			this.wakeSleep = finish;
		});
	}

	private update(patch: Partial<UploaderStatus>): void {
		const next = { ...this.current, ...patch };
		next.pending = Math.max(0, next.availableSeq - next.ackedSeq);
		this.current = next;
		this.options.onStatus(next);
	}
}
