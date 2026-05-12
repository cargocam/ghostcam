import type { CameraResponse } from '$lib/api-types';
import type { TelemetryData } from '$lib/types.js';

/**
 * How long (ms) since the last server_ts before considering a camera offline.
 * The camera posts telemetry every 10s; 30s gives ~2 missed polls of grace.
 */
const ONLINE_STALE_MS = 30_000;

export interface CameraState {
	device_id: string;
	device_name: string;
	/** Derived from server_ts freshness of last telemetry. */
	online: boolean;
	telemetry: TelemetryData | null;
	/** server_ts (epoch ms) from the most recent telemetry for this camera. */
	lastServerTs: number | null;
	resolution: string;
	recording_mode: string;
	fw_version: string;
	/** Manually-set power mode (live | standby | sleep). The currently
	 * effective mode may differ if a schedule or battery rule overrides;
	 * effectivePowerMode below carries that. */
	power_mode: string;
	upload_mode: string;
	/** JSON-encoded list of {start, end, days, power_mode, upload_mode}.
	 * undefined = no schedule set. */
	schedule: string | undefined;
	/** JSON-encoded list of {threshold_pct, power_mode, upload_mode}. */
	battery_rules: string | undefined;
	/** Currently-effective values from the latest telemetry datagram.
	 * Distinct from power_mode/upload_mode so the UI can show
	 * "manually live, schedule overriding to sleep right now." */
	effectivePowerMode: string | undefined;
	effectiveUploadMode: string | undefined;
	/** 0–100, only set when a battery-sensing HAT is reporting (GH #73). */
	battery_pct: number | undefined;
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

	setInitialList(list: CameraResponse[]) {
		this.cameras = list.map((c) => {
			const t = c.telemetry;
			const initialTelemetry: TelemetryData | null = t ? {
				cpu_percent: t.cpu,
				temp_celsius: t.temp,
				memory_mb: t.mem,
				uptime_secs: t.uptime,
				gps: t.lat != null && t.lon != null
					? { latitude: t.lat, longitude: t.lon, alt: t.alt ?? undefined }
					: undefined,
				// Pass-through health metrics (camera-side fields added
				// during the 2026-05-12 perf series — PRs #80, #84).
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
			} : null;
			return {
				device_id: c.device_id,
				device_name: c.display_name,
				online: false, // SSE telemetry burst will update this immediately
				telemetry: initialTelemetry,
				lastServerTs: null,
				resolution: c.resolution ?? '720p',
				recording_mode: c.recording_mode ?? 'never',
				fw_version: c.fw_version ?? '',
				power_mode: c.power_mode ?? 'live',
				upload_mode: c.upload_mode ?? 'proactive',
				schedule: c.schedule != null ? JSON.stringify(c.schedule) : undefined,
				battery_rules:
					c.battery_rules != null ? JSON.stringify(c.battery_rules) : undefined,
				effectivePowerMode: t?.power_mode ?? undefined,
				effectiveUploadMode: t?.upload_mode ?? undefined,
				battery_pct: t?.battery_pct ?? undefined,
			};
		});
	}

	/**
	 * Update telemetry for a camera. Derives online status from server_ts:
	 * if the server received this telemetry recently, the camera is online.
	 */
	setTelemetry(deviceId: string, data: TelemetryData, serverTs: number) {
		const cam = this.cameras.find((c) => c.device_id === deviceId);
		if (!cam) return;
		cam.telemetry = data;
		cam.lastServerTs = serverTs;
		cam.online = Date.now() - serverTs < ONLINE_STALE_MS;
		if (data.power_mode != null) cam.effectivePowerMode = data.power_mode;
		if (data.upload_mode != null) cam.effectiveUploadMode = data.upload_mode;
		if (data.battery_pct != null) cam.battery_pct = data.battery_pct;
	}

	/**
	 * Re-evaluate online status for all cameras based on server_ts staleness.
	 * Call periodically (e.g. every 10s) so cameras that stop reporting
	 * transition to offline even without new telemetry events.
	 */
	recheckOnline() {
		const now = Date.now();
		for (const cam of this.cameras) {
			cam.online = cam.lastServerTs != null && now - cam.lastServerTs < ONLINE_STALE_MS;
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
