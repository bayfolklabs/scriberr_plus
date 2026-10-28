import { useEffect, useState, type PropsWithChildren } from "react";
import { useToast } from "@/components/ui/toast";
import { getLiveController } from "./liveController";
import { LiveSessionContext } from "./liveSessionContext";

/**
 * Mounted above the routes (and above the auth gate) so a live meeting keeps recording while
 * the user moves around the app. The controller itself is a module singleton; this provider
 * hands it to the tree and turns its notices into toasts.
 */
export function LiveSessionProvider({ children }: PropsWithChildren) {
	const [controller] = useState(getLiveController);
	const { toast } = useToast();

	useEffect(() => controller.onNotice(toast), [controller, toast]);

	// Look for meetings a crashed or closed tab left unfinished.
	useEffect(() => {
		void controller.refreshRecoverable();
	}, [controller]);

	return <LiveSessionContext.Provider value={controller}>{children}</LiveSessionContext.Provider>;
}
