// Small formatting helpers shared by the live meeting UI.

const pad = (value: number) => value.toString().padStart(2, "0");

/** Seconds as mm:ss. Minutes keep counting past 59 (72:05), which suits transcript timestamps. */
export function formatClock(totalSeconds: number): string {
	const seconds = Math.max(0, Math.floor(totalSeconds || 0));
	return `${pad(Math.floor(seconds / 60))}:${pad(seconds % 60)}`;
}

/** Seconds as mm:ss, or h:mm:ss once an hour has passed. Used for elapsed meeting time. */
export function formatElapsed(totalSeconds: number): string {
	const seconds = Math.max(0, Math.floor(totalSeconds || 0));
	const hours = Math.floor(seconds / 3600);
	if (hours === 0) return formatClock(seconds);
	return `${hours}:${pad(Math.floor((seconds % 3600) / 60))}:${pad(seconds % 60)}`;
}

export function formatBytes(bytes: number): string {
	if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
	const units = ["B", "KB", "MB", "GB"];
	const exponent = Math.min(units.length - 1, Math.floor(Math.log(bytes) / Math.log(1024)));
	const value = bytes / Math.pow(1024, exponent);
	return `${value >= 10 || exponent === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[exponent]}`;
}

export function formatDateTime(value: number | string | undefined): string {
	if (value === undefined || value === "") return "";
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return "";
	return date.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export function defaultMeetingTitle(): string {
	return `Meeting ${formatDateTime(Date.now())}`;
}

export function errorMessage(error: unknown): string {
	if (error instanceof Error && error.message) return error.message;
	if (typeof error === "string" && error) return error;
	return "Something went wrong";
}
