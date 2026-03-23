import { CameraConnection, type ClientMode } from '$lib/webrtc.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { videoStatsStore } from '$lib/stores/videoStats.svelte.js';

const RECONNECT_DELAY_MS = 3000;

/**
 * Manages all per-camera WebRTC connections.
 * Creates/destroys CameraConnections in response to SSE events and user actions.
 */
export class ConnectionManager {
	private connections = new Map<string, CameraConnection>();
	private reconnectTimers = new Map<string, ReturnType<typeof setTimeout>>();
	private statsInterval: ReturnType<typeof setInterval> | null = null;
	private prevBytes = new Map<string, number>();
	private prevTimestamp = new Map<string, number>();

	constructor() {
		this.startStatsPolling();
	}

	async connectCamera(deviceId: string): Promise<void> {
		// Don't double-connect
		if (this.connections.has(deviceId)) return;

		const conn = new CameraConnection(deviceId, {
			onVideoTrack: (stream) => {
				cameraStore.setVideoStream(deviceId, stream);
			},
			onAudioTrack: (stream) => {
				cameraStore.setAudioStream(deviceId, stream);
			},
			onTelemetry: (data) => {
				cameraStore.setTelemetry(deviceId, data);
			},
			onDisconnect: () => {
				this.handleDisconnect(deviceId);
			},
		});

		this.connections.set(deviceId, conn);

		try {
			await conn.connect();
		} catch (e) {
			console.error(`Failed to connect to ${deviceId}:`, e);
			this.connections.delete(deviceId);
			this.scheduleReconnect(deviceId);
		}
	}

	async disconnectCamera(deviceId: string): Promise<void> {
		this.clearReconnectTimer(deviceId);
		const conn = this.connections.get(deviceId);
		if (conn) {
			await conn.disconnect();
			this.connections.delete(deviceId);
		}
		cameraStore.clearStreams(deviceId);
		videoStatsStore.remove(deviceId);
		this.prevBytes.delete(deviceId);
		this.prevTimestamp.delete(deviceId);
	}

	private handleDisconnect(deviceId: string) {
		const conn = this.connections.get(deviceId);
		this.connections.delete(deviceId);
		cameraStore.clearStreams(deviceId);
		// Tear down the session on the server so the EgressHandle is cleaned up.
		// Don't await — fire and forget, reconnect can proceed in parallel.
		conn?.disconnect().catch(() => {});
		this.scheduleReconnect(deviceId);
	}

	private scheduleReconnect(deviceId: string) {
		// Only reconnect if camera is still online
		if (!cameraStore.isOnline(deviceId)) return;

		this.clearReconnectTimer(deviceId);
		const timer = setTimeout(() => {
			this.reconnectTimers.delete(deviceId);
			this.connectCamera(deviceId);
		}, RECONNECT_DELAY_MS);
		this.reconnectTimers.set(deviceId, timer);
	}

	private clearReconnectTimer(deviceId: string) {
		const timer = this.reconnectTimers.get(deviceId);
		if (timer) {
			clearTimeout(timer);
			this.reconnectTimers.delete(deviceId);
		}
	}

	async disconnectAll(): Promise<void> {
		this.stopStatsPolling();
		for (const timer of this.reconnectTimers.values()) {
			clearTimeout(timer);
		}
		this.reconnectTimers.clear();

		for (const [id, conn] of this.connections) {
			await conn.disconnect().catch(() => {});
			cameraStore.clearStreams(id);
		}
		this.connections.clear();
		this.prevBytes.clear();
		this.prevTimestamp.clear();
	}

	private startStatsPolling() {
		this.stopStatsPolling();
		this.statsInterval = setInterval(() => this.pollStats(), 2000);
	}

	private stopStatsPolling() {
		if (this.statsInterval) {
			clearInterval(this.statsInterval);
			this.statsInterval = null;
		}
	}

	private pollIndex = 0;
	private static readonly STATS_BATCH_SIZE = 6;

	private async pollStats() {
		// Stagger: poll a subset of connections each interval to spread CPU load
		const entries = [...this.connections.entries()];
		if (entries.length === 0) return;

		const start = this.pollIndex % entries.length;
		const batch = [];
		for (let i = 0; i < ConnectionManager.STATS_BATCH_SIZE && i < entries.length; i++) {
			batch.push(entries[(start + i) % entries.length]);
		}
		this.pollIndex = (start + ConnectionManager.STATS_BATCH_SIZE) % Math.max(entries.length, 1);

		for (const [deviceId, conn] of batch) {
			const statsPromise = conn.getStats();
			if (!statsPromise) continue;

			try {
				const report = await statsPromise;

				// Build codec lookup
				const codecMap = new Map<string, string>();
				report.forEach((stat) => {
					if (stat.type === 'codec') {
						codecMap.set(stat.id, stat.mimeType?.split('/')[1] ?? '');
					}
				});

				// Process inbound-rtp for video
				report.forEach((stat) => {
					if (stat.type !== 'inbound-rtp' || stat.kind !== 'video') return;

					const now = stat.timestamp;
					const bytesReceived = stat.bytesReceived ?? 0;

					let bitrateKbps = 0;
					const prevB = this.prevBytes.get(deviceId);
					const prevT = this.prevTimestamp.get(deviceId);
					if (prevB !== undefined && prevT !== undefined && now > prevT) {
						const deltaSec = (now - prevT) / 1000;
						bitrateKbps = ((bytesReceived - prevB) * 8) / 1000 / deltaSec;
					}
					this.prevBytes.set(deviceId, bytesReceived);
					this.prevTimestamp.set(deviceId, now);

					let codec = '';
					if (stat.codecId) {
						codec = codecMap.get(stat.codecId) ?? '';
					}

					videoStatsStore.update(deviceId, {
						width: stat.frameWidth ?? 0,
						height: stat.frameHeight ?? 0,
						codec,
						framesDecoded: stat.framesDecoded ?? 0,
						framesDropped: stat.framesDropped ?? 0,
						bitrateKbps,
					});
				});
			} catch {}
		}
	}

	/** Send client_mode to a specific camera. */
	sendClientMode(deviceId: string, mode: ClientMode) {
		this.connections.get(deviceId)?.sendClientMode(mode);
	}

	/** Broadcast client_mode to all connected cameras. */
	broadcastClientMode(mode: ClientMode) {
		for (const conn of this.connections.values()) {
			conn.sendClientMode(mode);
		}
	}

	get connectionCount(): number {
		return this.connections.size;
	}
}
