import type { SubscriptionInfo, TierInfo, UsageInfo } from '$lib/types.js';
import {
	getSubscription,
	getTiers,
	getUsage,
	createCheckout,
	createPortal,
} from '$lib/signaling.js';

class BillingStore {
	subscription = $state<SubscriptionInfo | null>(null);
	tiers = $state<TierInfo[]>([]);
	usage = $state<UsageInfo | null>(null);
	loading = $state(false);
	error = $state<string | null>(null);

	billingEnabled = $derived(this.subscription?.billing_enabled ?? false);
	currentTier = $derived(this.subscription?.tier ?? 'free');
	isPastDue = $derived(this.subscription?.status === 'past_due');
	isSuspended = $derived(this.subscription?.status === 'suspended');

	async load() {
		this.loading = true;
		this.error = null;
		try {
			const [sub, tiers, usage] = await Promise.all([
				getSubscription(),
				getTiers(),
				getUsage(),
			]);
			this.subscription = sub;
			this.tiers = tiers;
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
				window.location.origin + '/',
				window.location.origin + '/',
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
