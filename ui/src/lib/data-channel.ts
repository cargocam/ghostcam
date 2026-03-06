import type { DataChannelMessage } from '$lib/types.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { alertsStore } from '$lib/stores/alerts.svelte.js';
import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';

export function handleDataChannelMessage(msg: DataChannelMessage) {
	switch (msg.type) {
		case 'cameras':
			cameraStore.setCameras(msg.cameras);
			break;

		case 'camera_join':
			cameraStore.addCamera(msg.camera);
			alertsStore.addAlert(
				'reconnect',
				msg.camera.device_id,
				cameraConfigStore.getDisplayName(msg.camera.device_id),
				`Camera joined group ${msg.camera.group_id}`
			);
			// Renegotiation handles track addition — no reconnect needed
			break;

		case 'camera_leave':
			alertsStore.addAlert(
				'disconnect',
				msg.device_id,
				cameraConfigStore.getDisplayName(msg.device_id),
				'Camera disconnected'
			);
			cameraStore.removeCamera(msg.device_id);
			break;

		case 'telemetry':
			cameraStore.updateTelemetry({
				device_id: msg.device_id,
				cpu_percent: msg.cpu_percent,
				temp_celsius: msg.temp_celsius,
				memory_mb: msg.memory_mb,
				uptime_secs: msg.uptime_secs,
				gps: msg.gps,
			});
			break;

		case 'renegotiate':
			// Handled internally by WebRtcSession — should not reach here
			break;
	}
}
