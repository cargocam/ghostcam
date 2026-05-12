// Client-only UI types. Anything that crosses the wire with the server
// belongs in $lib/api-types (generated from Go). If you find yourself adding
// a `foo_response` / `foo_request` interface here, you're drifting — put the
// Go struct in server/apitypes/ and run `make generate-types` instead.

export interface GpsData {
	latitude: number;
	longitude: number;
	alt?: number;
}

/**
 * Reshaped telemetry used by the camera store's internal state. The raw wire
 * shape is `TelemetryEntry` from `$lib/api-types`; this type renames fields
 * to match how Svelte components consume them (e.g. `cpu_percent` instead of
 * `cpu`). Keep it UI-only.
 */
export interface TelemetryData {
	device_id?: string;
	cpu_percent?: number;
	temp_celsius?: number;
	memory_mb?: number;
	uptime_secs?: number;
	gps?: GpsData;
	/** Currently-effective power mode after schedule + battery rules. */
	power_mode?: string;
	upload_mode?: string;
	/** 0–100. Only set when a battery-sensing HAT (GH #73) is wired up. */
	battery_pct?: number;
	/** Health metrics surfaced from the camera's TelemetryDatagram.
	 * Names mirror the wire format intentionally — these are pass-
	 * through for the UI; renaming would force two places to stay in
	 * sync for no gain. */
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
}

/**
 * Client-side grouping of cameras. Not persisted on the server — the backend
 * has no /groups endpoint. If a future release adds server groups, move this
 * to a Go struct and generate it.
 */
export interface GroupInfo {
	group_id: string;
	camera_count: number;
}

export type GridLayout = 'auto' | '1+5';
export type ViewMode = 'live' | 'map' | 'dashboard' | 'camera' | 'admin';
export type MarkerMode = 'dot' | 'detailed' | 'pip';
