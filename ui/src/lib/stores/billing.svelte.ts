import type { SubscriptionResponse, TierInfo, UsageResponse } from '$lib/api-types';
import {
	getSubscription,
	getUsage,
	listTiers,
	refreshTiers,
	createPortal,
	createCheckout,
} from '$lib/signaling.js';

// FreeTierID mirrors billing.FreeTierID on the server.
const FREE_TIER_ID = 'free';

/**
 * Percent of the storage cap at which the UI should surface a soft warning
 * (amber banner). Shared between the banner component and its tests so the
 * threshold has a single source of truth.
 */
export const STORAGE_WARN_PERCENT = 85;

class BillingStore {
	subscription = $state<SubscriptionResponse | null>(null);
	usage = $state<UsageResponse | null>(null);
	// Tiers fetched from GET /billing/tiers. Paid tiers come from the
	// server's Stripe-backed cache; ids are Stripe price IDs
	// (e.g. "price_1ABC..."). The free tier is always present.
	tiers = $state<TierInfo[]>([]);
	// Three-state UX: before load returns, billing is in `loading`.
	// On a clean load success with at least one paid tier, `tiersLoaded`
	// flips true and the UI shows the plan picker. On any failure
	// (including an empty response, which is indistinguishable from a
	// misconfigured Stripe account and equally unusable), `loadError`
	// carries a message and the UI shows a retry button instead of a
	// confusing "no plans available" dead-end.
	loading = $state(false);
	loadError = $state<string | null>(null);
	tiersLoaded = $state(false);
	// Non-null while a checkout/portal click is in flight. Kept separate
	// from `loading` so the main load spinner doesn't flicker when the
	// user clicks "Manage Subscription".
	actionInFlight = $state(false);
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

	async load(forceRefresh = false) {
		// Guard against concurrent invocations. On startup the transport
		// store calls `load()` once and then connects SSE; an incoming
		// `storage_capped` event can land before the first load resolves,
		// which would otherwise fire a second parallel round-trip whose
		// response simply overwrites the first.
		if (this.loading) return;
		this.loading = true;
		this.loadError = null;
		this.error = null;
		try {
			const [sub, usage, tiersResp] = await Promise.all([
				getSubscription(),
				getUsage(),
				// When the user hits Retry we force a fresh Stripe round-trip
				// so metadata they just tagged in the dashboard shows up
				// immediately. The ordinary initial load reads the cached
				// list because hammering Stripe on every settings open
				// would be wasteful.
				forceRefresh ? refreshTiers() : listTiers(),
			]);
			this.subscription = sub;
			this.usage = usage;
			this.tiers = tiersResp.tiers ?? [];

			this.tiersLoaded = true;
		} catch (e) {
			this.loadError = e instanceof Error ? e.message : 'Failed to load billing';
			this.tiersLoaded = false;
		} finally {
			this.loading = false;
		}
	}

	/**
	 * Refresh only the usage numbers (not subscription or tier catalogue).
	 * Used by the SSE path when a `storage_capped` event arrives — we only
	 * need the updated storage_bytes/limit to drive the banner, not a full
	 * three-request round-trip that would also flicker `this.loading` in
	 * any open settings panel.
	 */
	async refreshUsage() {
		try {
			this.usage = await getUsage();
		} catch {
			/* non-fatal: banner will catch up on the next full load */
		}
	}

	/**
	 * Start a Stripe checkout for the given tier. The ID is whatever GET
	 * /billing/tiers returned (a Stripe price ID for paid tiers). There are
	 * no hardcoded tier names on the client — the full list is rendered
	 * from the server response.
	 */
	async checkout(tierID: string) {
		this.actionInFlight = true;
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
			this.actionInFlight = false;
		}
	}

	async openPortal() {
		this.actionInFlight = true;
		this.error = null;
		try {
			const { url } = await createPortal(window.location.origin + '/');
			window.location.href = url;
		} catch (e) {
			this.error = e instanceof Error ? e.message : 'Portal failed';
			this.actionInFlight = false;
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
