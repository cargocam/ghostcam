<script lang="ts">
	import { onMount } from 'svelte';
	import type { AdminBillingTier } from '$lib/api-types';
	import {
		adminListBillingTiers,
		adminUpdateBillingTier,
		type AdminUpdateBillingTier,
	} from '$lib/signaling.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { ArrowLeft, RefreshCw, Save, CheckCircle2, AlertTriangle } from 'lucide-svelte';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cn } from '$lib/utils.js';

	// Admin view. Designed as a top-level surface with a list of cards so
	// additional admin modules can drop in without restructuring the page.
	// Right now there's one card: Billing Tiers (Stripe product metadata
	// editor). Future cards should follow the same section pattern.

	type Draft = {
		cameraLimitText: string; // raw input value, "" = unlimited
		storageGBText: string;
		dirty: boolean;
		saving: boolean;
		error: string | null;
	};

	let tiers = $state<AdminBillingTier[]>([]);
	let loading = $state<boolean>(true);
	let loadError = $state<string | null>(null);
	let drafts = $state<Record<string, Draft>>({});

	async function load() {
		loading = true;
		loadError = null;
		try {
			const resp = await adminListBillingTiers();
			tiers = resp.tiers ?? [];
			// Reset drafts from the fresh server values.
			drafts = Object.fromEntries(
				tiers.map((t) => [
					t.price_id,
					{
						cameraLimitText: parseLimitToInput(t.camera_limit_raw),
						storageGBText: parseLimitToInput(t.storage_gb_raw),
						dirty: false,
						saving: false,
						error: null,
					},
				]),
			);
		} catch (e) {
			loadError = e instanceof Error ? e.message : 'Failed to load admin billing tiers';
		} finally {
			loading = false;
		}
	}

	// "unlimited" / empty / "-1" all render as an empty input which the user
	// can read as "no limit". Numbers pass through as-is. Bad values from
	// the server (shouldn't happen — it stores what the admin typed) also
	// collapse to empty so the UI stays editable.
	function parseLimitToInput(raw: string): string {
		const s = raw.trim().toLowerCase();
		if (s === '' || s === 'unlimited' || s === 'inf' || s === '-1') return '';
		if (/^\d+$/.test(s)) return s;
		return '';
	}

	// Convert the user's typed text back into the server's *int | null shape.
	// "" → null (unlimited), non-negative integer → number, anything else → error.
	function draftToLimit(text: string): { value: number | null; error: string | null } {
		const s = text.trim();
		if (s === '') return { value: null, error: null };
		if (!/^\d+$/.test(s)) return { value: null, error: 'Must be a whole number or blank for unlimited' };
		return { value: parseInt(s, 10), error: null };
	}

	function markDirty(priceID: string) {
		const d = drafts[priceID];
		if (!d) return;
		drafts[priceID] = { ...d, dirty: true, error: null };
	}

	async function save(tier: AdminBillingTier) {
		const d = drafts[tier.price_id];
		if (!d) return;
		const cam = draftToLimit(d.cameraLimitText);
		const stor = draftToLimit(d.storageGBText);
		if (cam.error || stor.error) {
			drafts[tier.price_id] = {
				...d,
				error: cam.error ?? stor.error,
			};
			return;
		}
		drafts[tier.price_id] = { ...d, saving: true, error: null };

		const update: AdminUpdateBillingTier = {
			camera_limit: cam.value,
			storage_gb: stor.value,
		};
		try {
			const resp = await adminUpdateBillingTier(tier.price_id, update);
			tiers = resp.tiers ?? [];
			drafts = Object.fromEntries(
				tiers.map((t) => [
					t.price_id,
					{
						cameraLimitText: parseLimitToInput(t.camera_limit_raw),
						storageGBText: parseLimitToInput(t.storage_gb_raw),
						dirty: false,
						saving: false,
						error: null,
					},
				]),
			);
		} catch (e) {
			drafts[tier.price_id] = {
				...d,
				saving: false,
				error: e instanceof Error ? e.message : 'Save failed',
			};
		}
	}

	function formatPrice(tier: AdminBillingTier): string {
		if (tier.price_cents <= 0 || !tier.currency) return '—';
		const amount = tier.price_cents / 100;
		const amountStr = amount.toLocaleString(undefined, {
			style: 'currency',
			currency: tier.currency.toUpperCase(),
			minimumFractionDigits: amount % 1 === 0 ? 0 : 2,
		});
		return tier.interval ? `${amountStr}/${tier.interval}` : amountStr;
	}

	onMount(load);
</script>

