interface CameraOverrides {
	displayName?: string;
}

const STORAGE_KEY = 'ghostcam-camera-config';

class CameraConfigStore {
	overrides = $state<Record<string, CameraOverrides>>({});

	constructor() {
		if (typeof window !== 'undefined') {
			try {
				const raw = localStorage.getItem(STORAGE_KEY);
				if (raw) {
					const data = JSON.parse(raw);
					if (data.overrides) this.overrides = data.overrides;
				}
			} catch {}
		}
	}

	private persist() {
		if (typeof window === 'undefined') return;
		localStorage.setItem(STORAGE_KEY, JSON.stringify({ overrides: this.overrides }));
	}

	getDisplayName(deviceId: string, fallback?: string): string {
		return this.overrides[deviceId]?.displayName ?? fallback ?? deviceId;
	}

	rename(deviceId: string, name: string) {
		const trimmed = name.trim();
		const current = this.overrides[deviceId] ?? {};
		if (!trimmed) {
			delete current.displayName;
		} else {
			current.displayName = trimmed;
		}
		this.overrides = { ...this.overrides, [deviceId]: current };
		this.persist();
	}
}

export const cameraConfigStore = new CameraConfigStore();
