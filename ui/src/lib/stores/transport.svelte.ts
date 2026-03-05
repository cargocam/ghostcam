import { SignalingClient } from '$lib/signaling.js';
import { WebRtcSession } from '$lib/webrtc.js';
import { handleDataChannelMessage } from '$lib/data-channel.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { groupStore } from '$lib/stores/groups.svelte.js';
import { videoStatsStore } from '$lib/stores/videoStats.svelte.js';
import { alertsStore } from '$lib/stores/alerts.svelte.js';

const MAX_RETRIES = 10;
const CONNECTION_TIMEOUT_MS = 10_000;

class TransportStore {
	connected = $state(false);
	error = $state<string | null>(null);
	connectionState = $state<string>('new');
	connectedAt = $state<number | null>(null);
	reconnecting = $state(false);
	reconnectAttempt = $state(0);

	private signaling = new SignalingClient();
	private session: WebRtcSession | null = null;
	private connecting = false;
	private statsInterval: ReturnType<typeof setInterval> | null = null;
	private prevBytes = new Map<string, number>();
	private prevTimestamp = new Map<string, number>();
	private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
	private autoReconnectTimer: ReturnType<typeof setTimeout> | null = null;
	private connectionTimeoutTimer: ReturnType<typeof setTimeout> | null = null;

	async connect(groupId?: string) {
		if (this.connecting || this.session) return;
		this.connecting = true;

		const targetGroup = groupId ?? groupStore.activeGroupId ?? 'default';

		try {
			// Fetch groups first
			const groups = await this.signaling.listGroups();
			groupStore.setGroups(groups);

			// Use first available group if target doesn't exist (skip check for __all__)
			let connectGroup = targetGroup;
			if (connectGroup !== '__all__' && groups.length > 0 && !groups.find(g => g.group_id === connectGroup)) {
				connectGroup = groups[0].group_id;
			}
			groupStore.setActiveGroup(connectGroup);

			// Create WebRTC session
			this.session = new WebRtcSession(this.signaling);

			this.session.onTrack = (deviceId, stream) => {
				cameraStore.setStream(deviceId, stream);
			};

			this.session.onData = (msg) => {
				handleDataChannelMessage(msg);
			};

			this.session.onConnectionStateChange = (state) => {
				this.connectionState = state;
				this.connected = state === 'connected';

				if (state === 'connected') {
					this.clearConnectionTimeout();
					if (this.reconnecting) {
						alertsStore.addAlert('reconnect', '__session__', 'System', 'Connection restored');
						this.reconnecting = false;
						this.reconnectAttempt = 0;
					}
					if (!this.connectedAt) {
						this.connectedAt = Date.now();
					}
				}

				if (state === 'failed' || state === 'disconnected') {
					this.error = `WebRTC ${state}`;
					this.clearConnectionTimeout();
					alertsStore.addAlert('disconnect', '__session__', 'System', `Connection ${state}`);
					this.scheduleAutoReconnect();
				}
			};

			// Start connection timeout
			this.connectionTimeoutTimer = setTimeout(() => {
				this.connectionTimeoutTimer = null;
				if (this.connectionState === 'new' || this.connectionState === 'connecting') {
					this.error = 'Connection timed out';
					this.teardownSession();
					this.scheduleAutoReconnect();
				}
			}, CONNECTION_TIMEOUT_MS);

			await this.session.connect(connectGroup);
			this.connected = true;
			this.connectedAt = Date.now();
			this.error = null;

			this.startStatsPolling();
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Connection failed';
			this.connected = false;
			this.clearConnectionTimeout();
			this.scheduleAutoReconnect();
		} finally {
			this.connecting = false;
		}
	}

