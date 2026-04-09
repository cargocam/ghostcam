import type { CameraInfo, TelemetryData } from '$lib/types.js';

export interface CameraState {
	device_id: string;
	device_name: string;
	/** Set by server-side camera_status SSE events. */
	online: boolean;
	telemetry: TelemetryData | null;
	/** Epoch ms when we last received telemetry for this camera. */
	lastTelemetryAt: number | null;
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

	setInitialList(list: CameraInfo[]) {
		this.cameras = list.map((c) => {
			// Telemetry seeded from API response. Online status will be set by
			// the server's camera_status SSE event burst on connect.
			const t = c.telemetry;
			const initialTelemetry: TelemetryData | null = t ? {
				cpu_percent: t.cpu,
				temp_celsius: t.temp,
				memory_mb: t.mem,
				uptime_secs: t.uptime,
				gps: t.lat != null && t.lon != null
					? { latitude: t.lat, longitude: t.lon, alt: t.alt }
					: undefined,
			} : null;
			return {
				device_id: c.device_id,
				device_name: c.display_name,
				online: false,
				telemetry: initialTelemetry,
				lastTelemetryAt: null,
				resolution: c.resolution ?? '720p',
				recording_mode: c.recording_mode ?? 'constant',
			};
		});
	}

	setTelemetry(deviceId: string, data: TelemetryData) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].telemetry = data;
			this.cameras[idx].lastTelemetryAt = Date.now();
		}
	}

	/** Set online/offline status from server-side camera_status SSE event. */
	setOnlineStatus(deviceId: string, online: boolean) {
		const cam = this.cameras.find((c) => c.device_id === deviceId);
		if (cam) {
			cam.online = online;
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
