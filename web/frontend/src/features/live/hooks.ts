import { useCallback, useEffect, useState } from "react";

/** Current time, refreshed every intervalMs while intervalMs is not null. */
export function useNow(intervalMs: number | null): number {
	const [now, setNow] = useState(() => Date.now());
	useEffect(() => {
		if (intervalMs === null) return;
		setNow(Date.now());
		const timer = window.setInterval(() => setNow(Date.now()), intervalMs);
		return () => window.clearInterval(timer);
	}, [intervalMs]);
	return now;
}

export interface MicrophoneOption {
	deviceId: string;
	label: string;
}

/**
 * Audio input devices. Browsers hide device names until microphone permission is granted,
 * so `hasLabels` tells the UI whether to offer `requestAccess`.
 */
export function useMicrophones(enabled: boolean) {
	const [devices, setDevices] = useState<MicrophoneOption[]>([]);
	const [defaultLabel, setDefaultLabel] = useState("");
	const [hasLabels, setHasLabels] = useState(false);

	const refresh = useCallback(async () => {
		if (!navigator.mediaDevices?.enumerateDevices) return;
		try {
			const all = await navigator.mediaDevices.enumerateDevices();
			const inputs = all.filter((device) => device.kind === "audioinput" && device.deviceId);
			// Chromium lists a "default" pseudo-device; it is offered as the default option instead.
			const fallback = inputs.find((device) => device.deviceId === "default");
			setDefaultLabel(fallback?.label.replace(/^Default\s*-\s*/i, "") ?? "");
			setDevices(
				inputs
					.filter((device) => device.deviceId !== "default")
					.map((device, index) => ({
						deviceId: device.deviceId,
						label: device.label || `Microphone ${index + 1}`,
					})),
			);
			setHasLabels(inputs.some((device) => device.label));
		} catch (error) {
			console.warn("Live: could not list microphones", error);
		}
	}, []);

	const requestAccess = useCallback(async (): Promise<boolean> => {
		try {
			const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
			stream.getTracks().forEach((track) => track.stop());
			await refresh();
			return true;
		} catch (error) {
			console.warn("Live: microphone permission not granted", error);
			return false;
		}
	}, [refresh]);

	useEffect(() => {
		if (!enabled || !navigator.mediaDevices) return;
		const mediaDevices = navigator.mediaDevices;
		const handleChange = () => {
			void refresh();
		};
		void refresh();
		mediaDevices.addEventListener("devicechange", handleChange);
		return () => mediaDevices.removeEventListener("devicechange", handleChange);
	}, [enabled, refresh]);

	return { devices, defaultLabel, hasLabels, refresh, requestAccess };
}