	async disconnect() {
		this.clearAutoReconnect();
		this.clearConnectionTimeout();
		if (this.reconnectTimer) {
			clearTimeout(this.reconnectTimer);
			this.reconnectTimer = null;
		}
		this.reconnecting = false;
		this.reconnectAttempt = 0;
		this.stopStatsPolling();
		this.connecting = false;
		if (this.session) {
			await this.session.disconnect();
			this.session = null;
		}
		this.connected = false;
		this.connectedAt = null;
		cameraStore.clear();
	}

	async switchGroup(groupId: string) {
		await this.disconnect();
		await this.connect(groupId);
	}

	/** Tear down and rebuild the session to pick up new/removed tracks. */
	async reconnect() {
		if (this.reconnectTimer) return; // already scheduled
		// Debounce: wait briefly so multiple camera_join events batch into one reconnect
		this.reconnectTimer = setTimeout(async () => {
			this.reconnectTimer = null;
			const groupId = groupStore.activeGroupId;
			if (!groupId) return;
			await this.disconnect();
			await this.connect(groupId);
		}, 1000);
	}

	async refreshGroups() {
		try {
			const groups = await this.signaling.listGroups();
			groupStore.setGroups(groups);
		} catch {}
	}

	private scheduleAutoReconnect() {
		if (this.autoReconnectTimer) return;
		if (this.reconnectAttempt >= MAX_RETRIES) {
			this.error = 'Max reconnection attempts reached';
			this.reconnecting = false;
			return;
		}

		this.reconnecting = true;
		const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempt), 30_000);

		this.autoReconnectTimer = setTimeout(async () => {
			this.autoReconnectTimer = null;
			this.reconnectAttempt++;

			// Tear down session without clearing camera store
			this.teardownSession();

			const groupId = groupStore.activeGroupId;
			if (groupId) {
				await this.connect(groupId);
			}
		}, delay);
	}

	private clearAutoReconnect() {
		if (this.autoReconnectTimer) {
			clearTimeout(this.autoReconnectTimer);
			this.autoReconnectTimer = null;
		}
	}

	private clearConnectionTimeout() {
		if (this.connectionTimeoutTimer) {
			clearTimeout(this.connectionTimeoutTimer);
			this.connectionTimeoutTimer = null;
		}
	}

	/** Tear down session state without clearing camera store or reconnection state. */
	private teardownSession() {
		this.stopStatsPolling();
		this.connecting = false;
		if (this.session) {
			this.session.disconnect();
			this.session = null;
		}
		this.connected = false;
		this.connectedAt = null;
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
		this.prevBytes.clear();
		this.prevTimestamp.clear();
	}

	private async pollStats() {
		if (!this.session) return;
		const statsPromise = this.session.getStats();
		if (!statsPromise) return;

		const report = await statsPromise;
		const trackMap = this.session.getTrackDeviceMap();

		// Build mid → codec lookup from codec reports
		const codecMap = new Map<string, string>();
		report.forEach((stat) => {
			if (stat.type === 'codec') {
				codecMap.set(stat.id, stat.mimeType?.split('/')[1] ?? '');
			}
		});

		// Process inbound-rtp reports for video
		report.forEach((stat) => {
			if (stat.type !== 'inbound-rtp' || stat.kind !== 'video') return;

			const mid = String(stat.mid ?? '');
			const deviceId = trackMap.get(mid);
			if (!deviceId) return;

			const now = stat.timestamp;
			const bytesReceived = stat.bytesReceived ?? 0;

			// Calculate bitrate from delta
			let bitrateKbps = 0;
			const prevB = this.prevBytes.get(deviceId);
			const prevT = this.prevTimestamp.get(deviceId);
			if (prevB !== undefined && prevT !== undefined && now > prevT) {
				const deltaSec = (now - prevT) / 1000;
				bitrateKbps = ((bytesReceived - prevB) * 8) / 1000 / deltaSec;
			}
			this.prevBytes.set(deviceId, bytesReceived);
			this.prevTimestamp.set(deviceId, now);

			// Resolve codec name
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
	}
}

export const transportStore = new TransportStore();
