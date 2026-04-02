export interface CameraInfo {
	device_id: string;
	display_name: string;
	group_id?: string;
	capabilities?: string[];
	resolution: string;
	recording_mode: string;
}

export interface GpsData {
	latitude: number;
	longitude: number;
	alt?: number;
}

export interface TelemetryData {
	device_id?: string;
	cpu_percent?: number;
	temp_celsius?: number;
	memory_mb?: number;
	uptime_secs?: number;
	gps?: GpsData;
}

export interface GroupInfo {
	group_id: string;
	camera_count: number;
}

export type GridLayout = 'auto' | '1+5';
export type ViewMode = 'live' | 'map' | 'dashboard' | 'camera';
export type MarkerMode = 'dot' | 'detailed' | 'pip';

export interface SubscriptionInfo {
	tier: string;
	status: string;
	billing_enabled: boolean;
	current_period_end?: number;
	grace_expires_at?: number;
	stripe_public_key?: string;
	stripe_pricing_table_id?: string;
}

export interface TierInfo {
	id: string;
	name: string;
	camera_limit: number | null;
	storage_gb: number | null;
	bandwidth_gb: number | null;
}

export interface UsageInfo {
	cameras_count: number;
	storage_bytes: number;
	bandwidth_bytes: number;
	camera_limit: number | null;
	storage_limit_gb: number | null;
	bandwidth_limit_gb: number | null;
}
