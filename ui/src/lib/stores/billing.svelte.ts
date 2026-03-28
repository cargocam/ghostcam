import type { SubscriptionInfo, UsageInfo } from '$lib/types.js';
import {
	getSubscription,
	getUsage,
	createPortal,
} from '$lib/signaling.js';

class BillingStore {
	subscription = $state<SubscriptionInfo | null>(null);
	usage = $state<UsageInfo | null>(null);
	loading = $state(false);
	error = $state<string | null>(null);

	billingEnabled = $derived(this.subscription?.billing_enabled ?? false);
	currentTier = $derived(this.subscription?.tier ?? 'free');
	isPastDue = $derived(this.subscription?.status === 'past_due');
	isSuspended = $derived(this.subscription?.status === 'suspended');
	stripePublicKey = $derived(this.subscription?.stripe_public_key ?? null);
	stripePricingTableId = $derived(this.subscription?.stripe_pricing_table_id ?? null);

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
