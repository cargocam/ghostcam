import type { GroupInfo, CameraInfo, SubscriptionInfo, UsageInfo } from '$lib/types.js';

interface CoverageSegment {
	id: string;
	start_ms: number;
	end_ms: number;
}

interface CoverageResponse {
	online: boolean;
	segments: CoverageSegment[];
}

const API_BASE = '/api/v1';

interface WatchResponse {
	session_id: string;
	sdp_answer: string;
}

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

export async function watchCamera(
	deviceId: string,
	sdpOffer: string,
): Promise<WatchResponse> {
	const res = await fetch(`${API_BASE}/watch`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ device_id: deviceId, sdp_offer: sdpOffer }),
		credentials: 'include',
	});
	if (!res.ok) {
		throw new Error(`Watch failed: ${res.status} ${await res.text()}`);
	}
	return res.json();
}

export async function unwatchCamera(sessionId: string): Promise<void> {
	await fetch(`${API_BASE}/session/${sessionId}`, {
		method: 'DELETE',
		headers: headers(),
		credentials: 'include',
	});
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

// --- Cache Status ---

export interface CacheStatusSegment {
	id: string;
	start_ms: number;
	end_ms: number;
	state: 'cached' | 'uploading' | 'available';
}

interface CacheStatusResponse {
	segments: CacheStatusSegment[];
}

export async function fetchCacheStatus(deviceId: string): Promise<CacheStatusResponse> {
	const res = await fetch(`/hls/${encodeURIComponent(deviceId)}/cache-status`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`fetchCacheStatus failed: ${res.status}`);
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

// --- HLS Prefetch ---

/** Hint the server to pre-fetch segments covering a time range (best-effort, fire-and-forget). */
export async function sendPrefetchHint(
	deviceId: string,
	fromMs: number,
	toMs: number,
): Promise<void> {
	try {
		await fetch(`/hls/${encodeURIComponent(deviceId)}/prefetch`, {
			method: 'POST',
			headers: headers(),
			body: JSON.stringify({ from_ms: Math.floor(fromMs), to_ms: Math.floor(toMs) }),
			credentials: 'include',
		});
	} catch {
		// Best-effort — silently ignore failures
	}
}

// --- Enrollment QR ---

export interface EnrollQrRequest {
	wifi_ssid?: string;
	wifi_password?: string;
	ttl_hours?: number;
}

/**
 * Generate a QR code SVG for camera enrollment.
 * Returns the raw SVG string.
 */
export async function generateEnrollmentQr(opts?: EnrollQrRequest): Promise<string> {
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
	return res.text();
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
