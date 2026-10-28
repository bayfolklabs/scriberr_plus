// Audio capture for live meetings: microphone, computer audio (getDisplayMedia),
// and a Web Audio mix of the two feeding a single MediaRecorder. Same approach as
// SystemAudioRecorder, but sources can be re-attached mid-meeting without
// restarting the recorder.

export type LiveSource = "mic" | "mic+system" | "system";
export type InputKind = "mic" | "system";

export const PREFERRED_MIME_TYPE = "audio/webm;codecs=opus";
export const AUDIO_BITS_PER_SECOND = 128_000;
export const TIMESLICE_MS = 1000;

export const usesMic = (source: LiveSource) => source !== "system";
export const usesSystemAudio = (source: LiveSource) => source !== "mic";

/** A failure with a message written for the user. */
export class MediaAccessError extends Error {
	constructor(message: string) {
		super(message);
		this.name = "MediaAccessError";
	}
}

export function isChromium(): boolean {
	const userAgent = navigator.userAgent.toLowerCase();
	return userAgent.includes("chrome") || userAgent.includes("chromium") || userAgent.includes("edg/");
}

export function pickMimeType(): string {
	if (typeof MediaRecorder === "undefined" || typeof MediaRecorder.isTypeSupported !== "function") return "";
	return MediaRecorder.isTypeSupported(PREFERRED_MIME_TYPE) ? PREFERRED_MIME_TYPE : "";
}

export interface SupportCheck {
	error?: string;
	warning?: string;
}

export function checkSupport(source: LiveSource): SupportCheck {
	if (!window.isSecureContext) {
		return { error: "Recording needs a secure page. Open Scriberr over https or on localhost." };
	}
	if (typeof MediaRecorder === "undefined") {
		return { error: "This browser cannot record audio. Use a current Chrome or Edge." };
	}
	if (usesMic(source) && !navigator.mediaDevices?.getUserMedia) {
		return { error: "This browser does not allow microphone access." };
	}
	if (usesSystemAudio(source)) {
		if (!navigator.mediaDevices?.getDisplayMedia) {
			return { error: "This browser cannot capture computer audio. Use Chrome or Edge." };
		}
		if (!isChromium()) {
			return { warning: "Computer audio capture works reliably only in Chrome or Edge. Other browsers may share no audio." };
		}
	}
	return {};
}

export function stopStream(stream: MediaStream | null | undefined): void {
	stream?.getTracks().forEach((track) => track.stop());
}

/**
 * Asks the user to share a screen or tab and keeps only its audio. The video track is
 * required by the API and stopped immediately.
 */
export async function captureSystemAudio(): Promise<MediaStream> {
	if (!navigator.mediaDevices?.getDisplayMedia) {
		throw new MediaAccessError("This browser cannot capture computer audio. Use Chrome or Edge.");
	}
	let display: MediaStream;
	try {
		display = await navigator.mediaDevices.getDisplayMedia({
			video: true,
			audio: {
				echoCancellation: false,
				noiseSuppression: false,
				autoGainControl: false,
			},
			// Chromium hints: offer "Share system audio" for screens and keep this tab out of the list.
			systemAudio: "include",
			selfBrowserSurface: "exclude",
		} as DisplayMediaStreamOptions);
	} catch (error) {
		if (error instanceof DOMException && error.name === "NotAllowedError") {
			throw new MediaAccessError("Screen sharing was cancelled or blocked, so no computer audio was captured.");
		}
		throw new MediaAccessError("The browser could not start screen sharing for computer audio.");
	}

	for (const track of display.getVideoTracks()) {
		track.stop();
		display.removeTrack(track);
	}
	const audioTracks = display.getAudioTracks();
	if (audioTracks.length === 0) {
		stopStream(display);
		throw new MediaAccessError(
			"No computer audio was shared. Pick \"Entire screen\" and turn on \"Share system audio\" for desktop Zoom or Teams, " +
			"or pick the meeting's tab and turn on \"Share tab audio\".",
		);
	}
	return new MediaStream(audioTracks);
}

