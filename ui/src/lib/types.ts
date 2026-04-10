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
export type ViewMode = 'live' | 'map' | 'dashboard' | 'camera';
export type MarkerMode = 'dot' | 'detailed' | 'pip';
