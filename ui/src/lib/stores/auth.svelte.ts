// Reactive auth store backed by the `ghostcam-token` cookie.
//
// The JWT cookie is not HttpOnly, so the UI can decode it client-side and
// derive claim-based values (like the user's email) reactively. The cookie
// remains the single source of truth for identity — login/logout/
// change-password all rewrite it and the store re-reads via `refresh()`.
//
// Admin status is deliberately NOT in the JWT: the server resolves it
// from the admins table per-request, so grants/revocations take effect
// without a token rotation. The UI learns its admin status via
// GET /api/v1/auth/me, called on init and after every login/logout.

import type { AuthMeResponse } from '$lib/api-types';

const COOKIE_NAME = 'ghostcam-token';

type JwtClaims = {
	sub?: string;
	email?: string;
	exp?: number;
};

function readTokenFromCookie(): string | null {
	if (typeof document === 'undefined') return null;
	for (const part of document.cookie.split(';')) {
		const [rawName, ...rest] = part.trim().split('=');
		if (rawName === COOKIE_NAME) return rest.join('=') || null;
	}
	return null;
}

function base64UrlDecode(input: string): string {
	let s = input.replace(/-/g, '+').replace(/_/g, '/');
	while (s.length % 4 !== 0) s += '=';
	return atob(s);
}

function decodeJwt(token: string | null): JwtClaims | null {
	if (!token) return null;
	const parts = token.split('.');
	if (parts.length !== 3) return null;
	try {
		return JSON.parse(base64UrlDecode(parts[1])) as JwtClaims;
	} catch {
		return null;
	}
}

class AuthStore {
	token = $state<string | null>(readTokenFromCookie());
	claims = $derived<JwtClaims | null>(decodeJwt(this.token));
	email = $derived<string>(this.claims?.email ?? '');
	userId = $derived<string>(this.claims?.sub ?? '');
	// Admin status is server-resolved via GET /api/v1/auth/me; it is
	// null while the fetch is pending or the user isn't authenticated.
	// A nullish value should be treated as "not admin" by UI code.
	isAdmin = $state<boolean | null>(null);

	/** Re-read the cookie and refetch admin status from the server. */
	refresh() {
		this.token = readTokenFromCookie();
		if (this.token) {
			void this.loadMe();
		} else {
			this.isAdmin = null;
		}
	}

	async loadMe(): Promise<void> {
		try {
			const res = await fetch('/api/v1/auth/me', { credentials: 'include' });
			if (!res.ok) {
				this.isAdmin = null;
				return;
			}
			const data = (await res.json()) as AuthMeResponse;
			this.isAdmin = data.is_admin === true;
		} catch {
			this.isAdmin = null;
		}
	}
}

export const authStore = new AuthStore();

// Kick off the initial /auth/me fetch if the cookie is already present
// at module load (returning visitor with a valid session).
if (typeof document !== 'undefined' && authStore.token) {
	void authStore.loadMe();
}
