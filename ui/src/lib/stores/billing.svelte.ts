import type { SubscriptionResponse, TierInfo, UsageResponse } from '$lib/api-types';
import {
	getSubscription,
	getUsage,
	listTiers,
	createPortal,
	createCheckout,
} from '$lib/signaling.js';

// FreeTierID mirrors billing.FreeTierID on the server.
const FREE_TIER_ID = 'free';

class BillingStore {
	subscription = $state<SubscriptionResponse | null>(null);
	usage = $state<UsageResponse | null>(null);
	// Tiers fetched from GET /billing/tiers. Paid tiers come from the
	// server's Stripe-backed cache; ids are Stripe price IDs
	// (e.g. "price_1ABC..."). The free tier is always present.
	tiers = $state<TierInfo[]>([]);
	loading = $state(false);
	error = $state<string | null>(null);

	billingEnabled = $derived(this.subscription?.billing_enabled ?? false);
	currentTier = $derived(this.subscription?.tier ?? FREE_TIER_ID);
	currentTierName = $derived(this.subscription?.tier_name ?? 'Free');

	paidTiers = $derived(this.tiers.filter((t) => t.id !== FREE_TIER_ID));

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
			const [sub, usage, tiers] = await Promise.all([
				getSubscription(),
				getUsage(),
				listTiers().catch(() => ({ tiers: [] as TierInfo[] })),
			]);
			this.subscription = sub;
			this.usage = usage;
			this.tiers = tiers.tiers ?? [];
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Failed to load billing';
		} finally {
			this.loading = false;
		}
	}

	/**
	 * Start a Stripe checkout for the given tier. The ID is whatever GET
	 * /billing/tiers returned (a Stripe price ID for paid tiers). There are
	 * no hardcoded tier names on the client — the full list is rendered
	 * from the server response.
	 */
	async checkout(tierID: string) {
		this.loading = true;
		this.error = null;
		try {
			const { url } = await createCheckout(
				tierID,
				window.location.origin + '/?checkout=success',
				window.location.origin + '/?checkout=cancel',
			);
			window.location.href = url;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Checkout failed';
			this.loading = false;
		}
	}

	async openPortal() {
		this.loading = true;
		this.error = null;
		try {
			const { url } = await createPortal(window.location.origin + '/');
			window.location.href = url;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Portal failed';
			this.loading = false;
		}
	}
}

/** Format a price returned from the billing API for display. */
export function formatTierPrice(tier: TierInfo): string {
	if (tier.price_cents <= 0 || !tier.currency) return 'Free';
	const amount = tier.price_cents / 100;
	const amountStr = amount.toLocaleString(undefined, {
		style: 'currency',
		currency: tier.currency.toUpperCase(),
		minimumFractionDigits: amount % 1 === 0 ? 0 : 2,
	});
	if (tier.interval) return `${amountStr}/${tier.interval}`;
	return amountStr;
}

/** Human-readable camera limit label. */
export function formatCameraLimit(tier: TierInfo): string {
	return tier.camera_limit == null
		? 'Unlimited cameras'
		: `${tier.camera_limit} camera${tier.camera_limit === 1 ? '' : 's'}`;
}

/** Human-readable storage limit label. */
export function formatStorageLimit(tier: TierInfo): string {
	return tier.storage_gb == null ? 'Unlimited storage' : `${tier.storage_gb} GB storage`;
}

export const billingStore = new BillingStore();
