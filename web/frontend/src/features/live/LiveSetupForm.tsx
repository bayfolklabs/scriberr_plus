import { useEffect, useState, type ReactNode } from "react";
import { Loader2, Mic, MonitorSpeaker, Radio } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { useLiveSession } from "./liveSessionContext";
import { useMicrophones } from "./hooks";
import { checkSupport, usesMic, usesSystemAudio, type LiveSource } from "./media";
import { LiveBanner } from "./LiveBanner";

const PREFS_KEY = "scriberr-live-prefs";
const DEFAULT_DEVICE = "__default__";

interface LivePrefs {
	source: LiveSource;
	micDeviceId: string;
	autoRetranscribe: boolean;
}

function loadPrefs(): LivePrefs {
	const fallback: LivePrefs = { source: "mic+system", micDeviceId: "", autoRetranscribe: false };
	try {
		const raw = localStorage.getItem(PREFS_KEY);
		if (!raw) return fallback;
		const parsed = JSON.parse(raw) as Partial<LivePrefs>;
		return {
			source: parsed.source === "mic" || parsed.source === "system" || parsed.source === "mic+system" ? parsed.source : fallback.source,
			micDeviceId: typeof parsed.micDeviceId === "string" ? parsed.micDeviceId : "",
			autoRetranscribe: parsed.autoRetranscribe === true,
		};
	} catch {
		return fallback;
	}
}

function savePrefs(prefs: LivePrefs): void {
	try {
		localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
	} catch {
		/* storage blocked: preferences are a convenience only */
	}
}

const SOURCES: { value: LiveSource; title: string; description: string; icons: ReactNode }[] = [
	{
		value: "mic",
		title: "Microphone",
		description: "In-person meetings, or a call played on speakers.",
		icons: <Mic className="h-4 w-4" />,
	},
	{
		value: "mic+system",
		title: "Microphone + computer audio",
		description: "Online calls: your voice and everyone else's.",
		icons: (
			<>
				<Mic className="h-4 w-4" />
				<MonitorSpeaker className="h-4 w-4" />
			</>
		),
	},
	{
		value: "system",
		title: "Computer audio only",
		description: "Webinars, or a call you only listen to.",
		icons: <MonitorSpeaker className="h-4 w-4" />,
	},
];

