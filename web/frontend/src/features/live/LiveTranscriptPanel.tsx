import { useLayoutEffect, useRef, useState } from "react";
import { ArrowDownCircle } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { LivePartial, LiveSegment } from "./api";
import { formatClock } from "./format";

interface LiveTranscriptPanelProps {
	segments: LiveSegment[];
	partial: LivePartial | null;
	/** Shown while there is no text yet. */
	placeholder: string;
}

const FOLLOW_THRESHOLD_PX = 48;

/** Final segments with timestamps, the current partial in a muted style, and follow-the-bottom scrolling. */
export function LiveTranscriptPanel({ segments, partial, placeholder }: LiveTranscriptPanelProps) {
	const scrollRef = useRef<HTMLDivElement>(null);
	const followRef = useRef(true);
	const [following, setFollowing] = useState(true);

	const handleScroll = () => {
		const el = scrollRef.current;
		if (!el) return;
		const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < FOLLOW_THRESHOLD_PX;
		if (atBottom !== followRef.current) {
			followRef.current = atBottom;
			setFollowing(atBottom);
		}
	};

	const jumpToLive = () => {
		const el = scrollRef.current;
		if (!el) return;
		followRef.current = true;
		setFollowing(true);
		el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
	};

	// Keep the newest text in view unless the user scrolled up to read.
	useLayoutEffect(() => {
		const el = scrollRef.current;
		if (el && followRef.current) el.scrollTop = el.scrollHeight;
	}, [segments, partial]);

	const isEmpty = segments.length === 0 && !partial;

	return (
		<div className="relative">
			<div
				ref={scrollRef}
				onScroll={handleScroll}
				className="h-[45vh] min-h-[260px] max-h-[560px] overflow-y-auto custom-scrollbar rounded-[var(--radius-card)] border border-[var(--border-subtle)] bg-[var(--bg-main)] px-4 py-3 sm:px-5"
				aria-live="polite"
				aria-label="Live transcript"
			>
				{isEmpty ? (
					<div className="flex h-full items-center justify-center text-center text-sm text-[var(--text-tertiary)] px-6">
						{placeholder}
					</div>
				) : (
					<div className="space-y-1">
						{segments.map((segment) => (
							<div key={segment.seq} className="flex gap-3 py-1">
								<span className="shrink-0 pt-0.5 font-mono text-xs tabular-nums text-[var(--text-tertiary)]">
									{formatClock(segment.start)}
								</span>
								<p className="text-[15px] leading-relaxed text-[var(--text-primary)]">{segment.text}</p>
							</div>
						))}
						{partial && (
							<div className="flex gap-3 py-1">
								<span className="shrink-0 pt-0.5 font-mono text-xs tabular-nums text-[var(--text-tertiary)]">
									{formatClock(partial.start)}
								</span>
								<p className="text-[15px] leading-relaxed italic text-[var(--text-tertiary)]">
									{partial.text}
									<span className="ml-0.5 inline-block h-4 w-[2px] translate-y-0.5 animate-pulse bg-[var(--brand-solid)]" />
								</p>
							</div>
						)}
					</div>
				)}
			</div>

			{!following && !isEmpty && (
				<Button
					size="sm"
					variant="outline"
					onClick={jumpToLive}
					className="absolute bottom-3 left-1/2 -translate-x-1/2 shadow-[var(--shadow-float)] rounded-full"
				>
					<ArrowDownCircle className="h-4 w-4" />
					Jump to live
				</Button>
			)}
		</div>
	);
}
