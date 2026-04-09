import type { GroupInfo, CameraInfo, SubscriptionInfo, UsageInfo } from '$lib/types.js';

interface CoverageSegment {
	id: string;
	start_ms: number;
	end_ms: number;
	has_motion?: boolean;
}

interface CoverageResponse {
	online: boolean;
	segments: CoverageSegment[];
}

const API_BASE = '/api/v1';

export interface TelemetryEntry {
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
}

interface TelemetryPage {
	entries: TelemetryEntry[];
	next_cursor?: string;
}

function headers(): HeadersInit {
	return {
		'Content-Type': 'application/json',
	};
}

export async function listCameras(): Promise<CameraInfo[]> {
	const res = await fetch(`${API_BASE}/cameras`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`listCameras failed: ${res.status}`);
	return res.json();
}

export async function listGroups(): Promise<GroupInfo[]> {
	const res = await fetch(`${API_BASE}/groups`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`listGroups failed: ${res.status}`);
	return res.json();
}

export async function fetchCoverage(deviceId: string): Promise<CoverageResponse> {
	const res = await fetch(`/hls/${encodeURIComponent(deviceId)}/coverage`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`fetchCoverage failed: ${res.status}`);
	return res.json();
}

// --- Billing ---

export async function getSubscription(): Promise<SubscriptionInfo> {
	const res = await fetch(`${API_BASE}/billing/subscription`, { credentials: 'include' });
	if (!res.ok) throw new Error(`getSubscription failed: ${res.status}`);
	return res.json();
}

export async function getUsage(): Promise<UsageInfo> {
	const res = await fetch(`${API_BASE}/billing/usage`, { credentials: 'include' });
	if (!res.ok) throw new Error(`getUsage failed: ${res.status}`);
	return res.json();
}

export async function createPortal(returnUrl: string): Promise<{ url: string }> {
	const res = await fetch(`${API_BASE}/billing/portal`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ return_url: returnUrl }),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`createPortal failed: ${res.status}`);
	return res.json();
}

// --- Camera Settings ---

export async function updateCameraSettings(
	deviceId: string,
	update: { display_name?: string; notes?: string; resolution?: string; recording_mode?: string },
): Promise<void> {
	const res = await fetch(`${API_BASE}/cameras/${encodeURIComponent(deviceId)}`, {
		method: 'PATCH',
		headers: headers(),
		body: JSON.stringify(update),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`updateCameraSettings failed: ${res.status}`);
}

// --- Camera Delete ---

export async function deleteCamera(deviceId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/cameras/${encodeURIComponent(deviceId)}`, {
		method: 'DELETE',
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`deleteCamera failed: ${res.status}`);
}

// --- Billing Checkout ---

export async function createCheckout(tier: string, successUrl: string, cancelUrl: string): Promise<{ url: string }> {
	const res = await fetch(`${API_BASE}/billing/checkout`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ tier, success_url: successUrl, cancel_url: cancelUrl }),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`createCheckout failed: ${res.status}`);
	return res.json();
}

// --- Enrollment QR ---

export interface EnrollQrRequest {
	wifi_ssid?: string;
	wifi_password?: string;
	ttl_hours?: number;
}

export interface EnrollQrResponse {
	payload: string;
	token: string;
	expires_at: number;
}

/**
 * Create a provision token and return the QR payload for client-side rendering.
 */
export async function generateEnrollmentQr(opts?: EnrollQrRequest): Promise<EnrollQrResponse> {
	const body = opts ? JSON.stringify(opts) : '{}';
	const res = await fetch(`${API_BASE}/cameras/enroll/qr`, {
		method: 'POST',
		headers: headers(),
		body,
		credentials: 'include',
	});
	if (!res.ok) {
		const text = await res.text();
		throw new Error(`generateEnrollmentQr failed: ${res.status} ${text}`);
	}
	return res.json();
}

// --- Telemetry ---

export async function fetchTelemetryRange(
	deviceId: string,
	fromMs: number,
	toMs: number,
	limit = 600,
): Promise<TelemetryPage> {
	const from = Math.max(0, Math.floor(fromMs));
	const to = Math.max(from, Math.floor(toMs));
	const params = new URLSearchParams({
		from: String(from),
		to: String(to),
		limit: String(limit),
	});
	const res = await fetch(
		`${API_BASE}/telemetry/${encodeURIComponent(deviceId)}?${params.toString()}`,
		{ credentials: 'include' },
	);
	if (!res.ok) throw new Error(`fetchTelemetryRange failed: ${res.status}`);
	return res.json();
}

// --- Events / Notifications ---

export interface ServerEvent {
	id: string;
	type: string;
	device_id: string;
	data: string; // JSON string
	created_at: number;
	read: boolean;
	dismissed: boolean;
}

export async function fetchEvents(count = 50, before?: string): Promise<ServerEvent[]> {
	const params = new URLSearchParams({ count: String(count) });
	if (before) params.set('before', before);
	const res = await fetch(`${API_BASE}/events?${params}`, { credentials: 'include' });
	if (!res.ok) throw new Error(`fetchEvents failed: ${res.status}`);
	const data = await res.json();
	return data.events ?? [];
}

export async function fetchUnreadCount(): Promise<number> {
	const res = await fetch(`${API_BASE}/events/unread`, { credentials: 'include' });
	if (!res.ok) return 0;
	const data = await res.json();
	return data.count ?? 0;
}

export async function markEventRead(eventId: string): Promise<void> {
	await fetch(`${API_BASE}/events/${encodeURIComponent(eventId)}/read`, {
		method: 'PATCH',
		credentials: 'include',
	});
}

export async function markAllEventsRead(): Promise<void> {
	await fetch(`${API_BASE}/events/read-all`, {
		method: 'POST',
		credentials: 'include',
	});
}

export async function dismissEvent(eventId: string): Promise<void> {
	await fetch(`${API_BASE}/events/${encodeURIComponent(eventId)}`, {
		method: 'DELETE',
		credentials: 'include',
	});
}