// Chromium accepts "remote-only" for echoCancellation; the DOM typings only know booleans.
type ChromiumTrackConstraints = Omit<MediaTrackConstraints, "echoCancellation"> & {
	echoCancellation?: ConstrainBoolean | "remote-only" | "all";
};

/**
 * Opens the microphone. With computer audio mixed in, echo cancellation and noise suppression
 * are on (as in SystemAudioRecorder); for a microphone-only meeting the signal stays raw so a
 * call played on speakers is still captured.
 */
export async function captureMicrophone(deviceId: string, mixedWithSystemAudio: boolean): Promise<MediaStream> {
	if (!navigator.mediaDevices?.getUserMedia) {
		throw new MediaAccessError("This browser does not allow microphone access.");
	}
	const base: ChromiumTrackConstraints = mixedWithSystemAudio
		? { echoCancellation: "remote-only", noiseSuppression: true, autoGainControl: true }
		: { echoCancellation: false, noiseSuppression: false, autoGainControl: true, channelCount: 1 };
	const open = (withDevice: boolean) =>
		navigator.mediaDevices.getUserMedia({
			audio: (withDevice && deviceId ? { ...base, deviceId: { exact: deviceId } } : base) as MediaTrackConstraints,
		});

	try {
		return await open(true);
	} catch (error) {
		const name = error instanceof DOMException ? error.name : "";
		// The chosen device may have been unplugged since it was picked: fall back to the default.
		if (deviceId && (name === "OverconstrainedError" || name === "NotFoundError" || name === "NotReadableError")) {
			try {
				return await open(false);
			} catch {
				/* report the original problem below */
			}
		}
		if (name === "NotAllowedError") {
			throw new MediaAccessError("Microphone access is blocked. Allow it in the address bar and try again.");
		}
		if (name === "NotFoundError") {
			throw new MediaAccessError("No microphone was found.");
		}
		if (name === "NotReadableError") {
			throw new MediaAccessError("The microphone is in use by another app or could not be opened.");
		}
		throw new MediaAccessError("The microphone could not be opened.");
	}
}

interface MixInput {
	stream: MediaStream;
	source: MediaStreamAudioSourceNode;
	gain: GainNode;
}

/** Mixes the inputs into one MediaStreamDestination that the MediaRecorder records. */
export class AudioMix {
	readonly context: AudioContext;
	readonly destination: MediaStreamAudioDestinationNode;
	readonly analyser: AnalyserNode;
	private readonly inputs = new Map<InputKind, MixInput>();

	constructor() {
		this.context = new AudioContext();
		this.destination = this.context.createMediaStreamDestination();
		this.analyser = this.context.createAnalyser();
		this.analyser.fftSize = 1024;
	}

	get stream(): MediaStream {
		return this.destination.stream;
	}

	/** Connects a stream as the given input, replacing (and stopping) any previous one. */
	setInput(kind: InputKind, stream: MediaStream): void {
		this.removeInput(kind);
		const source = this.context.createMediaStreamSource(stream);
		const gain = this.context.createGain();
		source.connect(gain);
		gain.connect(this.destination);
		gain.connect(this.analyser);
		this.inputs.set(kind, { stream, source, gain });
	}

	removeInput(kind: InputKind): void {
		const input = this.inputs.get(kind);
		if (!input) return;
		input.source.disconnect();
		input.gain.disconnect();
		stopStream(input.stream);
		this.inputs.delete(kind);
	}

	streamFor(kind: InputKind): MediaStream | null {
		return this.inputs.get(kind)?.stream ?? null;
	}

	/** True when the input exists and at least one of its tracks is still live. */
	isLive(kind: InputKind): boolean {
		const stream = this.streamFor(kind);
		return !!stream && stream.getAudioTracks().some((track) => track.readyState === "live");
	}

	async resume(): Promise<void> {
		if (this.context.state === "suspended") {
			await this.context.resume().catch(() => undefined);
		}
	}

	close(): void {
		for (const kind of Array.from(this.inputs.keys())) this.removeInput(kind);
		this.analyser.disconnect();
		if (this.context.state !== "closed") void this.context.close().catch(() => undefined);
	}
}
