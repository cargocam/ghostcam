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

class AlertsStore {
	alerts = $state<Alert[]>([]);
	enabled = $state(true);

	private nextId = 1;

	unreadCount = $derived(this.alerts.filter((a) => !a.read).length);

	addAlert(type: AlertType, cameraId: string, cameraName: string, message: string) {
		if (!this.enabled) return;

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
