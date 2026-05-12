import { checkSession, login as authLogin, register as authRegister, logout as authLogout } from '$lib/auth.js';
import { authStore } from '$lib/stores/auth.svelte.js';
import { listCameras, fetchCoverage } from '$lib/signaling.js';
import { cameraStore } from '$lib/stores/cameras.svelte.js';
import { groupStore } from '$lib/stores/groups.svelte.js';
import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
import { alertsStore } from '$lib/stores/alerts.svelte.js';
import { billingStore } from '$lib/stores/billing.svelte.js';
import { settingsStore } from '$lib/stores/settings.svelte.js';

class TransportStore {
	authenticated = $state(false);
	initialized = $state(false);
	connected = $state(false);
	connectedAt = $state<number | null>(null);
	error = $state<string | null>(null);

	get connectionState(): string {
		if (this.connected) return 'connected';
		if (this.authenticated) return 'disconnected';
		return 'unauthenticated';
	}

	private sse: EventSource | null = null;
	private staleCheckInterval: ReturnType<typeof setInterval> | null = null;

	async initialize() {
		try {
			this.authenticated = await checkSession();
			if (!this.authenticated) return;

			// Fetch initial camera list
			const cameras = await listCameras();
			cameraStore.setInitialList(cameras);

			// Fetch coverage for all cameras
			await Promise.all(cameras.map((c) => this.refreshCoverage(c.device_id)));

			// Load persisted events/notifications
			await alertsStore.initialize();

			// Load billing subscription + usage so the storage-cap banner is
			// accurate from app startup, not only after settings is opened.
			// Failures are non-fatal (billing may be disabled on self-hosted).
			billingStore.load().catch(() => { /* non-fatal */ });

			// Connect SSE — delivers initial telemetry on connect, then realtime.
			// Client derives online status from server_ts freshness.
			this.connectSse();

			// Recheck staleness every 10s so cameras that stop reporting
			// transition to offline even without new events.
			this.staleCheckInterval = setInterval(() => {
				cameraStore.recheckOnline();
			}, 10_000);

			this.connected = true;
			this.connectedAt = Date.now();
			this.error = null;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Initialization failed';
		} finally {
			this.initialized = true;
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
						power_mode?: string;
						upload_mode?: string;
						battery_pct?: number;
						segment_upload_p95_ms?: number;
						segment_upload_retries?: number;
						segment_queue_depth?: number;
						live_ws_bytes_per_sec?: number;
						live_ws_dropped_frames?: number;
						gpsd_query_ms?: number;
						event_loop_lag_ms?: number;
						disk_used_pct?: number;
						modem_rat?: string;
						network_recovery_attempts?: number;
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
					power_mode: t.power_mode,
					upload_mode: t.upload_mode,
					battery_pct: t.battery_pct,
					segment_upload_p95_ms: t.segment_upload_p95_ms,
					segment_upload_retries: t.segment_upload_retries,
					segment_queue_depth: t.segment_queue_depth,
					live_ws_bytes_per_sec: t.live_ws_bytes_per_sec,
					live_ws_dropped_frames: t.live_ws_dropped_frames,
					gpsd_query_ms: t.gpsd_query_ms,
					event_loop_lag_ms: t.event_loop_lag_ms,
					disk_used_pct: t.disk_used_pct,
					modem_rat: t.modem_rat,
					network_recovery_attempts: t.network_recovery_attempts,
				}, t.server_ts);
			} catch {
				// Ignore malformed events
			}
		});

		es.addEventListener('motion_detected', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as { event_id?: string; device_id: string; segment_id: string; start_ts: number; end_ts: number };
				if (settingsStore.isMotionAlertsMuted(data.device_id)) return;
				const cam = cameraStore.getCamera(data.device_id);
				const name = cam?.device_name ?? data.device_id;
				alertsStore.addAlert('motion', data.device_id, name, 'Motion detected', data.event_id);
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
				const data = JSON.parse(e.data) as { event_id?: string; device_id?: string; storage_bytes?: number; limit_gb?: number };
				alertsStore.addAlert('storage_capped', data.device_id ?? '', 'Storage', 'Storage limit reached. Uploads paused.', data.event_id);
				// Refresh the billing usage so the persistent banner and
				// settings panel reflect the capped state without waiting
				// for the next manual settings-open. Only the usage numbers
				// change on a storage event — subscription/tiers are
				// unchanged, so we skip the full three-request round-trip.
				billingStore.refreshUsage();
			} catch { /* ignore */ }
		});

		es.addEventListener('events_sync', (e: MessageEvent) => {
			try {
				const data = JSON.parse(e.data) as { action: string; event_id?: string };
				alertsStore.applySync(data.action, data.event_id);
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
				// Server omits `uploaded_to_s3` for historical rows that
				// default TRUE; explicit undefined → true via the store's
				// fallback. Lazy-mode rows explicitly carry false.
				uploaded: s.uploaded_to_s3 ?? true,
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
			authStore.refresh();
			this.authenticated = true;
			await this.initialize();
		}
		return ok;
	}

	async register(email: string, password: string, displayName?: string): Promise<{ ok: boolean; error?: string }> {
		const result = await authRegister(email, password, displayName);
		if (result.ok) {
			authStore.refresh();
			this.authenticated = true;
			await this.initialize();
		}
		return result;
	}

	async logout() {
		await authLogout();
		authStore.refresh();
		this.authenticated = false;
		this.connected = false;
		this.sse?.close();
		this.sse = null;
		if (this.staleCheckInterval) { clearInterval(this.staleCheckInterval); this.staleCheckInterval = null; }
		cameraStore.clear();
		groupStore.clear();
	}

	async switchGroup(groupId: string) {
		groupStore.setActiveGroup(groupId);
	}

	destroy() {
		this.sse?.close();
		this.sse = null;
		if (this.staleCheckInterval) { clearInterval(this.staleCheckInterval); this.staleCheckInterval = null; }
	}
}

export const transportStore = new TransportStore();
