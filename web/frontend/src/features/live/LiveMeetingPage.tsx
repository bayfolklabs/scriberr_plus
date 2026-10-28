import { Header } from "@/components/Header";
import { MainLayout } from "@/components/layout/MainLayout";
import { useLiveSession } from "./liveSessionContext";
import { LiveSetupForm } from "./LiveSetupForm";
import { LiveSessionView } from "./LiveSessionView";
import { LiveRecoveryPanel } from "./LiveRecoveryPanel";

/** /live: start a live meeting, follow it, and recover meetings that did not finish. */
export function LiveMeetingPage() {
	const { snapshot } = useLiveSession();
	const showSetup = snapshot.phase === "idle" || snapshot.phase === "starting";

	return (
		<MainLayout header={<Header />}>
			<div className="space-y-6">
				<section className="bg-[var(--bg-card)] border border-[var(--border-subtle)] shadow-[var(--shadow-card)] rounded-[var(--radius-card)] p-4 sm:p-6">
					{showSetup ? <LiveSetupForm /> : <LiveSessionView />}
				</section>
				<LiveRecoveryPanel />
			</div>
		</MainLayout>
	);
}
