import {
	fetchEvents,
	markEventRead as apiMarkRead,
	markAllEventsRead as apiMarkAllRead,
	dismissEvent as apiDismiss,
	type ServerEvent,
} from '$lib/signaling.js';

export type AlertType = 'disconnect' | 'reconnect' | 'motion' | 'storage_capped';

export interface Alert {
	id: string; // Redis stream ID (or client-generated for ephemeral alerts)
	type: AlertType;
	cameraId: string;
	cameraName: string;
	message: string;
	timestamp: number;
	read: boolean;
	/** Server-persisted events have a stream ID; client-only alerts do not. */
	persisted: boolean;
	/** Number of clustered events (e.g. "Motion detected +5"). Default 1. */
	count: number;
}

const MAX_ALERTS = 200;

/** Alert types that are state-based and should upsert instead of duplicating. */
const UPSERT_TYPES: Set<AlertType> = new Set(['storage_capped', 'disconnect', 'reconnect']);

let clientIdCounter = 0;

class AlertsStore {
	alerts = $state<Alert[]>([]);
	enabled = $state(true);
	initialized = $state(false);

	unreadCount = $derived(this.alerts.filter((a) => !a.read).length);

	/** Load persisted events from server on startup. */
	async initialize() {
		try {
			const events = await fetchEvents(100);
			this.alerts = events.map(serverEventToAlert);
			this.initialized = true;
		} catch {
			this.initialized = true; // proceed even if server unavailable
		}
	}

	/** Add an alert from an SSE event. Server-persisted events include an event_id. */
	addAlert(type: AlertType, cameraId: string, cameraName: string, message: string, eventId?: string) {
		if (!this.enabled) return;

		// State-based alerts: upsert by type + cameraId
		if (UPSERT_TYPES.has(type)) {
			const idx = this.alerts.findIndex((a) => a.type === type && a.cameraId === cameraId);
			if (idx >= 0) {
				const existing = this.alerts[idx];
				existing.timestamp = Date.now();
				existing.message = message;
				existing.read = false;
				if (eventId) existing.id = eventId;
				this.alerts = [existing, ...this.alerts.filter((_, i) => i !== idx)];
				return;
			}
		}

		// Motion alerts: cluster consecutive events for the same camera within 60s
		if (type === 'motion') {
			const recent = this.alerts[0];
			if (recent && recent.type === 'motion' && recent.cameraId === cameraId && !recent.read
				&& Date.now() - recent.timestamp < 60_000) {
				recent.count++;
				recent.timestamp = Date.now();
				recent.message = `Motion detected +${recent.count - 1}`;
				if (eventId) recent.id = eventId;
				this.alerts = [...this.alerts]; // trigger reactivity
				return;
			}
		}

		const alert: Alert = {
			id: eventId ?? `client-${++clientIdCounter}`,
			type,
			cameraId,
			cameraName,
			message,
			timestamp: Date.now(),
			read: false,
			persisted: !!eventId,
			count: 1,
		};

		this.alerts = [alert, ...this.alerts].slice(0, MAX_ALERTS);
	}

	async markRead(id: string) {
		const alert = this.alerts.find((a) => a.id === id);
		if (!alert || alert.read) return;
		alert.read = true;
		if (alert.persisted) {
			try { await apiMarkRead(id); } catch { /* SSE sync will handle */ }
		}
	}

	async markAllRead() {
		for (const alert of this.alerts) {
			alert.read = true;
		}
		try { await apiMarkAllRead(); } catch { /* best effort */ }
	}

	async dismiss(id: string) {
		const alert = this.alerts.find((a) => a.id === id);
		this.alerts = this.alerts.filter((a) => a.id !== id);
		if (alert?.persisted) {
			try { await apiDismiss(id); } catch { /* best effort */ }
		}
	}

	clearAll() {
		this.alerts = [];
	}

	// --- Cross-client sync via SSE ---

	/** Apply a sync action from another client. */
	applySync(action: string, eventId?: string) {
		switch (action) {
			case 'read':
				if (eventId) {
					const a = this.alerts.find((x) => x.id === eventId);
					if (a) a.read = true;
				}
				break;
			case 'read_all':
				for (const a of this.alerts) a.read = true;
				break;
			case 'dismiss':
				if (eventId) {
					this.alerts = this.alerts.filter((a) => a.id !== eventId);
				}
				break;
		}
	}
}

function serverEventToAlert(e: ServerEvent): Alert {
	let type: AlertType = 'motion';
	if (e.type === 'storage_capped') type = 'storage_capped';
	else if (e.type === 'disconnect') type = 'disconnect';
	else if (e.type === 'reconnect') type = 'reconnect';

	// Try to extract a readable message from the data JSON
	let message = e.type;
	let cameraName = e.device_id?.slice(0, 8) ?? '';
	try {
		const d = JSON.parse(e.data);
		if (e.type === 'motion_detected') message = 'Motion detected';
		if (e.type === 'storage_capped') message = `Storage limit reached`;
		if (d.device_id) cameraName = d.device_id.slice(0, 8);
	} catch { /* use defaults */ }

	return {
		id: e.id,
		type,
		cameraId: e.device_id,
		cameraName,
		message,
		timestamp: e.created_at,
		read: e.read,
		persisted: true,
		count: 1,
	};
}

export const alertsStore = new AlertsStore();
