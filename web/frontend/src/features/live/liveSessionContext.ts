import { createContext, useContext, useSyncExternalStore } from "react";
import { getLiveController, type LiveSessionController, type LiveSnapshot } from "./liveController";

export const LiveSessionContext = createContext<LiveSessionController | null>(null);

/** The live meeting controller and its current state. Re-renders on every state change. */
export function useLiveSession(): { controller: LiveSessionController; snapshot: LiveSnapshot } {
	const controller = useContext(LiveSessionContext) ?? getLiveController();
	const snapshot = useSyncExternalStore(controller.subscribe, controller.getSnapshot);
	return { controller, snapshot };
}
