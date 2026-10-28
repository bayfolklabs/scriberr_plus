import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Download, FileText, Loader2, Mic, MonitorSpeaker, Plus, RefreshCw, RotateCcw, Square } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import { useLiveSession } from "./liveSessionContext";
import type { LiveSnapshot } from "./liveController";
import { downloadLocalRecording } from "./chunkStore";
import { errorMessage } from "./format";
import { LiveBanner } from "./LiveBanner";
import { LiveStatusBar } from "./LiveStatusBar";
import { LiveTranscriptPanel } from "./LiveTranscriptPanel";
import { LevelMeter } from "./LevelMeter";

const SOURCE_LABELS: Record<string, string> = {
	mic: "Microphone",
	"mic+system": "Microphone + computer audio",
	system: "Computer audio",
};

function PhaseBadge({ snapshot }: { snapshot: LiveSnapshot }) {
	const { phase } = snapshot;
	const base = "inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold uppercase tracking-wide";
	if (phase === "recording") {
		return (
			<span className={cn(base, "bg-[var(--error)]/10 text-[var(--error)]")}>
				<span className="h-2 w-2 rounded-full bg-[var(--error)] animate-pulse" />
				Recording
			</span>
		);
	}
	if (phase === "draining" || phase === "finalizing") {
		return (
			<span className={cn(base, "bg-[var(--brand-light)] text-[var(--brand-solid)]")}>
				<Loader2 className="h-3 w-3 animate-spin" />
				{phase === "draining" ? "Uploading" : "Finalizing"}
			</span>
		);
	}
	if (phase === "finalized") {
		return <span className={cn(base, "bg-[var(--success-translucent)] text-[var(--success-solid)]")}>Saved</span>;
	}
	if (phase === "failed") {
		return <span className={cn(base, "bg-[var(--error)]/10 text-[var(--error)]")}>Needs attention</span>;
	}
	return null;
}

