import { useCallback, useEffect, useState } from "react";
import { CloudUpload, Download, HardDrive, Loader2, RefreshCw, Server, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
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
import { heldSessionLocks, isActivePhase } from "./liveController";
import {
	deleteSession,
	downloadLocalRecording,
	listSessions,
	pruneFinalizedSessions,
	type LocalSession,
	type LocalSessionState,
} from "./chunkStore";
import { LiveApiError, finalizeSession, listServerSessions, type LiveServerSession } from "./api";
import { errorMessage, formatBytes, formatClock, formatDateTime } from "./format";

interface LocalRow {
	session: LocalSession;
	/** Another tab holds this meeting's lock, i.e. it is still recording or uploading there. */
	lockedElsewhere: boolean;
}

const LOCAL_STATE_LABELS: Record<LocalSessionState, string> = {
	recording: "Interrupted while recording",
	stopping: "Interrupted while stopping",
	uploading: "Upload not finished",
	finalizing: "Waiting for the server",
	finalized: "Finalized",
	failed: "Needs attention",
};

function StateBadge({ children, tone }: { children: string; tone: "warn" | "neutral" | "bad" }) {
	return (
		<span
			className={cn(
				"inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium",
				tone === "warn" && "bg-[var(--warning-translucent)] text-[var(--warning-solid)]",
				tone === "bad" && "bg-[var(--error)]/10 text-[var(--error)]",
				tone === "neutral" && "bg-[var(--secondary)] text-[var(--text-secondary)]",
			)}
		>
			{children}
		</span>
	);
}

/**
 * Meetings that did not finish: copies saved in this browser (resume, download, discard) and
 * sessions the server still considers open (finalize there).
 */
export function LiveRecoveryPanel() {
	const { controller, snapshot } = useLiveSession();
	const { toast } = useToast();
	const [localRows, setLocalRows] = useState<LocalRow[]>([]);
	const [localError, setLocalError] = useState<string | null>(null);
	const [serverSessions, setServerSessions] = useState<LiveServerSession[]>([]);
	const [serverNote, setServerNote] = useState<string | null>(null);
	const [loading, setLoading] = useState(false);
	const [busyId, setBusyId] = useState<string | null>(null);
	const [discardTarget, setDiscardTarget] = useState<LocalSession | null>(null);
	const [finalizeTarget, setFinalizeTarget] = useState<LiveServerSession | null>(null);

	const active = isActivePhase(snapshot.phase);
	const activeLocalId = snapshot.localId;
	const activeServerId = snapshot.serverId;

	const refresh = useCallback(async () => {
		setLoading(true);
		try {
			await pruneFinalizedSessions().catch(() => undefined);
			const [sessions, held] = await Promise.all([listSessions(), heldSessionLocks()]);
			setLocalRows(
				sessions
					.filter((session) => session.state !== "finalized")
					.map((session) => ({ session, lockedElsewhere: held.has(session.id) })),
			);
			setLocalError(null);
		} catch (error) {
			setLocalRows([]);
			setLocalError(`Could not read meetings saved in this browser: ${errorMessage(error)}`);
		}
		try {
			// "failed" = a server-side finalize failed; "Finalize on server" retries it
			setServerSessions(await listServerSessions(["recording", "interrupted", "failed"]));
			setServerNote(null);
		} catch (error) {
			setServerSessions([]);
			setServerNote(
				error instanceof LiveApiError && error.status === 404
					? "Live meetings are not enabled on this server."
					: `Could not load open meetings from the server: ${errorMessage(error)}`,
			);
		} finally {
			setLoading(false);
		}
		void controller.refreshRecoverable();
	}, [controller]);

	// Reload whenever a meeting starts, finishes or is set aside.
	useEffect(() => {
		void refresh();
	}, [refresh, snapshot.phase]);

	// The meeting shown above (active, finished or failed) is handled there, not here.
	const visibleLocal = localRows.filter((row) => row.session.id !== activeLocalId);
	const visibleServer = serverSessions.filter((session) => session.id !== activeServerId);
	const localByServerId = new Map(
		localRows.filter((row) => row.session.serverId).map((row) => [row.session.serverId as string, row.session]),
	);

	const handleResume = (session: LocalSession) => {
		window.scrollTo({ top: 0, behavior: "smooth" });
		void controller.resume(session.id);
	};

	const handleDownload = async (session: LocalSession) => {
		setBusyId(session.id);
		try {
			await downloadLocalRecording(session);
		} catch (error) {
			toast({ title: "Download failed", description: errorMessage(error) });
		} finally {
			setBusyId(null);
		}
	};

	const handleDiscard = async () => {
		const target = discardTarget;
		setDiscardTarget(null);
		if (!target) return;
		setBusyId(target.id);
		try {
			await deleteSession(target.id);
			toast({ title: "Recording discarded", description: `"${target.title}" was removed from this browser.` });
		} catch (error) {
			toast({ title: "Could not discard", description: errorMessage(error) });
		} finally {
			setBusyId(null);
			void refresh();
		}
	};

	const handleFinalize = async () => {
		const target = finalizeTarget;
		setFinalizeTarget(null);
		if (!target) return;
		setBusyId(target.id);
		try {
			await finalizeSession(target.id);
			toast({ title: "Finalizing on the server", description: "The meeting will appear in your library shortly." });
		} catch (error) {
			toast({ title: "Could not finalize", description: errorMessage(error) });
		} finally {
			setBusyId(null);
			void refresh();
		}
	};

	if (visibleLocal.length === 0 && visibleServer.length === 0 && !localError) {
		return null;
	}

	const finalizeLocal = finalizeTarget ? localByServerId.get(finalizeTarget.id) : undefined;
	const finalizeLosesAudio =
		!!finalizeTarget &&
		!!finalizeLocal &&
		typeof finalizeTarget.next_seq === "number" &&
		finalizeLocal.lastSeq >= finalizeTarget.next_seq;

	return (
		<section className="bg-[var(--bg-card)] border border-[var(--border-subtle)] shadow-[var(--shadow-card)] rounded-[var(--radius-card)] p-4 sm:p-6 space-y-5">
			<div className="flex items-start justify-between gap-3">
				<div>
					<h2 className="text-lg font-semibold text-[var(--text-primary)]">Unfinished meetings</h2>
					<p className="text-sm text-[var(--text-secondary)] mt-1">
						Meetings that were interrupted before they were saved. Nothing here is lost until you discard it.
					</p>
				</div>
				<Button variant="ghost" size="icon" onClick={() => void refresh()} disabled={loading} aria-label="Refresh">
					<RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} />
				</Button>
			</div>

			{localError && <p className="text-sm text-[var(--error)]">{localError}</p>}

			{visibleLocal.length > 0 && (
				<div className="space-y-3">
					<h3 className="flex items-center gap-2 text-sm font-medium text-[var(--text-secondary)]">
						<HardDrive className="h-4 w-4" />
						Saved in this browser
					</h3>
					{visibleLocal.map(({ session, lockedElsewhere }) => {
						const recorded = session.lastSeq + 1;
						const uploaded = Math.max(0, session.ackedSeq + 1);
						const busy = busyId === session.id;
						return (
							<div
								key={session.id}
								className="rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] p-4 space-y-3"
							>
								<div className="flex flex-wrap items-center gap-2">
									<span className="font-medium text-[var(--text-primary)] truncate">{session.title}</span>
									{lockedElsewhere ? (
										<StateBadge tone="neutral">Open in another tab</StateBadge>
									) : (
										<StateBadge tone={session.state === "failed" ? "bad" : "warn"}>
											{LOCAL_STATE_LABELS[session.state]}
										</StateBadge>
									)}
								</div>
								<div className="text-xs text-[var(--text-tertiary)] space-y-1">
									<div>
										{formatDateTime(session.createdAt)} · about {formatClock(recorded)} recorded · {uploaded} of {recorded}{" "}
										chunks on the server{session.serverId ? "" : " (not started on the server yet)"}
									</div>
									{session.error && <div className="text-[var(--text-secondary)]">{session.error}</div>}
								</div>
								<div className="flex flex-wrap gap-2">
									<Button
										size="sm"
										onClick={() => handleResume(session)}
										disabled={active || lockedElsewhere || busy}
										title={active ? "Finish the current meeting first" : undefined}
									>
										<CloudUpload className="h-4 w-4" />
										Resume upload
									</Button>
									<Button
										size="sm"
										variant="outline"
										onClick={() => void handleDownload(session)}
										disabled={busy || recorded === 0}
									>
										{busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
										Download recording
									</Button>
									<Button
										size="sm"
										variant="ghost"
										onClick={() => setDiscardTarget(session)}
										disabled={lockedElsewhere || busy}
										className="text-[var(--error)] hover:text-[var(--error)]"
									>
										<Trash2 className="h-4 w-4" />
										Discard
									</Button>
								</div>
							</div>
						);
					})}
				</div>
			)}

			{visibleServer.length > 0 && (
				<div className="space-y-3">
					<h3 className="flex items-center gap-2 text-sm font-medium text-[var(--text-secondary)]">
						<Server className="h-4 w-4" />
						Open on the server
					</h3>
					{visibleServer.map((session) => {
						const local = localByServerId.get(session.id);
						const busy = busyId === session.id;
						return (
							<div
								key={session.id}
								className="rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] p-4 space-y-3"
							>
								<div className="flex flex-wrap items-center gap-2">
									<span className="font-medium text-[var(--text-primary)] truncate">{session.title || "Untitled meeting"}</span>
									<StateBadge tone={session.live_status === "interrupted" ? "warn" : "neutral"}>
										{session.live_status === "interrupted" ? "Interrupted" : "Recording"}
									</StateBadge>
								</div>
								<div className="text-xs text-[var(--text-tertiary)] space-y-1">
									<div>
										{[
											formatDateTime(session.created_at),
											typeof session.bytes === "number" ? `${formatBytes(session.bytes)} received` : "",
											typeof session.next_seq === "number" ? `${session.next_seq} chunks` : "",
										]
											.filter(Boolean)
											.join(" · ")}
									</div>
									{local && (
										<div className="text-[var(--text-secondary)]">
											Also saved in this browser. "Resume upload" above sends the rest and finalizes it.
										</div>
									)}
								</div>
								<Button size="sm" variant="outline" onClick={() => setFinalizeTarget(session)} disabled={busy}>
									{busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Server className="h-4 w-4" />}
									Finalize on server
								</Button>
							</div>
						);
					})}
				</div>
			)}

			{serverNote && <p className="text-xs text-[var(--text-tertiary)]">{serverNote}</p>}

			<AlertDialog open={!!discardTarget} onOpenChange={(open) => !open && setDiscardTarget(null)}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>Discard this recording?</AlertDialogTitle>
						<AlertDialogDescription>
							This deletes the copy of "{discardTarget?.title}" saved in this browser. Audio already uploaded stays on the
							server. This cannot be undone.
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>Keep it</AlertDialogCancel>
						<AlertDialogAction
							onClick={() => void handleDiscard()}
							className="bg-[var(--error)] text-white hover:bg-[var(--error)]/90"
						>
							Discard
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>

			<AlertDialog open={!!finalizeTarget} onOpenChange={(open) => !open && setFinalizeTarget(null)}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>Finalize on the server?</AlertDialogTitle>
						<AlertDialogDescription>
							{finalizeLosesAudio
								? "This browser holds audio the server has not received yet. Finalizing now leaves it out; use \"Resume upload\" instead to keep everything."
								: finalizeTarget?.live_status === "recording"
									? "The server still treats this meeting as recording, so it may be live in another tab or device. Finalizing ends it with the audio received so far."
									: "The meeting is closed with the audio the server received, and its live transcript is saved to your library."}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>Cancel</AlertDialogCancel>
						<AlertDialogAction onClick={() => void handleFinalize()}>Finalize</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}
