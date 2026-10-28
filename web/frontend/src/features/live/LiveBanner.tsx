import type { ReactNode } from "react";
import { AlertCircle, CheckCircle, Info, X } from "lucide-react";
import { cn } from "@/lib/utils";

export type BannerTone = "error" | "warning" | "info" | "success";

const toneClasses: Record<BannerTone, string> = {
	error: "bg-[var(--error)]/10 border-[var(--error)]/20",
	warning: "bg-[var(--warning-translucent)] border-[var(--warning-solid)]/20",
	info: "bg-[var(--brand-light)] border-[var(--brand-solid)]/20",
	success: "bg-[var(--success-translucent)] border-[var(--success-solid)]/20",
};

const iconClasses: Record<BannerTone, string> = {
	error: "text-[var(--error)]",
	warning: "text-[var(--warning-solid)]",
	info: "text-[var(--brand-solid)]",
	success: "text-[var(--success-solid)]",
};

interface LiveBannerProps {
	tone: BannerTone;
	title?: string;
	children?: ReactNode;
	action?: ReactNode;
	onDismiss?: () => void;
}

export function LiveBanner({ tone, title, children, action, onDismiss }: LiveBannerProps) {
	const Icon = tone === "info" ? Info : tone === "success" ? CheckCircle : AlertCircle;
	return (
		<div className={cn("flex items-start gap-3 rounded-[var(--radius-card)] border p-3 sm:p-4", toneClasses[tone])}>
			<Icon className={cn("mt-0.5 h-5 w-5 flex-shrink-0", iconClasses[tone])} />
			<div className="min-w-0 flex-1 space-y-2">
				{title && <div className="font-semibold text-sm text-[var(--text-primary)]">{title}</div>}
				{children && <div className="text-sm text-[var(--text-secondary)]">{children}</div>}
				{action && <div className="flex flex-wrap gap-2 pt-1">{action}</div>}
			</div>
			{onDismiss && (
				<button
					type="button"
					onClick={onDismiss}
					className="flex-shrink-0 rounded-md p-1 text-[var(--text-tertiary)] hover:text-[var(--text-primary)] cursor-pointer"
					aria-label="Dismiss"
				>
					<X className="h-4 w-4" />
				</button>
			)}
		</div>
	);
}
