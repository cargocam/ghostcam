import type { ClientLogEntry } from '$lib/api-types';

// Persisted developer-mode settings. Kept in its own store so the values
// survive reloads (localStorage) and so tree-shaking of debug helpers is
// trivial (all consumers go through this module).
const STORAGE_KEY = 'ghostcam-dev';

interface DevStatePersisted {
	clientLogging: boolean;
}

function load(): DevStatePersisted {
	if (typeof localStorage === 'undefined') {
		return { clientLogging: false };
	}
	try {
		const raw = localStorage.getItem(STORAGE_KEY);
		if (!raw) return { clientLogging: false };
		const parsed = JSON.parse(raw) as Partial<DevStatePersisted>;
		return { clientLogging: parsed.clientLogging === true };
	} catch {
		return { clientLogging: false };
	}
}

class DevStore {
	clientLogging = $state<boolean>(load().clientLogging);

	setClientLogging(v: boolean) {
		this.clientLogging = v;
		try {
			localStorage.setItem(STORAGE_KEY, JSON.stringify({ clientLogging: v }));
		} catch {
			// localStorage unavailable (private browsing / quota) — state
			// still lives in memory for the current session.
		}
	}
}

export const devStore = new DevStore();

/**
 * Forward a diagnostic entry to POST /api/v1/client-log when the
 * "Client error logging" developer toggle is enabled. Silently no-ops
 * otherwise — zero network traffic in the default state.
 *
 * Failures are swallowed on purpose: we do not want a logging error to
 * cascade into another surface, and the user already has a local
 * fallback (the "No footage" overlay, console logs) for anything they
 * really need to see.
 */
export function reportClientLog(
	entry: Omit<ClientLogEntry, 'user_agent' | 'url'> & {
		user_agent?: string;
		url?: string;
	},
): void {
	if (!devStore.clientLogging) return;

	const payload: ClientLogEntry = {
		level: entry.level,
		source: entry.source,
		message: entry.message,
		user_agent: entry.user_agent ?? (typeof navigator !== 'undefined' ? navigator.userAgent : ''),
		url: entry.url ?? (typeof window !== 'undefined' ? window.location.href : ''),
		context: entry.context,
	};

	try {
		void fetch('/api/v1/client-log', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(payload),
			credentials: 'include',
			keepalive: true,
		}).catch(() => {});
	} catch {
		// Synchronous fetch failure (malformed URL / CSP) — nothing to do.
	}
}
