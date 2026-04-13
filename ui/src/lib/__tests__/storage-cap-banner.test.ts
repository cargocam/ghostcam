import { describe, it, expect } from 'vitest';
import { STORAGE_WARN_PERCENT } from '$lib/stores/billing.svelte.js';

// Inline the banner visibility logic from StorageCapBanner.svelte so we can
// test the thresholds without spinning up a DOM runtime. The warning
// threshold itself is imported from the store so any change there is
// immediately reflected here — no manual "keep in sync" comment needed.
const WARN_PERCENT = STORAGE_WARN_PERCENT;

interface Usage {
	storage_bytes: number;
	storage_limit_gb: number | null;
}

function derive(usage: Usage | null) {
	const usedGB = (usage?.storage_bytes ?? 0) / (1024 * 1024 * 1024);
	const limitGB = usage?.storage_limit_gb ?? null;
	const percent =
		limitGB != null && limitGB > 0 ? Math.min(100, (usedGB / limitGB) * 100) : 0;
	const isCapped = limitGB != null && limitGB > 0 && usedGB >= limitGB;
	const inWarning = !isCapped && percent >= WARN_PERCENT;
	return { percent, isCapped, inWarning };
}

function visible(usage: Usage | null, dismissedWarning: boolean): boolean {
	const { isCapped, inWarning } = derive(usage);
	return isCapped || (inWarning && !dismissedWarning);
}

const GB = 1024 * 1024 * 1024;

describe('storage cap banner visibility', () => {
	it('hides on unlimited plans', () => {
		expect(visible({ storage_bytes: 500 * GB, storage_limit_gb: null }, false)).toBe(false);
	});

	it('hides below the warning threshold', () => {
		// 50 GB of 100 GB = 50% — well clear of the 85% warning
		expect(visible({ storage_bytes: 50 * GB, storage_limit_gb: 100 }, false)).toBe(false);
	});

	it('shows a warning at or above 85%', () => {
		expect(visible({ storage_bytes: 85 * GB, storage_limit_gb: 100 }, false)).toBe(true);
		expect(visible({ storage_bytes: 99 * GB, storage_limit_gb: 100 }, false)).toBe(true);
	});

	it('warning is dismissible for the session', () => {
		expect(visible({ storage_bytes: 90 * GB, storage_limit_gb: 100 }, true)).toBe(false);
	});

	it('capped banner is persistent and ignores dismissal', () => {
		// User dismissed the warning earlier; once capped, the banner must
		// come back regardless of the prior dismissal.
		expect(visible({ storage_bytes: 100 * GB, storage_limit_gb: 100 }, true)).toBe(true);
		expect(visible({ storage_bytes: 200 * GB, storage_limit_gb: 100 }, true)).toBe(true);
	});

	it('hides when no usage data is loaded', () => {
		expect(visible(null, false)).toBe(false);
	});

	it('reports 100% when usage exceeds limit', () => {
		const { percent, isCapped } = derive({ storage_bytes: 200 * GB, storage_limit_gb: 100 });
		expect(percent).toBe(100);
		expect(isCapped).toBe(true);
	});
});
