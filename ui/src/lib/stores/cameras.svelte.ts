import type { CameraInfo, TelemetryData } from '$lib/types.js';

/** Camera is online if we received telemetry within this window (ms). */
const ONLINE_THRESHOLD_MS = 30_000;

export interface CameraState {
	device_id: string;
	device_name: string;
	/** Derived client-side from lastTelemetryAt freshness. */
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

	onlineCount = $derived(this.cameras.filter((c) => this.isOnline(c.device_id)).length);

	getCamera(deviceId: string): CameraState | undefined {
		return this.cameras.find((c) => c.device_id === deviceId);
	}

	/** Camera is online if telemetry was received within ONLINE_THRESHOLD_MS. */
	isOnline(deviceId: string): boolean {
		const cam = this.cameras.find((c) => c.device_id === deviceId);
		if (!cam?.lastTelemetryAt) return false;
		return Date.now() - cam.lastTelemetryAt < ONLINE_THRESHOLD_MS;
	}

	setInitialList(list: CameraInfo[]) {
		this.cameras = list.map((c) => ({
			device_id: c.device_id,
			device_name: c.display_name,
			online: false,
			telemetry: null,
			lastTelemetryAt: null,
			resolution: c.resolution ?? '720p',
			recording_mode: c.recording_mode ?? 'constant',
		}));
	}

	setTelemetry(deviceId: string, data: TelemetryData) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].telemetry = data;
			this.cameras[idx].lastTelemetryAt = Date.now();
			this.cameras[idx].online = true;
		}
	}

	/** Recompute online flags based on telemetry freshness. Called periodically. */
	refreshOnlineStatus() {
		const now = Date.now();
		for (const cam of this.cameras) {
			cam.online = cam.lastTelemetryAt != null && now - cam.lastTelemetryAt < ONLINE_THRESHOLD_MS;
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
