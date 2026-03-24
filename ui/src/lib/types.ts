export interface CameraInfo {
	device_id: string;
	display_name: string;
	group_id?: string;
	capabilities?: string[];
	online: boolean;
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
