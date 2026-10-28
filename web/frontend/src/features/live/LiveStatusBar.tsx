import type { ReactNode } from "react";
import { AudioLines, Clock, CloudUpload, HardDrive, Loader2, WifiOff } from "lucide-react";
import { cn } from "@/lib/utils";
import type { LiveSnapshot } from "./liveController";
import type { LiveAsrInfo } from "./api";
import { formatElapsed } from "./format";
import { useNow } from "./hooks";

type Tone = "neutral" | "good" | "warn" | "bad";

const toneClasses: Record<Tone, string> = {
	neutral: "text-[var(--text-secondary)] border-[var(--border-subtle)] bg-[var(--bg-main)]",
	good: "text-[var(--success-solid)] border-[var(--success-solid)]/20 bg-[var(--success-translucent)]",
	warn: "text-[var(--warning-solid)] border-[var(--warning-solid)]/20 bg-[var(--warning-translucent)]",
	bad: "text-[var(--error)] border-[var(--error)]/20 bg-[var(--error)]/10",
};

function Pill({ tone = "neutral", icon, children, title }: { tone?: Tone; icon: ReactNode; children: ReactNode; title?: string }) {
	return (
		<span
			title={title}
			className={cn(
				"inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium tabular-nums [&_svg]:h-3.5 [&_svg]:w-3.5",
				toneClasses[tone],
			)}
		>
			{icon}
			{children}
		</span>
	);
}

interface AsrSummary {
	label: string;
	tone: Tone;
	/** Explanation shown under the pills when live text is not flowing normally. */
	message?: string;
}

const LAG_WARNING_SEC = 10;

function describeAsr(asr: LiveAsrInfo | null, serverConnected: boolean): AsrSummary {
	if (!serverConnected) return { label: "Live text waiting for server", tone: "neutral" };
	if (!asr) return { label: "Live text starting", tone: "neutral" };
	const lag = typeof asr.lag_sec === "number" ? asr.lag_sec : null;
	switch (asr.state) {
		case "running":
			if (lag !== null && lag > LAG_WARNING_SEC) {
				return {
					label: `Live text ${Math.round(lag)} s behind`,
					tone: "warn",
					message: "Live transcription is falling behind. Recording is safe; live text will catch up.",
				};
			}
			return { label: lag !== null ? `Live, lag ${lag.toFixed(1)} s` : "Live", tone: "good" };
		case "starting":
			return {
				label: "Live text starting",
				tone: "neutral",
				message: "Loading the live transcription model. The first start can take a while; recording is already running.",
			};
		case "restarting":
			return {
				label: "Live text restarting",
				tone: "warn",
				message: "Live transcription is restarting. Recording is safe; live text will catch up.",
			};
		case "unavailable":
			return {
				label: "Live text unavailable",
				tone: "warn",
				message: `Live transcription is unavailable${asr.error ? ` (${asr.error})` : ""}. Recording is safe; live text will catch up.`,
			};
		case "failed":
			return {
				label: "Live text stopped",
				tone: "bad",
				message: `Live transcription stopped${asr.error ? `: ${asr.error}` : ""}. Recording is safe; you can transcribe the full recording after the meeting.`,
			};
		case "stopped":
			return { label: "Live text finished", tone: "neutral" };
		default:
			return { label: `Live text: ${String(asr.state)}`, tone: "neutral" };
	}
}

export function LiveStatusBar({ snapshot }: { snapshot: LiveSnapshot }) {
	const ticking = snapshot.phase === "recording";
	const now = useNow(ticking ? 1000 : null);

	const elapsedSec = snapshot.startedAt
		? ((snapshot.stoppedAt ?? now) - snapshot.startedAt) / 1000
		: snapshot.recordedChunks; // resumed meeting: one chunk is about one second
	const upload = snapshot.upload;
	const pending = Math.max(0, snapshot.recordedChunks - snapshot.uploadedChunks);
	const serverDown = !snapshot.serverReachable || (upload?.retrying ?? false);
	const asr = describeAsr(snapshot.asr, !!snapshot.serverId && snapshot.serverReachable);
	const showAsr = snapshot.phase === "recording" || snapshot.phase === "draining";

	return (
		<div className="space-y-2">
			<div className="flex flex-wrap items-center gap-2">
				<Pill icon={<Clock />} title={snapshot.resumed ? "Approximate length recorded" : "Elapsed"}>
					{snapshot.resumed ? "≈ " : ""}
					{formatElapsed(elapsedSec)}
				</Pill>

				{/* Once finalized the local copy is deleted, so the indicator no longer applies. */}
				{snapshot.phase !== "finalized" &&
					(snapshot.localSaveFailed ? (
						<Pill tone="warn" icon={<HardDrive />} title="This browser could not store the latest audio">
							Not saved locally
						</Pill>
					) : (
						<Pill tone="good" icon={<HardDrive />} title="Every chunk is written to this browser before upload">
							Saved locally
						</Pill>
					))}

				<Pill
					tone={upload?.fatal ? "bad" : serverDown ? "warn" : "neutral"}
					icon={serverDown ? <WifiOff /> : <CloudUpload />}
					title={upload?.lastError ?? snapshot.pollError ?? "Chunks confirmed by the server / chunks recorded"}
				>
					Uploaded {snapshot.uploadedChunks}/{snapshot.recordedChunks}
					{pending > 0 && <span className="opacity-80">({pending} pending)</span>}
				</Pill>

				{showAsr && (
					<Pill tone={asr.tone} icon={asr.tone === "neutral" && snapshot.asr?.state === "starting" ? <Loader2 className="animate-spin" /> : <AudioLines />}>
						{asr.label}
					</Pill>
				)}
			</div>

			{showAsr && asr.message && <p className="text-xs text-[var(--text-secondary)]">{asr.message}</p>}
		</div>
	);
}
