// Reactive auth store backed by the `ghostcam-token` cookie.
//
// The JWT cookie is not HttpOnly, so the UI can decode it client-side and
// derive claim-based values (like the user's email) reactively. The cookie
// remains the single source of truth — login/logout/change-password all
// rewrite it, and the store re-reads it via `refresh()` at those moments.

const COOKIE_NAME = 'ghostcam-token';

type JwtClaims = {
	sub?: string;
	email?: string;
	is_admin?: boolean;
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
	// is_admin is a UI hint only; the server's adminAuth middleware
	// re-validates the admin email on every admin-scoped request so a
	// forged claim cannot grant elevated access.
	isAdmin = $derived<boolean>(this.claims?.is_admin === true);

	/** Re-read the cookie. Call after login, logout, or change-password. */
	refresh() {
		this.token = readTokenFromCookie();
	}
}

export const authStore = new AuthStore();
