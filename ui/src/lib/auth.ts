const API_BASE = '/api/v1';

export async function login(email: string, password: string): Promise<boolean> {
	const res = await fetch(`${API_BASE}/auth/login`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, password }),
		credentials: 'include',
	});
	return res.ok;
}

export async function register(
	email: string,
	password: string,
	displayName?: string,
): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/register`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({
			email,
			password,
			...(displayName ? { display_name: displayName } : {}),
		}),
		credentials: 'include',
	});
	if (res.ok) return { ok: true };
	const text = await res.text();
	return { ok: false, error: text || 'Registration failed' };
}

export async function logout(): Promise<void> {
	await fetch(`${API_BASE}/auth/logout`, {
		method: 'POST',
		credentials: 'include',
	});
}

export async function checkSession(): Promise<boolean> {
	const res = await fetch(`${API_BASE}/cameras`, {
		credentials: 'include',
	});
	return res.ok;
}

export async function requestEmailChange(
	newEmail: string,
	currentPassword: string,
): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/email`, {
		method: 'PATCH',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ new_email: newEmail, current_password: currentPassword }),
		credentials: 'include',
	});
	if (res.ok) return { ok: true };
	if (res.status === 401) return { ok: false, error: 'Current password is incorrect' };
	if (res.status === 409) return { ok: false, error: 'That email is already in use' };
	if (res.status === 400) {
		const data = await res.json().catch(() => null);
		return { ok: false, error: data?.error || 'Invalid request' };
	}
	return { ok: false, error: 'Failed to request email change' };
}

export async function confirmEmailChange(token: string): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/email/confirm`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ token }),
	});
	if (res.ok) return { ok: true };
	if (res.status === 409) return { ok: false, error: 'That email is already in use' };
	return { ok: false, error: 'Invalid or expired link' };
}

export async function forgotPassword(email: string): Promise<void> {
	await fetch(`${API_BASE}/auth/forgot-password`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email }),
	});
}

export async function resetPassword(
	token: string,
	newPassword: string,
): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/reset-password`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ token, new_password: newPassword }),
	});
	if (res.ok) return { ok: true };
	if (res.status === 400) {
		const data = await res.json().catch(() => null);
		return { ok: false, error: data?.error || 'Invalid request' };
	}
	return { ok: false, error: 'Failed to reset password' };
}

export async function verifyEmail(token: string): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/verify-email`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ token }),
	});
	if (res.ok) return { ok: true };
	return { ok: false, error: 'Invalid or expired link' };
}

export async function requestLoginOTP(email: string): Promise<void> {
	await fetch(`${API_BASE}/auth/otp/request`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email }),
	});
}

export async function verifyLoginOTP(
	email: string,
	code: string,
): Promise<boolean> {
	const res = await fetch(`${API_BASE}/auth/otp/verify`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ email, code }),
		credentials: 'include',
	});
	return res.ok;
}

export async function changePassword(
	currentPassword: string,
	newPassword: string,
): Promise<{ ok: boolean; error?: string }> {
	const res = await fetch(`${API_BASE}/auth/password`, {
		method: 'PATCH',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
		credentials: 'include',
	});
	if (res.ok) return { ok: true };
	if (res.status === 401) return { ok: false, error: 'Current password is incorrect' };
	if (res.status === 400) return { ok: false, error: 'Password must be 8-128 characters' };
	return { ok: false, error: 'Failed to change password' };
}
