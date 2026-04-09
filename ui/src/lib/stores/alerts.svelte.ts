export type AlertType = 'disconnect' | 'reconnect' | 'motion' | 'storage_capped';

export interface Alert {
	id: number;
	type: AlertType;
	cameraId: string;
	cameraName: string;
	message: string;
	timestamp: number;
	read: boolean;
}

const MAX_ALERTS = 100;

/** Alert types that are state-based (not time-based) and should upsert
 *  instead of creating duplicates. Keyed by type + cameraId. */
const UPSERT_TYPES: Set<AlertType> = new Set(['storage_capped', 'disconnect', 'reconnect']);

class AlertsStore {
	alerts = $state<Alert[]>([]);
	enabled = $state(true);

	private nextId = 1;

	unreadCount = $derived(this.alerts.filter((a) => !a.read).length);

	addAlert(type: AlertType, cameraId: string, cameraName: string, message: string) {
		if (!this.enabled) return;

		// State-based alerts: upsert by type + cameraId
		if (UPSERT_TYPES.has(type)) {
			const idx = this.alerts.findIndex((a) => a.type === type && a.cameraId === cameraId);
			if (idx >= 0) {
				// Bump to top with fresh timestamp, mark unread
				const existing = this.alerts[idx];
				existing.timestamp = Date.now();
				existing.message = message;
				existing.read = false;
				this.alerts = [existing, ...this.alerts.filter((_, i) => i !== idx)];
				return;
			}
		}

		const alert: Alert = {
			id: this.nextId++,
			type,
			cameraId,
			cameraName,
			message,
			timestamp: Date.now(),
			read: false,
		};

		this.alerts = [alert, ...this.alerts].slice(0, MAX_ALERTS);
	}

	markRead(id: number) {
		const idx = this.alerts.findIndex((a) => a.id === id);
		if (idx >= 0) {
			this.alerts[idx].read = true;
		}
	}

	markAllRead() {
		for (const alert of this.alerts) {
			alert.read = true;
		}
	}

	clearAll() {
		this.alerts = [];
	}

	dismiss(id: number) {
		this.alerts = this.alerts.filter((a) => a.id !== id);
	}
}

export const alertsStore = new AlertsStore();
