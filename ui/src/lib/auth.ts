const API_BASE = '/api/v1';

export async function login(password: string): Promise<boolean> {
	const res = await fetch(`${API_BASE}/auth/login`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ password }),
		credentials: 'include',
	});
	return res.ok;
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