export function LiveSetupForm() {
	const { controller, snapshot } = useLiveSession();
	const [initial] = useState(loadPrefs);
	const [title, setTitle] = useState("");
	const [source, setSource] = useState<LiveSource>(initial.source);
	const [micDeviceId, setMicDeviceId] = useState(initial.micDeviceId);
	const [autoRetranscribe, setAutoRetranscribe] = useState(initial.autoRetranscribe);
	const microphones = useMicrophones(usesMic(source));

	const starting = snapshot.phase === "starting";
	const support = checkSupport(source);

	useEffect(() => {
		savePrefs({ source, micDeviceId, autoRetranscribe });
	}, [source, micDeviceId, autoRetranscribe]);

	// Forget a remembered device that is no longer plugged in.
	const deviceAvailable = !micDeviceId || microphones.devices.some((device) => device.deviceId === micDeviceId);
	const selectedDevice = deviceAvailable ? micDeviceId : "";

	const handleStart = () => {
		void controller.start({ title, source, micDeviceId: selectedDevice, autoRetranscribe });
	};

	return (
		<div className="space-y-6">
			<div>
				<h1 className="flex items-center gap-2 text-2xl font-display font-bold text-[var(--text-primary)] mb-2">
					<Radio className="h-6 w-6 text-[var(--brand-solid)]" />
					Live meeting
				</h1>
				<p className="text-[var(--text-secondary)]">
					Transcribes while you record. The full recording is saved in this browser and on the server as it happens,
					so a crash, a dropped connection or slow transcription never costs the meeting.
				</p>
			</div>

			{snapshot.error && (
				<LiveBanner tone="error" title="Could not start" onDismiss={controller.dismissMessage}>
					{snapshot.error}
				</LiveBanner>
			)}
			{snapshot.info && (
				<LiveBanner tone="info" onDismiss={controller.dismissMessage}>
					{snapshot.info}
				</LiveBanner>
			)}

			<div className="space-y-2">
				<Label htmlFor="live-title" className="text-[var(--text-primary)]">
					Title
				</Label>
				<Input
					id="live-title"
					value={title}
					onChange={(event) => setTitle(event.target.value)}
					placeholder="Weekly sync (optional)"
					disabled={starting}
					maxLength={200}
				/>
			</div>

			<div className="space-y-2">
				<div className="text-sm font-medium text-[var(--text-primary)]">Audio source</div>
				<div role="radiogroup" aria-label="Audio source" className="grid grid-cols-1 gap-3 sm:grid-cols-3">
					{SOURCES.map((option) => {
						const selected = option.value === source;
						return (
							<button
								key={option.value}
								type="button"
								role="radio"
								aria-checked={selected}
								disabled={starting}
								onClick={() => setSource(option.value)}
								className={cn(
									"flex flex-col items-start gap-2 rounded-[var(--radius-card)] border p-4 text-left transition-all cursor-pointer disabled:cursor-not-allowed disabled:opacity-60",
									selected
										? "border-[var(--brand-solid)] bg-[var(--brand-light)] shadow-sm"
										: "border-[var(--border-subtle)] bg-[var(--bg-main)] hover:border-[var(--brand-solid)]/50",
								)}
							>
								<span className={cn("flex items-center gap-1", selected ? "text-[var(--brand-solid)]" : "text-[var(--text-tertiary)]")}>
									{option.icons}
								</span>
								<span className="text-sm font-medium text-[var(--text-primary)]">{option.title}</span>
								<span className="text-xs text-[var(--text-secondary)]">{option.description}</span>
							</button>
						);
					})}
				</div>
			</div>

			{usesMic(source) && (
				<div className="space-y-2">
					<Label htmlFor="live-mic" className="text-[var(--text-primary)]">
						Microphone
					</Label>
					<div className="flex flex-col gap-2 sm:flex-row sm:items-center">
						<Select
							value={selectedDevice || DEFAULT_DEVICE}
							onValueChange={(value) => setMicDeviceId(value === DEFAULT_DEVICE ? "" : value)}
							disabled={starting}
						>
							<SelectTrigger id="live-mic" className="w-full sm:flex-1 min-w-0">
								<SelectValue placeholder="Default microphone" />
							</SelectTrigger>
							<SelectContent>
								<SelectItem value={DEFAULT_DEVICE}>
									{microphones.defaultLabel ? `Default (${microphones.defaultLabel})` : "Default microphone"}
								</SelectItem>
								{microphones.devices.map((device) => (
									<SelectItem key={device.deviceId} value={device.deviceId}>
										{device.label}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
						{!microphones.hasLabels && (
							<Button variant="outline" onClick={() => void microphones.requestAccess()} disabled={starting}>
								<Mic className="h-4 w-4" />
								Show microphone names
							</Button>
						)}
					</div>
				</div>
			)}

			{usesSystemAudio(source) && (
				<div className="rounded-[var(--radius-card)] border border-[var(--brand-solid)]/20 bg-[var(--brand-light)] p-4">
					<h3 className="mb-3 flex items-center gap-2 text-sm font-semibold text-[var(--text-primary)]">
						<MonitorSpeaker className="h-4 w-4 text-[var(--brand-solid)]" />
						Sharing computer audio (Chrome or Edge on Windows)
					</h3>
					<ol className="space-y-2 text-sm text-[var(--text-secondary)]">
						<li className="flex gap-3">
							<span className="flex-shrink-0 font-bold text-[var(--brand-solid)]">1.</span>
							<span>
								<strong className="text-[var(--text-primary)]">Desktop Zoom or Teams:</strong> choose{" "}
								<strong className="text-[var(--text-primary)]">Entire screen</strong> and turn on{" "}
								<strong className="text-[var(--text-primary)]">Share system audio</strong>.
							</span>
						</li>
						<li className="flex gap-3">
							<span className="flex-shrink-0 font-bold text-[var(--brand-solid)]">2.</span>
							<span>
								<strong className="text-[var(--text-primary)]">Meeting in a browser tab:</strong> choose that{" "}
								<strong className="text-[var(--text-primary)]">tab</strong> and turn on{" "}
								<strong className="text-[var(--text-primary)]">Share tab audio</strong>.
							</span>
						</li>
						<li className="flex gap-3">
							<span className="flex-shrink-0 font-bold text-[var(--brand-solid)]">3.</span>
							<span>
								Sharing a single <strong className="text-[var(--text-primary)]">Window</strong> includes no audio. Only the
								sound is kept; the picture is dropped immediately.
							</span>
						</li>
					</ol>
					{source === "mic+system" && (
						<p className="mt-3 rounded-[var(--radius-btn)] border border-[var(--warning-solid)]/20 bg-[var(--warning-translucent)] p-3 text-xs text-[var(--text-secondary)]">
							<strong>Tip:</strong> wear headphones, so the microphone does not pick up the other side of the call a
							second time.
						</p>
					)}
				</div>
			)}

			<div className="flex items-start gap-3 rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] p-3">
				<Checkbox
					id="live-retranscribe"
					checked={autoRetranscribe}
					onCheckedChange={(checked) => setAutoRetranscribe(checked === true)}
					disabled={starting}
					className="mt-0.5"
				/>
				<div className="space-y-1">
					<Label htmlFor="live-retranscribe" className="cursor-pointer text-[var(--text-primary)]">
						Re-transcribe full recording after the meeting
					</Label>
					<p className="text-xs text-[var(--text-tertiary)]">
						Runs your default transcription profile on the complete recording once the meeting ends. The live
						transcript is kept either way.
					</p>
				</div>
			</div>

			{support.error && <LiveBanner tone="error">{support.error}</LiveBanner>}
			{!support.error && support.warning && <LiveBanner tone="warning">{support.warning}</LiveBanner>}

			<Button
				onClick={handleStart}
				size="lg"
				disabled={starting || !!support.error}
				className="w-full rounded-xl text-white cursor-pointer bg-gradient-to-r from-[#FFAB40] to-[#FF3D00] hover:opacity-90 active:scale-[0.98] transition-all shadow-lg shadow-orange-500/20"
			>
				{starting ? (
					<>
						<Loader2 className="h-5 w-5 animate-spin" />
						Starting...
					</>
				) : (
					<>
						<Radio className="h-5 w-5" />
						Start live meeting
					</>
				)}
			</Button>
		</div>
	);
}
