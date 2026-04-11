import type {
	CameraResponse,
	CheckoutResponse,
	CoverageResponse,
	EventEntry,
	ListEventsResponse,
	ListTiersResponse,
	PortalResponse,
	PrepareClipResponse,
	QRRequest,
	QRResponse,
	SubscriptionResponse,
	TelemetryRangeResponse,
	UnreadCountResponse,
	UsageResponse,
} from '$lib/api-types';

const API_BASE = '/api/v1';

function headers(): HeadersInit {
	return {
		'Content-Type': 'application/json',
	};
}

// --- Cameras ---

export async function listCameras(): Promise<CameraResponse[]> {
	const res = await fetch(`${API_BASE}/cameras`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`listCameras failed: ${res.status}`);
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

export async function getSubscription(): Promise<SubscriptionResponse> {
	const res = await fetch(`${API_BASE}/billing/subscription`, { credentials: 'include' });
	if (!res.ok) throw new Error(`getSubscription failed: ${res.status}`);
	return res.json();
}

export async function listTiers(): Promise<ListTiersResponse> {
	const res = await fetch(`${API_BASE}/billing/tiers`, { credentials: 'include' });
	if (!res.ok) throw new Error(`listTiers failed: ${res.status}`);
	return res.json();
}

// Force a Stripe-side refresh of the tier cache, then return the fresh
// list. Used by the billing UI's Retry button so that users who just
// tagged product metadata in the Stripe dashboard see the change
// immediately instead of waiting for the next webhook or hourly tick.
export async function refreshTiers(): Promise<ListTiersResponse> {
	const res = await fetch(`${API_BASE}/billing/tiers/refresh`, {
		method: 'POST',
		headers: headers(),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`refreshTiers failed: ${res.status}`);
	return res.json();
}

export async function getUsage(): Promise<UsageResponse> {
	const res = await fetch(`${API_BASE}/billing/usage`, { credentials: 'include' });
	if (!res.ok) throw new Error(`getUsage failed: ${res.status}`);
	return res.json();
}

export async function createPortal(returnUrl: string): Promise<PortalResponse> {
	const res = await fetch(`${API_BASE}/billing/portal`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ return_url: returnUrl }),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`createPortal failed: ${res.status}`);
	return res.json();
}

export async function createCheckout(
	tier: string,
	successUrl: string,
	cancelUrl: string,
): Promise<CheckoutResponse> {
	const res = await fetch(`${API_BASE}/billing/checkout`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ tier, success_url: successUrl, cancel_url: cancelUrl }),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`createCheckout failed: ${res.status}`);
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

export async function deleteCamera(deviceId: string): Promise<void> {
	const res = await fetch(`${API_BASE}/cameras/${encodeURIComponent(deviceId)}`, {
		method: 'DELETE',
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`deleteCamera failed: ${res.status}`);
}

// --- Enrollment QR ---

export async function generateEnrollmentQr(opts?: QRRequest): Promise<QRResponse> {
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
): Promise<TelemetryRangeResponse> {
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

export async function fetchEvents(count = 50, before?: string): Promise<EventEntry[]> {
	const params = new URLSearchParams({ count: String(count) });
	if (before) params.set('before', before);
	const res = await fetch(`${API_BASE}/events?${params}`, { credentials: 'include' });
	if (!res.ok) throw new Error(`fetchEvents failed: ${res.status}`);
	const data: ListEventsResponse = await res.json();
	return data.events ?? [];
}

export async function fetchUnreadCount(): Promise<number> {
	const res = await fetch(`${API_BASE}/events/unread`, { credentials: 'include' });
	if (!res.ok) return 0;
	const data: UnreadCountResponse = await res.json();
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

// --- Clips / Export ---

export async function prepareClip(
	deviceId: string,
	fromMs: number,
	toMs: number,
): Promise<PrepareClipResponse> {
	const res = await fetch(`${API_BASE}/clips/prepare`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify({ device_id: deviceId, from_ms: fromMs, to_ms: toMs }),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`prepareClip failed: ${res.status}`);
	return res.json();
}

export async function exportTelemetry(
	deviceId: string,
	fromMs: number,
	toMs: number,
	format: 'csv' | 'json' = 'json',
): Promise<Blob> {
	const params = new URLSearchParams({ from: String(fromMs), to: String(toMs), format });
	const res = await fetch(
		`${API_BASE}/telemetry/${encodeURIComponent(deviceId)}/export?${params}`,
		{ credentials: 'include' },
	);
	if (!res.ok) throw new Error(`exportTelemetry failed: ${res.status}`);
	return res.blob();
}
