import { useEffect, useRef } from "react";

/**
 * Input level of the mixed recording, drawn straight to the DOM on each animation frame
 * (no React state), so it costs nothing when the tab is hidden.
 */
export function LevelMeter({ analyser }: { analyser: AnalyserNode | null }) {
	const barRef = useRef<HTMLDivElement>(null);

	useEffect(() => {
		const bar = barRef.current;
		if (!analyser || !bar) return;
		const samples = new Float32Array(analyser.fftSize);
		let frame = 0;
		let level = 0;
		const draw = () => {
			analyser.getFloatTimeDomainData(samples);
			let sum = 0;
			for (let i = 0; i < samples.length; i++) sum += samples[i] * samples[i];
			const rms = Math.sqrt(sum / samples.length);
			// Fast attack, slow release, roughly perceptual scaling.
			const target = Math.min(1, Math.sqrt(rms) * 1.6);
			level = target > level ? target : level * 0.92 + target * 0.08;
			bar.style.width = `${(level * 100).toFixed(1)}%`;
			frame = requestAnimationFrame(draw);
		};
		frame = requestAnimationFrame(draw);
		return () => {
			cancelAnimationFrame(frame);
			bar.style.width = "0%";
		};
	}, [analyser]);

	return (
		<div className="flex items-center gap-3" aria-hidden="true">
			<span className="text-xs font-medium text-[var(--text-tertiary)] w-10">Input</span>
			<div className="h-1.5 flex-1 overflow-hidden rounded-full bg-[var(--secondary)]">
				<div
					ref={barRef}
					className="h-full w-0 rounded-full bg-gradient-to-r from-[#FFAB40] to-[#FF3D00] transition-[width] duration-75"
				/>
			</div>
		</div>
	);
}
