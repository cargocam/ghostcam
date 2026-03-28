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
