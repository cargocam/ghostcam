import type {
	AdminBillingTierSubscribersResponse,
	AdminCreateUserRequest,
	AdminCreateUserResponse,
	AdminListBillingTiersResponse,
	AdminListCamerasResponse,
	AdminListUsersResponse,
	AdminReassignCameraConflictResponse,
	AdminReassignCameraRequest,
	AdminRepriceBillingTierResponse,
	AdminResetPasswordResponse,
	AdminUpdateUserRequest,
	CameraResponse,
	CheckoutResponse,
	CoverageResponse,
	DeleteFootageResponse,
	EventEntry,
	ListEventsResponse,
	ListTiersResponse,
	PiImagesResponse,
	PortalResponse,
	PrepareClipResponse,
	QRRequest,
	QRResponse,
	SubscriptionResponse,
	TelemetryRangeResponse,
	UnreadCountResponse,
	UsageResponse,
} from '$lib/api-types';

// AdminUpdateBillingTier / AdminCreateBillingTier on the wire use `null`
// for unlimited, but tygo generates `number | undefined` from Go's `*int`.
// Declare the client-facing shapes explicitly so callers can pass `null`
// without a cast — the JSON round-trips cleanly either way.
export type AdminUpdateBillingTier = {
	camera_limit: number | null;
	storage_gb: number | null;
	name?: string;
};

export type AdminCreateBillingTier = {
	name: string;
	camera_limit: number | null;
	storage_gb: number | null;
	price_cents: number;
	currency: string;
	interval: 'month' | 'year';
};

export type AdminArchiveConflict = {
	error: 'active_subscribers';
	active_subscribers: number;
};

export type AdminRepriceBillingTier = {
	price_cents: number;
	migrate_subscribers: boolean;
	prorate: boolean;
	confirm_dropping_subscribers: boolean;
};

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

// --- Admin ---

export async function adminListBillingTiers(): Promise<AdminListBillingTiersResponse> {
	const res = await fetch(`${API_BASE}/admin/billing/tiers`, { credentials: 'include' });
	if (!res.ok) throw new Error(`adminListBillingTiers failed: ${res.status}`);
	return res.json();
}

