import type { CameraResponse } from '$lib/api-types';
import type { TelemetryData } from '$lib/types.js';

/**
 * How long (ms) since the last server_ts before considering a camera offline.
 * The camera posts telemetry every 10s; 30s gives ~2 missed polls of grace.
 */
const ONLINE_STALE_MS = 30_000;

export interface CameraState {
	device_id: string;
	device_name: string;
	/** Derived from server_ts freshness of last telemetry. */
	online: boolean;
	telemetry: TelemetryData | null;
	/** server_ts (epoch ms) from the most recent telemetry for this camera. */
	lastServerTs: number | null;
	resolution: string;
	recording_mode: string;
}

class CameraStore {
	cameras = $state<CameraState[]>([]);
	selectedId = $state<string | null>(null);

	selected = $derived(
		this.selectedId ? this.cameras.find((c) => c.device_id === this.selectedId) ?? null : null
	);

	onlineCount = $derived(this.cameras.filter((c) => c.online).length);

	getCamera(deviceId: string): CameraState | undefined {
		return this.cameras.find((c) => c.device_id === deviceId);
	}

	setInitialList(list: CameraResponse[]) {
		this.cameras = list.map((c) => {
			const t = c.telemetry;
			const initialTelemetry: TelemetryData | null = t ? {
				cpu_percent: t.cpu,
				temp_celsius: t.temp,
				memory_mb: t.mem,
				uptime_secs: t.uptime,
				gps: t.lat != null && t.lon != null
					? { latitude: t.lat, longitude: t.lon, alt: t.alt ?? undefined }
					: undefined,
			} : null;
			return {
				device_id: c.device_id,
				device_name: c.display_name,
				online: false, // SSE telemetry burst will update this immediately
				telemetry: initialTelemetry,
				lastServerTs: null,
				resolution: c.resolution ?? '720p',
				recording_mode: c.recording_mode ?? 'never',
			};
		});
	}

	/**
	 * Update telemetry for a camera. Derives online status from server_ts:
	 * if the server received this telemetry recently, the camera is online.
	 */
	setTelemetry(deviceId: string, data: TelemetryData, serverTs: number) {
		const cam = this.cameras.find((c) => c.device_id === deviceId);
		if (!cam) return;
		cam.telemetry = data;
		cam.lastServerTs = serverTs;
		cam.online = Date.now() - serverTs < ONLINE_STALE_MS;
	}

	/**
	 * Re-evaluate online status for all cameras based on server_ts staleness.
	 * Call periodically (e.g. every 10s) so cameras that stop reporting
	 * transition to offline even without new telemetry events.
	 */
	recheckOnline() {
		const now = Date.now();
		for (const cam of this.cameras) {
			cam.online = cam.lastServerTs != null && now - cam.lastServerTs < ONLINE_STALE_MS;
		}
	}

	removeCamera(deviceId: string) {
		this.cameras = this.cameras.filter((c) => c.device_id !== deviceId);
		if (this.selectedId === deviceId) {
			this.selectedId = null;
		}
	}

	select(id: string | null) {
		this.selectedId = this.selectedId === id ? null : id;
	}

	clear() {
		this.cameras = [];
		this.selectedId = null;
	}
}

export const cameraStore = new CameraStore();
