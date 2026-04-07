import { checkSession, login as authLogin, register as authRegister, logout as authLogout } from '$lib/auth.js';
import { listCameras, fetchCoverage } from '$lib/signaling.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { groupStore } from '$lib/stores/groups.svelte.js';
import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
import { alertsStore } from '$lib/stores/alerts.svelte.js';
import { settingsStore } from '$lib/stores/settings.svelte.js';

class TransportStore {
	authenticated = $state(false);
	connected = $state(false);
	connectedAt = $state<number | null>(null);
	error = $state<string | null>(null);

	get connectionState(): string {
		if (this.connected) return 'connected';
		if (this.authenticated) return 'disconnected';
		return 'unauthenticated';
	}

	private sse: EventSource | null = null;
	/** Periodically recompute online flags (cameras go offline when telemetry stops). */
	private onlineRefreshInterval: ReturnType<typeof setInterval> | null = null;

	async initialize() {
		this.authenticated = await checkSession();
		if (!this.authenticated) return;

		try {
			// Fetch initial camera list
			const cameras = await listCameras();
			cameraStore.setInitialList(cameras);

			// Fetch coverage for all cameras
			await Promise.all(cameras.map((c) => this.refreshCoverage(c.device_id)));

			// Connect SSE for realtime telemetry
			this.connectSse();

			// Refresh online flags every 5s (detects cameras that stopped sending telemetry)
			this.onlineRefreshInterval = setInterval(() => {
				cameraStore.refreshOnlineStatus();
			}, 5_000);

			// Coverage updates arrive in realtime via SSE "coverage" events.
			// No polling needed.

			this.connected = true;
			this.connectedAt = Date.now();
			this.error = null;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Initialization failed';
		}
	}

	/** Connect to /events SSE endpoint for realtime telemetry. */
	private connectSse() {
		this.sse?.close();

		const es = new EventSource('/events', { withCredentials: true });

		es.addEventListener('telemetry', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as {
					device_id: string;
					telemetry: {
						ts: number;
						server_ts: number;
						sig?: number;
						temp?: number;
						fps?: number;
						kbps?: number;
						cpu?: number;
						mem?: number;
						uptime?: number;
						lat?: number;
						lon?: number;
						alt?: number;
						gps_fix?: number;
					};
				};
				const t = data.telemetry;
				cameraStore.setTelemetry(data.device_id, {
					cpu_percent: t.cpu,
					temp_celsius: t.temp,
					memory_mb: t.mem,
					uptime_secs: t.uptime,
					gps: t.lat != null && t.lon != null
						? { latitude: t.lat, longitude: t.lon, alt: t.alt }
						: undefined,
				});
			} catch {
				// Ignore malformed events
			}
		});

		es.addEventListener('motion_detected', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as { device_id: string; segment_id: string; start_ts: number; end_ts: number };
				if (settingsStore.isMotionAlertsMuted(data.device_id)) return;
				const cam = cameraStore.getCamera(data.device_id);
				const name = cam?.device_name ?? data.device_id;
				alertsStore.addAlert('motion', data.device_id, name, 'Motion detected');
			} catch { /* ignore */ }
		});

		es.addEventListener('coverage', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as {
					device_id: string;
					segments: { id: string; start_ms: number; end_ms: number; has_motion: boolean }[];
				};
				// Append new segments to the scrubber's coverage
				const existing = scrubberStore.cameraCoverage.get(data.device_id) ?? [];
				const newSegs = data.segments.map((s) => ({
					start: s.start_ms / 1000,
					end: s.end_ms / 1000,
					hasMotion: s.has_motion,
				}));
				scrubberStore.setCameraCoverage(data.device_id, [...existing, ...newSegs]);
				this.updateAvailableWindow();
			} catch { /* ignore */ }
		});

		es.addEventListener('storage_capped', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as { device_id?: string; storage_bytes?: number; limit_gb?: number };
				alertsStore.addAlert('storage_capped', data.device_id ?? '', 'Storage', 'Storage limit reached. Uploads paused.');
			} catch { /* ignore */ }
		});

		es.onopen = () => {
			this.connected = true;
			this.connectedAt = Date.now();
		};

		es.onerror = () => {
			this.connected = false;
			// EventSource auto-reconnects
		};

		this.sse = es;
	}

	/** Fetch coverage for one camera and update the scrubber. */
	async refreshCoverage(deviceId: string): Promise<void> {
		try {
			const coverage = await fetchCoverage(deviceId);
			const segments = coverage.segments.map((s) => ({
				start: s.start_ms / 1000,
				end: s.end_ms / 1000,
				hasMotion: s.has_motion ?? false,
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

	async login(email: string, password: string): Promise<boolean> {
		const ok = await authLogin(email, password);
		if (ok) {
			this.authenticated = true;
			await this.initialize();
		}
		return ok;
	}

	async register(email: string, password: string, displayName?: string): Promise<{ ok: boolean; error?: string }> {
		const result = await authRegister(email, password, displayName);
		if (result.ok) {
			this.authenticated = true;
			await this.initialize();
		}
		return result;
	}

	async logout() {
		await authLogout();
		this.authenticated = false;
		this.connected = false;
		this.sse?.close();
		this.sse = null;
		if (this.onlineRefreshInterval) {
			clearInterval(this.onlineRefreshInterval);
			this.onlineRefreshInterval = null;
		}
		cameraStore.clear();
		groupStore.clear();
	}

	async switchGroup(groupId: string) {
		groupStore.setActiveGroup(groupId);
	}

	destroy() {
		this.sse?.close();
		this.sse = null;
		if (this.onlineRefreshInterval) {
			clearInterval(this.onlineRefreshInterval);
			this.onlineRefreshInterval = null;
		}
	}
}

export const transportStore = new TransportStore();