<div class="h-full overflow-y-auto">
	<div class="mx-auto max-w-4xl p-4 sm:p-6 space-y-6">
		<header class="flex items-center justify-between gap-3">
			<div class="flex items-center gap-3">
				<Button
					variant="ghost"
					size="icon"
					onclick={() => settingsStore.setView('live')}
					title="Back to live"
				>
					<ArrowLeft class="h-5 w-5" />
				</Button>
				<div>
					<h1 class="text-lg font-semibold">Admin</h1>
					<p class="text-xs text-muted-foreground">Platform-level configuration</p>
				</div>
			</div>
		</header>

		<!-- Billing Tiers section -->
		<section class="rounded-lg border bg-card">
			<div class="flex items-center justify-between gap-3 px-4 py-3 border-b">
				<div>
					<h2 class="text-sm font-semibold">Billing Tiers</h2>
					<p class="text-xs text-muted-foreground mt-0.5">
						Per-product entitlements stored as Stripe metadata. Changes take
						effect immediately — no deploy required.
					</p>
				</div>
				<Button variant="ghost" size="icon" onclick={load} title="Reload" disabled={loading}>
					<RefreshCw class={cn('h-4 w-4', loading && 'animate-spin')} />
				</Button>
			</div>

			{#if loading && tiers.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">Loading…</div>
			{:else if loadError}
				<div class="p-6 space-y-3">
					<p class="text-sm text-destructive">{loadError}</p>
					<Button variant="outline" size="sm" onclick={load}>Retry</Button>
				</div>
			{:else if tiers.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">
					No active prices found in Stripe. Create products in the Stripe dashboard first.
				</div>
			{:else}
				<div class="divide-y">
					{#each tiers as tier (tier.price_id)}
						{@const draft = drafts[tier.price_id]}
						<div class="p-4 space-y-3">
							<div class="flex items-start justify-between gap-3">
								<div class="min-w-0 flex-1">
									<div class="flex items-center gap-2 flex-wrap">
										<span class="text-sm font-semibold truncate">{tier.product_name || tier.product_id}</span>
										<span class="text-xs text-muted-foreground whitespace-nowrap">{formatPrice(tier)}</span>
										{#if tier.configured}
											<span class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-primary/15 text-primary">
												<CheckCircle2 class="h-3 w-3" />
												Configured
											</span>
										{:else}
											<span class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-warning/15 text-warning">
												<AlertTriangle class="h-3 w-3" />
												Unconfigured
											</span>
										{/if}
									</div>
									<div class="text-[11px] text-muted-foreground font-mono mt-0.5 truncate">{tier.price_id}</div>
								</div>
							</div>

							<div class="grid grid-cols-2 gap-3">
								<label class="block">
									<span class="text-xs text-muted-foreground block mb-1">Camera limit</span>
									<input
										type="text"
										inputmode="numeric"
										placeholder="Unlimited"
										value={draft?.cameraLimitText ?? ''}
										disabled={draft?.saving}
										oninput={(e) => {
											if (draft) {
												drafts[tier.price_id] = { ...draft, cameraLimitText: e.currentTarget.value };
												markDirty(tier.price_id);
											}
										}}
										class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
									/>
								</label>
								<label class="block">
									<span class="text-xs text-muted-foreground block mb-1">Storage (GB)</span>
									<input
										type="text"
										inputmode="numeric"
										placeholder="Unlimited"
										value={draft?.storageGBText ?? ''}
										disabled={draft?.saving}
										oninput={(e) => {
											if (draft) {
												drafts[tier.price_id] = { ...draft, storageGBText: e.currentTarget.value };
												markDirty(tier.price_id);
											}
										}}
										class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
									/>
								</label>
							</div>

							{#if draft?.error}
								<p class="text-xs text-destructive break-words">{draft.error}</p>
							{/if}

							<div class="flex items-center justify-end">
								<Button
									size="sm"
									onclick={() => save(tier)}
									disabled={!draft?.dirty || draft?.saving}
								>
									<Save class="h-3.5 w-3.5 mr-1.5" />
									{draft?.saving ? 'Saving…' : 'Save'}
								</Button>
							</div>
						</div>
					{/each}
				</div>
			{/if}

			<div class="px-4 py-3 border-t text-[11px] text-muted-foreground">
				Tip: blank = unlimited. Fields accept non-negative integers. Stripe
				metadata keys written:
				<code class="font-mono">ghostcam_camera_limit</code> and
				<code class="font-mono">ghostcam_storage_gb</code>.
			</div>
		</section>
	</div>
</div>
