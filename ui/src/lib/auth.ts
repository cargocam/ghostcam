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
