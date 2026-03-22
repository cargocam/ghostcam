import type { DataChannelMessage } from '$lib/types.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';

/**
 * Handle a data channel message received on a per-camera WebRTC connection.
 * With per-camera PeerConnections, messages are simpler — mainly telemetry.
 */
export function handleDataChannelMessage(msg: DataChannelMessage) {
	switch (msg.type) {
		case 'telemetry':
			cameraStore.setTelemetry(msg.device_id, {
				device_id: msg.device_id,
				cpu_percent: msg.cpu_percent,
				temp_celsius: msg.temp_celsius,
				memory_mb: msg.memory_mb,
				uptime_secs: msg.uptime_secs,
				gps: msg.gps,
			});
			break;
	}
}
