import type { SubscriptionResponse, UsageResponse } from '$lib/api-types';
import {
	getSubscription,
	getUsage,
	createPortal,
	createCheckout,
} from '$lib/signaling.js';

class BillingStore {
	subscription = $state<SubscriptionResponse | null>(null);
	usage = $state<UsageResponse | null>(null);
	loading = $state(false);
	error = $state<string | null>(null);

	billingEnabled = $derived(this.subscription?.billing_enabled ?? false);
	currentTier = $derived(this.subscription?.tier ?? 'free');

	storageUsedGB = $derived((this.usage?.storage_bytes ?? 0) / (1024 * 1024 * 1024));
	storageLimitGB = $derived(this.usage?.storage_limit_gb ?? null);
	storagePercent = $derived(
		this.storageLimitGB != null && this.storageLimitGB > 0
			? Math.min(100, (this.storageUsedGB / this.storageLimitGB) * 100)
			: 0
	);
	isStorageCapped = $derived(
		this.storageLimitGB != null && this.storageLimitGB > 0 && this.storageUsedGB >= this.storageLimitGB
	);

	async load() {
		this.loading = true;
		this.error = null;
		try {
			const [sub, usage] = await Promise.all([
				getSubscription(),
				getUsage(),
			]);
			this.subscription = sub;
			this.usage = usage;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Failed to load billing';
		} finally {
			this.loading = false;
		}
	}

	async checkout(tier: string) {
		try {
			const { url } = await createCheckout(
				tier,
				window.location.origin + '/?checkout=success',
				window.location.origin + '/?checkout=cancel',
			);
			window.location.href = url;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Checkout failed';
		}
	}

	async openPortal() {
		try {
			const { url } = await createPortal(window.location.origin + '/');
			window.location.href = url;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Portal failed';
		}
	}
}

export const billingStore = new BillingStore();
