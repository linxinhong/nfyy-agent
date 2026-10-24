import type { MeResponse } from "@/lib/api/eap";

/** An opaque in-memory marker; the employee key remains in Electron's encrypted host store. */
export const desktopBridgeToken = "yuanpu-desktop-bridge";

export interface DesktopPortalResponse {
	status: number;
	body: unknown;
	contentType: string;
}

declare global {
	interface Window {
		yuanpuPortal?: {
			restore: () => Promise<MeResponse | null>;
			request: (input: {
				path: string;
				method?: string;
				body?: string;
				workspace?: string;
				binary?: boolean;
			}) => Promise<DesktopPortalResponse>;
			saveAPIKey: (key: string) => Promise<unknown>;
			clearAPIKey: () => Promise<void>;
		};
	}
}
