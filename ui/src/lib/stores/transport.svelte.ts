import { checkSession, login as authLogin, logout as authLogout } from '$lib/auth.js';
import { listCameras, fetchCoverage } from '$lib/signaling.js';
import { connectSse, type SseEvent } from '$lib/sse.js';
import { ConnectionManager } from '$lib/connection-manager.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { groupStore } from '$lib/stores/groups.svelte.js';
import { alertsStore } from '$lib/stores/alerts.svelte.js';
import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
import { scrubberStore } from '$lib/stores/scrubber.svelte.js';

class TransportStore {
	authenticated = $state(false);
	connected = $state(false);
	connectedAt = $state<number | null>(null);
	error = $state<string | null>(null);
	reconnecting = $state(false);
	reconnectAttempt = $state(0);

	get connectionState(): string {
		if (this.connected) return 'connected';
		if (this.reconnecting) return 'reconnecting';
		if (this.authenticated) return 'disconnected';
		return 'unauthenticated';
	}

	private sse: EventSource | null = null;
	private connManager: ConnectionManager | null = null;

	async initialize() {
		this.authenticated = await checkSession();
		if (!this.authenticated) return;

		try {
			// Fetch initial camera list
			const cameras = await listCameras();
			cameraStore.setInitialList(cameras);

			// Start SSE
			this.sse = connectSse(
				(event) => this.handleSseEvent(event),
				() => {
					this.connected = true;
					this.connectedAt = Date.now();
				},
				() => {},
			);

			// Create connection manager
			this.connManager = new ConnectionManager();

			// Connect to all online cameras and fetch coverage for all cameras in parallel
			await Promise.all([
				...cameras.filter((c) => c.online).map((c) => this.connManager!.connectCamera(c.device_id)),
				...cameras.map((c) => this.refreshCoverage(c.device_id)),
			]);

			this.error = null;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Initialization failed';
		}
	}

	/** Fetch coverage for one camera and update the scrubber. */
	async refreshCoverage(deviceId: string): Promise<void> {
		try {
			const coverage = await fetchCoverage(deviceId);
			const segments = coverage.segments.map((s) => ({
				start: s.start_ms / 1000,
				end: s.end_ms / 1000,
			}));
			scrubberStore.setCameraCoverage(deviceId, segments);
			this.updateAvailableWindow();
		} catch {
			// Coverage unavailable for this camera — not fatal
		}
	}

	/** Recompute the scrubber's available window from all camera coverage. */
	private updateAvailableWindow(): void {
		let minStart = Infinity;
		for (const [, segs] of scrubberStore.cameraCoverage) {
			for (const seg of segs) {
				if (seg.start < minStart) minStart = seg.start;
			}
		}
		if (minStart < Infinity) {
			scrubberStore.setAvailableWindow({ start: minStart, end: Date.now() / 1000 });
		}
	}

	private handleSseEvent(event: SseEvent) {
		switch (event.type) {
			case 'camera_online': {
				cameraStore.setOnline(event.device_id, true);
				this.connManager?.connectCamera(event.device_id);
				this.refreshCoverage(event.device_id);
				const cam = cameraStore.getCamera(event.device_id);
				alertsStore.addAlert(
					'reconnect',
					event.device_id,
					cameraConfigStore.getDisplayName(event.device_id, cam?.device_name),
					'Camera came online',
				);
				break;
			}

			case 'camera_offline': {
				const offCam = cameraStore.getCamera(event.device_id);
				cameraStore.setOnline(event.device_id, false);
				this.connManager?.disconnectCamera(event.device_id);
				// Refresh coverage after offline — camera may have pushed a final manifest
				setTimeout(() => this.refreshCoverage(event.device_id), 1000);
				alertsStore.addAlert(
					'disconnect',
					event.device_id,
					cameraConfigStore.getDisplayName(event.device_id, offCam?.device_name),
					'Camera went offline',
				);
				break;
			}
		}
	}

	async login(email: string, password: string): Promise<boolean> {
		const ok = await authLogin(email, password);
		if (ok) {
			this.authenticated = true;
			await this.initialize();
		}
		return ok;
	}

	async logout() {
		await authLogout();
		this.authenticated = false;
		this.sse?.close();
		this.sse = null;
		await this.connManager?.disconnectAll();
		this.connManager = null;
		cameraStore.clear();
		groupStore.clear();
	}

	async switchGroup(groupId: string) {
		groupStore.setActiveGroup(groupId);
		// In per-camera model, group switching filters the view
		// but doesn't tear down connections
	}

	/** Broadcast client_mode to all connected cameras. */
	broadcastClientMode(mode: import('$lib/webrtc.js').ClientMode) {
		this.connManager?.broadcastClientMode(mode);
	}

	/** Send client_mode to one camera. */
	sendClientMode(deviceId: string, mode: import('$lib/webrtc.js').ClientMode) {
		this.connManager?.sendClientMode(deviceId, mode);
	}

	destroy() {
		this.sse?.close();
		this.connManager?.disconnectAll();
	}
}

export const transportStore = new TransportStore();
