import type { CameraInfo, TelemetryData } from '$lib/types.js';

export interface CameraState extends CameraInfo {
	telemetry?: TelemetryData;
	stream?: MediaStream;
	audioStream?: MediaStream;
}

class CameraStore {
	cameras = $state<CameraState[]>([]);
	selectedId = $state<string | null>(null);

	selected = $derived(
		this.selectedId ? this.cameras.find((c) => c.device_id === this.selectedId) ?? null : null
	);

	onlineCount = $derived(this.cameras.filter((c) => c.connected).length);

	setCameras(list: CameraInfo[]) {
		this.cameras = list.map((cam) => {
			const existing = this.cameras.find((c) => c.device_id === cam.device_id);
			return { ...cam, connected: true, telemetry: existing?.telemetry, stream: existing?.stream, audioStream: existing?.audioStream };
		});
	}

	addCamera(camera: CameraInfo) {
		const idx = this.cameras.findIndex((c) => c.device_id === camera.device_id);
		if (idx >= 0) {
			this.cameras[idx] = { ...this.cameras[idx], ...camera, connected: true };
		} else {
			this.cameras = [...this.cameras, { ...camera, connected: true }];
		}
	}

	removeCamera(deviceId: string) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].connected = false;
			this.cameras[idx].stream = undefined;
			this.cameras[idx].audioStream = undefined;
		}
	}

	setStream(deviceId: string, stream: MediaStream) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].stream = stream;
		}
	}

	setAudioStream(deviceId: string, stream: MediaStream) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].audioStream = stream;
		}
	}

	removeStream(deviceId: string) {
		const idx = this.cameras.findIndex((c) => c.device_id === deviceId);
		if (idx >= 0) {
			this.cameras[idx].stream = undefined;
			this.cameras[idx].audioStream = undefined;
		}
	}

	updateTelemetry(data: TelemetryData) {
		const idx = this.cameras.findIndex((c) => c.device_id === data.device_id);
		if (idx >= 0) {
			this.cameras[idx].telemetry = data;
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