export async function adminUpdateBillingTier(
	priceID: string,
	update: AdminUpdateBillingTier,
): Promise<AdminListBillingTiersResponse> {
	const res = await fetch(`${API_BASE}/admin/billing/tiers/${encodeURIComponent(priceID)}`, {
		method: 'PATCH',
		headers: headers(),
		body: JSON.stringify(update),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`adminUpdateBillingTier failed: ${res.status}`);
	return res.json();
}

export async function adminCreateBillingTier(
	create: AdminCreateBillingTier,
): Promise<AdminListBillingTiersResponse> {
	const res = await fetch(`${API_BASE}/admin/billing/tiers`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify(create),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`adminCreateBillingTier failed: ${res.status}`);
	return res.json();
}

/**
 * Archive a tier. Returns the fresh list on success, or a typed
 * AdminArchiveConflict when the tier still has active subscribers and
 * `confirm` was false — the caller is expected to show a confirmation
 * dialog and retry with `confirm=true`.
 */
export async function adminArchiveBillingTier(
	priceID: string,
	confirm: boolean,
): Promise<{ ok: true; tiers: AdminListBillingTiersResponse } | { ok: false; conflict: AdminArchiveConflict }> {
	const res = await fetch(
		`${API_BASE}/admin/billing/tiers/${encodeURIComponent(priceID)}/archive`,
		{
			method: 'POST',
			headers: headers(),
			body: JSON.stringify({ confirm }),
			credentials: 'include',
		},
	);
	if (res.status === 409) {
		const conflict = (await res.json()) as AdminArchiveConflict;
		return { ok: false, conflict };
	}
	if (!res.ok) throw new Error(`adminArchiveBillingTier failed: ${res.status}`);
	return { ok: true, tiers: await res.json() };
}

export async function adminGetTierSubscribers(
	priceID: string,
): Promise<AdminBillingTierSubscribersResponse> {
	const res = await fetch(
		`${API_BASE}/admin/billing/tiers/${encodeURIComponent(priceID)}/subscribers`,
		{ credentials: 'include' },
	);
	if (!res.ok) throw new Error(`adminGetTierSubscribers failed: ${res.status}`);
	return res.json();
}

// --- Admin: Users ---

export async function adminListUsers(): Promise<AdminListUsersResponse> {
	const res = await fetch(`${API_BASE}/admin/users`, { credentials: 'include' });
	if (!res.ok) throw new Error(`adminListUsers failed: ${res.status}`);
	return res.json();
}

export async function adminCreateUser(
	body: AdminCreateUserRequest,
): Promise<AdminCreateUserResponse> {
	const res = await fetch(`${API_BASE}/admin/users`, {
		method: 'POST',
		headers: headers(),
		body: JSON.stringify(body),
		credentials: 'include',
	});
	if (res.status === 409) {
		throw new Error('email_exists');
	}
	if (!res.ok) throw new Error(`adminCreateUser failed: ${res.status}`);
	return res.json();
}

export async function adminUpdateUser(
	userID: string,
	body: AdminUpdateUserRequest,
): Promise<void> {
	const res = await fetch(`${API_BASE}/admin/users/${encodeURIComponent(userID)}`, {
		method: 'PATCH',
		headers: headers(),
		body: JSON.stringify(body),
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`adminUpdateUser failed: ${res.status}`);
}

export async function adminResetUserPassword(
	userID: string,
): Promise<AdminResetPasswordResponse> {
	const res = await fetch(
		`${API_BASE}/admin/users/${encodeURIComponent(userID)}/reset-password`,
		{
			method: 'POST',
			headers: headers(),
			credentials: 'include',
		},
	);
	if (!res.ok) throw new Error(`adminResetUserPassword failed: ${res.status}`);
	return res.json();
}

export async function adminDeleteUser(userID: string): Promise<void> {
	const res = await fetch(`${API_BASE}/admin/users/${encodeURIComponent(userID)}`, {
		method: 'DELETE',
		credentials: 'include',
	});
	if (!res.ok) {
		// Surface the specific server reason so the UI can tell the
		// admin WHY a delete was refused (admin target / self / already
		// deleted). Body is a {error: string} object.
		let reason = `adminDeleteUser failed: ${res.status}`;
		try {
			const body = (await res.json()) as { error?: string };
			if (body?.error) reason = body.error;
		} catch {
			/* ignore */
		}
		throw new Error(reason);
	}
}

// --- Admin: Cameras ---

export async function adminListCameras(): Promise<AdminListCamerasResponse> {
	const res = await fetch(`${API_BASE}/admin/cameras`, { credentials: 'include' });
	if (!res.ok) throw new Error(`adminListCameras failed: ${res.status}`);
	return res.json();
}

/**
 * Reassign a camera to a different user. Returns ok=false with a typed
 * conflict when the target user is already at their tier limit.
 */
export async function adminReassignCamera(
	deviceID: string,
	body: AdminReassignCameraRequest,
): Promise<
	{ ok: true } | { ok: false; conflict: AdminReassignCameraConflictResponse }
> {
	const res = await fetch(`${API_BASE}/admin/cameras/${encodeURIComponent(deviceID)}`, {
		method: 'PATCH',
		headers: headers(),
		body: JSON.stringify(body),
		credentials: 'include',
	});
	if (res.status === 409) {
		const conflict = (await res.json()) as AdminReassignCameraConflictResponse;
		return { ok: false, conflict };
	}
	if (!res.ok) throw new Error(`adminReassignCamera failed: ${res.status}`);
	return { ok: true };
}

export async function adminDeleteCamera(deviceID: string): Promise<void> {
	const res = await fetch(`${API_BASE}/admin/cameras/${encodeURIComponent(deviceID)}`, {
		method: 'DELETE',
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`adminDeleteCamera failed: ${res.status}`);
}

/**
 * Reprice a tier (create new price, optionally migrate subscribers,
 * archive the old price). Same discriminated-union shape as archive:
 * on 409 the server is refusing to drop subscribers silently.
 */
export async function adminRepriceBillingTier(
	priceID: string,
	update: AdminRepriceBillingTier,
): Promise<
	| { ok: true; response: AdminRepriceBillingTierResponse }
	| { ok: false; conflict: AdminArchiveConflict }
> {
	const res = await fetch(
		`${API_BASE}/admin/billing/tiers/${encodeURIComponent(priceID)}/reprice`,
		{
			method: 'POST',
			headers: headers(),
			body: JSON.stringify(update),
			credentials: 'include',
		},
	);
	if (res.status === 409) {
		const conflict = (await res.json()) as AdminArchiveConflict;
		return { ok: false, conflict };
	}
	if (!res.ok) throw new Error(`adminRepriceBillingTier failed: ${res.status}`);
	return { ok: true, response: await res.json() };
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

/**
 * Delete a single batch of footage for a camera. Omitting both
 * `fromMs` and `toMs` tells the server to delete every segment. The
 * server processes deletions in bounded batches (see the `has_more`
 * flag) — callers should use {@link purgeFootage} in `$lib/footage`,
 * which loops until drained.
 */
export async function deleteFootage(
	deviceId: string,
	opts?: { fromMs?: number; toMs?: number },
): Promise<DeleteFootageResponse> {
	const params = new URLSearchParams();
	const fromMs = opts?.fromMs ?? 0;
	const toMs = opts?.toMs ?? 0;
	if (fromMs > 0) params.set('from_ms', String(Math.floor(fromMs)));
	if (toMs > 0) params.set('to_ms', String(Math.floor(toMs)));
	const qs = params.toString();
	const url = `${API_BASE}/cameras/${encodeURIComponent(deviceId)}/footage${qs ? `?${qs}` : ''}`;
	const res = await fetch(url, { method: 'DELETE', credentials: 'include' });
	if (!res.ok) throw new Error(`deleteFootage failed: ${res.status}`);
	return res.json();
}

// --- Firmware / Pi images ---

/**
 * Fetch the list of available Pi device images. The server ingests
 * these from the GitHub release webhook, so the list is empty until the
 * first release after the server was deployed. Public endpoint — no
 * credentials required but sent for consistency.
 */
export async function fetchPiImages(): Promise<PiImagesResponse> {
	const res = await fetch(`${API_BASE}/firmware/images`, {
		credentials: 'include',
	});
	if (!res.ok) throw new Error(`fetchPiImages failed: ${res.status}`);
	return res.json();
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
