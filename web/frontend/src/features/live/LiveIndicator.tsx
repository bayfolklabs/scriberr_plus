import { useNavigate } from "react-router-dom";
import { AlertCircle, Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { useLiveSession } from "./liveSessionContext";
import { formatElapsed } from "./format";
import { useNow } from "./hooks";

/**
 * Header pill shown anywhere in the app while a live meeting records, uploads or finalizes.
 * When idle it points at meetings a crashed tab left unfinished.
 */
export function LiveIndicator() {
	const { snapshot } = useLiveSession();
	const navigate = useNavigate();
	const { phase } = snapshot;
	const recording = phase === "recording";
	const now = useNow(recording ? 1000 : null);

	const base =
		"inline-flex items-center gap-2 h-8 sm:h-10 px-2.5 sm:px-3 rounded-[var(--radius-btn)] text-xs sm:text-sm font-semibold transition-all cursor-pointer hover:scale-105 active:scale-95";
	const open = () => navigate("/live");

	if (recording) {
		const elapsed = snapshot.startedAt ? (now - snapshot.startedAt) / 1000 : 0;
		return (
			<button
				type="button"
				onClick={open}
				className={cn(base, "bg-[var(--error)]/10 text-[var(--error)] border border-[var(--error)]/20")}
				title="Live meeting is recording"
			>
				<span className="relative flex h-2.5 w-2.5">
					<span className="absolute inline-flex h-full w-full rounded-full bg-[var(--error)] opacity-75 animate-ping" />
					<span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-[var(--error)]" />
				</span>
				<span className="hidden sm:inline">Live</span>
				<span className="font-mono tabular-nums">{formatElapsed(elapsed)}</span>
			</button>
		);
	}

	if (phase === "starting" || phase === "draining" || phase === "finalizing") {
		const pending = Math.max(0, snapshot.recordedChunks - snapshot.uploadedChunks);
		const label = phase === "starting" ? "Starting" : phase === "draining" ? `Uploading${pending ? ` ${pending}` : ""}` : "Finalizing";
		return (
			<button
				type="button"
				onClick={open}
				className={cn(base, "bg-[var(--brand-light)] text-[var(--brand-solid)] border border-[var(--brand-solid)]/20")}
				title="Live meeting is being saved"
			>
				<Loader2 className="h-4 w-4 animate-spin" />
				<span className="hidden sm:inline">{label}</span>
			</button>
		);
	}

	if (snapshot.recoverableCount > 0) {
		return (
			<button
				type="button"
				onClick={open}
				className={cn(base, "bg-[var(--warning-translucent)] text-[var(--warning-solid)] border border-[var(--warning-solid)]/20")}
				title="A live meeting was not finished. Open Live meeting to recover it."
			>
				<AlertCircle className="h-4 w-4" />
				<span className="hidden sm:inline">Unfinished meeting</span>
			</button>
		);
	}

	return null;
}
