import type { CameraInfo, TelemetryData } from '$lib/types.js';

export interface CameraState {
	device_id: string;
	device_name: string;
	online: boolean;
	videoStream: MediaStream | null;
	audioStream: MediaStream | null;
	telemetry: TelemetryData | null;
	lastTelemetryAt: number | null;
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

	isOnline(deviceId: string): boolean {
		return this.cameras.find((c) => c.device_id === deviceId)?.online ?? false;
	}

	setInitialList(list: CameraInfo[]) {
		this.cameras = list.map((c) => ({
			device_id: c.device_id,
			device_name: c.display_name,
			online: c.online,
			videoStream: null,
			audioStream: null,
			telemetry: null,
			lastTelemetryAt: null,
		}));
	}

	setOnline(deviceId: string, online: boolean) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].online = online;
			if (!online) {
				this.cameras[idx].videoStream = null;
				this.cameras[idx].audioStream = null;
			}
		} else {
			this.cameras = [
				...this.cameras,
				{
					device_id: deviceId,
					device_name: deviceId,
					online,
					videoStream: null,
					audioStream: null,
					telemetry: null,
					lastTelemetryAt: null,
				},
			];
		}
	}

	setVideoStream(deviceId: string, stream: MediaStream) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].videoStream = stream;
		}
	}

	setAudioStream(deviceId: string, stream: MediaStream) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].audioStream = stream;
		}
	}

	setTelemetry(deviceId: string, data: TelemetryData) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].telemetry = data;
			this.cameras[idx].lastTelemetryAt = Date.now();
		}
	}

	clearStreams(deviceId: string) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].videoStream = null;
			this.cameras[idx].audioStream = null;
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