export function LiveSessionView() {
	const { controller, snapshot } = useLiveSession();
	const navigate = useNavigate();
	const { toast } = useToast();
	const [confirmStop, setConfirmStop] = useState(false);
	const [downloading, setDownloading] = useState(false);

	const { phase } = snapshot;
	const recording = phase === "recording";
	const upload = snapshot.upload;
	const serverDown = !snapshot.serverReachable || (upload?.retrying ?? false);
	const pending = Math.max(0, snapshot.recordedChunks - snapshot.uploadedChunks);
	const uploadPercent = snapshot.recordedChunks > 0 ? (snapshot.uploadedChunks / snapshot.recordedChunks) * 100 : 100;

	const handleDownload = async () => {
		if (!snapshot.localId) return;
		setDownloading(true);
		try {
			await downloadLocalRecording({ id: snapshot.localId, title: snapshot.title, mimeType: snapshot.mimeType });
		} catch (error) {
			toast({ title: "Download failed", description: errorMessage(error) });
		} finally {
			setDownloading(false);
		}
	};

	const openTranscript = () => {
		if (snapshot.serverId) navigate(`/audio/${snapshot.serverId}`);
	};

	const placeholder = !snapshot.serverId
		? "Waiting for the server. The recording is being saved in this browser."
		: snapshot.asr?.state === "unavailable" || snapshot.asr?.state === "failed"
			? "Live text is not available right now. The recording continues."
			: recording
				? "Listening. Live text appears here as people speak."
				: "No live text was produced for this meeting.";

	return (
		<div className="space-y-5">
			<div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
				<div className="min-w-0 space-y-1.5">
					<div className="flex flex-wrap items-center gap-2">
						<PhaseBadge snapshot={snapshot} />
						{snapshot.source && (
							<span className="text-xs text-[var(--text-tertiary)]">{SOURCE_LABELS[snapshot.source]}</span>
						)}
					</div>
					<h1 className="truncate text-xl font-display font-bold text-[var(--text-primary)] sm:text-2xl">{snapshot.title}</h1>
				</div>
				{recording && (
					<Button
						size="lg"
						onClick={() => setConfirmStop(true)}
						className="shrink-0 bg-[var(--error)] text-white hover:bg-[var(--error)]/90"
					>
						<Square className="h-4 w-4 fill-current" />
						Stop meeting
					</Button>
				)}
			</div>

			{/* Problems while recording: none of them stop the recording. */}
			{recording && snapshot.systemAudioLost && (
				<LiveBanner
					tone="warning"
					title="Computer audio sharing ended"
					action={
						<Button size="sm" variant="outline" onClick={() => void controller.reshareSystemAudio()}>
							<MonitorSpeaker className="h-4 w-4" />
							Share computer audio again
						</Button>
					}
				>
					The other side of the call is no longer captured. The recording itself keeps going.
				</LiveBanner>
			)}
			{recording && snapshot.micLost && (
				<LiveBanner
					tone="warning"
					title="Microphone disconnected"
					action={
						<Button size="sm" variant="outline" onClick={() => void controller.reconnectMicrophone()}>
							<Mic className="h-4 w-4" />
							Reconnect microphone
						</Button>
					}
				>
					Your voice is no longer captured. The recording itself keeps going.
				</LiveBanner>
			)}
			{recording && upload?.fatal && (
				<LiveBanner tone="error" title="Uploading stopped">
					{upload.fatal} Recording continues in this browser; download it after the meeting.
				</LiveBanner>
			)}
			{recording && !upload?.fatal && upload?.closed && (
				<LiveBanner tone="warning" title="The server closed this meeting">
					The server finalized it with the audio it had (for example after a long outage), so new audio is no longer
					uploaded. Recording continues in this browser; after you stop, download the full recording from the
					recovery list below.
				</LiveBanner>
			)}
			{recording && !upload?.fatal && serverDown && (
				<LiveBanner
					tone="warning"
					title="Can't reach the server"
					action={
						<Button size="sm" variant="outline" onClick={controller.retryNow}>
							<RefreshCw className="h-4 w-4" />
							Retry now
						</Button>
					}
				>
					{pending > 0 ? `${pending} chunk${pending === 1 ? " is" : "s are"} waiting in this browser. ` : ""}
					The recording is safe here and uploads automatically when the server is back.
				</LiveBanner>
			)}
			{recording && snapshot.localSaveFailed && (
				<LiveBanner tone="warning" title="Not saving in this browser">
					The browser refused to store the latest audio (storage full or blocked). The server copy is still being
					written{serverDown ? " once it is reachable" : ""}.
				</LiveBanner>
			)}

			{recording && <LevelMeter analyser={snapshot.analyser} />}

			<LiveStatusBar snapshot={snapshot} />

			{phase === "draining" && (
				<div className="space-y-3 rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] p-4">
					<div className="flex items-center justify-between gap-3 text-sm">
						<span className="font-medium text-[var(--text-primary)]">
							{snapshot.serverId ? "Uploading the rest of the meeting" : "Connecting to the server"}
						</span>
						<span className="tabular-nums text-[var(--text-secondary)]">
							{snapshot.uploadedChunks} / {snapshot.recordedChunks}
						</span>
					</div>
					<Progress value={uploadPercent} className="h-2 bg-[var(--secondary)]" indicatorClassName="bg-[var(--brand-solid)]" />
					{upload?.fatal ? null : serverDown ? (
						<LiveBanner
							tone="warning"
							title="Can't reach the server"
							action={
								<>
									<Button size="sm" variant="outline" onClick={controller.retryNow}>
										<RefreshCw className="h-4 w-4" />
										Retry now
									</Button>
									<Button size="sm" variant="ghost" onClick={controller.keepLocally}>
										Keep in this browser for later
									</Button>
								</>
							}
						>
							The meeting is safe in this browser. Keep this page open to finish automatically, or keep it for later
							and use "Resume upload".
						</LiveBanner>
					) : (
						<p className="text-xs text-[var(--text-tertiary)]">Keep this page open until the upload finishes.</p>
					)}
				</div>
			)}

			{phase === "finalizing" && (
				<div className="flex items-start gap-3 rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] p-4">
					<Loader2 className="mt-0.5 h-5 w-5 flex-shrink-0 animate-spin text-[var(--brand-solid)]" />
					<div className="space-y-1 text-sm">
						<div className="font-medium text-[var(--text-primary)]">Finalizing on the server</div>
						<p className="text-[var(--text-secondary)]">
							Finishing the live transcript and building the final recording. This can take a minute or two. Your
							audio is already safe on the server.
						</p>
						{serverDown && (
							<Button size="sm" variant="ghost" onClick={controller.keepLocally} className="mt-1 -ml-3">
								Stop waiting (check back later)
							</Button>
						)}
					</div>
				</div>
			)}

			{phase === "finalized" && (
				<LiveBanner
					tone={snapshot.incompleteOnServer ? "warning" : "success"}
					title={snapshot.incompleteOnServer ? "Saved, but the server copy is incomplete" : "Meeting saved"}
					action={
						<>
							{snapshot.serverId && (
								<Button size="sm" onClick={openTranscript}>
									<FileText className="h-4 w-4" />
									Open transcript
								</Button>
							)}
							{snapshot.incompleteOnServer && (
								<Button size="sm" variant="outline" onClick={() => void handleDownload()} disabled={downloading}>
									{downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
									Download full recording
								</Button>
							)}
							<Button size="sm" variant="outline" onClick={controller.reset}>
								<Plus className="h-4 w-4" />
								New meeting
							</Button>
						</>
					}
				>
					{snapshot.incompleteOnServer
						? "The server finished this meeting before every chunk arrived. The complete recording is still in this browser; download it and upload it if you need the missing part."
						: snapshot.autoRetranscribe
							? "The live transcript is in your library. A full re-transcription of the recording has been queued."
							: "The live transcript and the full recording are in your library. You can re-transcribe it there with a better model."}
				</LiveBanner>
			)}

			{phase === "failed" && (
				<LiveBanner
					tone="error"
					title="The meeting is not finished on the server"
					action={
						<>
							<Button size="sm" onClick={() => void controller.retry()}>
								<RotateCcw className="h-4 w-4" />
								Try again
							</Button>
							<Button size="sm" variant="outline" onClick={() => void handleDownload()} disabled={downloading}>
								{downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
								Download recording
							</Button>
							{snapshot.serverId && (
								<Button size="sm" variant="outline" onClick={openTranscript}>
									<FileText className="h-4 w-4" />
									Open in library
								</Button>
							)}
							<Button size="sm" variant="ghost" onClick={controller.reset}>
								Back
							</Button>
						</>
					}
				>
					{snapshot.error}
				</LiveBanner>
			)}

			<LiveTranscriptPanel segments={snapshot.segments} partial={snapshot.partial} placeholder={placeholder} />

			<AlertDialog open={confirmStop} onOpenChange={setConfirmStop}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>Stop the meeting?</AlertDialogTitle>
						<AlertDialogDescription>
							Recording ends and the meeting is finalized. A stopped meeting cannot be continued.
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>Keep recording</AlertDialogCancel>
						<AlertDialogAction
							onClick={() => void controller.stop()}
							className="bg-[var(--error)] text-white hover:bg-[var(--error)]/90"
						>
							Stop meeting
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</div>
	);
}
