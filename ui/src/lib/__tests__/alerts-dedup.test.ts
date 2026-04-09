import { describe, it, expect } from 'vitest';

// Inline the upsert logic from alerts store for isolated testing
type AlertType = 'disconnect' | 'reconnect' | 'motion' | 'storage_capped';

interface Alert {
	id: string;
	type: AlertType;
	cameraId: string;
	message: string;
	timestamp: number;
	read: boolean;
}

const UPSERT_TYPES = new Set<AlertType>(['storage_capped', 'disconnect', 'reconnect']);

function addAlert(alerts: Alert[], type: AlertType, cameraId: string, message: string, eventId?: string): Alert[] {
	if (UPSERT_TYPES.has(type)) {
		const idx = alerts.findIndex((a) => a.type === type && a.cameraId === cameraId);
		if (idx >= 0) {
			const existing = { ...alerts[idx], timestamp: Date.now(), message, read: false };
			if (eventId) existing.id = eventId;
			return [existing, ...alerts.filter((_, i) => i !== idx)];
		}
	}
	return [{ id: eventId ?? `client-${Date.now()}`, type, cameraId, message, timestamp: Date.now(), read: false }, ...alerts];
}

describe('alert deduplication', () => {
	it('upserts storage_capped for same camera', () => {
		let alerts: Alert[] = [];
		alerts = addAlert(alerts, 'storage_capped', 'cam-1', 'Storage full');
		alerts = addAlert(alerts, 'storage_capped', 'cam-1', 'Storage still full');
		expect(alerts).toHaveLength(1);
		expect(alerts[0].message).toBe('Storage still full');
	});

	it('does not upsert storage_capped for different cameras', () => {
		let alerts: Alert[] = [];
		alerts = addAlert(alerts, 'storage_capped', 'cam-1', 'Full 1');
		alerts = addAlert(alerts, 'storage_capped', 'cam-2', 'Full 2');
		expect(alerts).toHaveLength(2);
	});

	it('does not upsert motion events', () => {
		let alerts: Alert[] = [];
		alerts = addAlert(alerts, 'motion', 'cam-1', 'Motion 1', 'ev-1');
		alerts = addAlert(alerts, 'motion', 'cam-1', 'Motion 2', 'ev-2');
		expect(alerts).toHaveLength(2);
	});

	it('upserts disconnect for same camera', () => {
		let alerts: Alert[] = [];
		alerts = addAlert(alerts, 'disconnect', 'cam-1', 'Disconnected');
		alerts = addAlert(alerts, 'disconnect', 'cam-1', 'Still disconnected');
		expect(alerts).toHaveLength(1);
	});

	it('bumps upserted alert to top', () => {
		let alerts: Alert[] = [];
		alerts = addAlert(alerts, 'motion', 'cam-1', 'Motion 1', 'ev-1');
		alerts = addAlert(alerts, 'storage_capped', 'cam-1', 'Full');
		alerts = addAlert(alerts, 'motion', 'cam-1', 'Motion 2', 'ev-2');
		// storage_capped is sandwiched; upsert should bump it to top
		alerts = addAlert(alerts, 'storage_capped', 'cam-1', 'Still full');
		expect(alerts[0].type).toBe('storage_capped');
		expect(alerts[0].message).toBe('Still full');
		expect(alerts).toHaveLength(3);
	});
});
