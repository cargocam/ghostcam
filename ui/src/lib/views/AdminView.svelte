<script lang="ts">
	import { onMount } from 'svelte';
	import type { AdminBillingTier } from '$lib/api-types';
	import {
		adminListBillingTiers,
		adminUpdateBillingTier,
		adminCreateBillingTier,
		adminArchiveBillingTier,
		adminGetTierSubscribers,
		adminRepriceBillingTier,
		type AdminUpdateBillingTier,
		type AdminCreateBillingTier,
		type AdminRepriceBillingTier,
	} from '$lib/signaling.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import {
		ArrowLeft,
		RefreshCw,
		Save,
		CheckCircle2,
		AlertTriangle,
		Plus,
		Archive,
		DollarSign,
	} from 'lucide-svelte';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cn } from '$lib/utils.js';

	// Admin view. Designed as a top-level surface with a list of cards so
	// additional admin modules can drop in without restructuring the page.
	// Right now there's one card: Billing Tiers (Stripe product metadata
	// editor). Future cards should follow the same section pattern.

	type Draft = {
		nameText: string;
		cameraLimitText: string; // raw input value, "" = unlimited
		storageGBText: string;
		dirty: boolean;
		saving: boolean;
		error: string | null;
	};

	// Build a draft from a server tier row. Used both on initial load
	// and after every mutation (create / update / archive) to keep the
	// drafts map in sync with the fresh server state.
	function makeDraft(t: AdminBillingTier): Draft {
		return {
			nameText: t.product_name ?? '',
			cameraLimitText: parseLimitToInput(t.camera_limit_raw),
			storageGBText: parseLimitToInput(t.storage_gb_raw),
			dirty: false,
			saving: false,
			error: null,
		};
	}

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
			drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
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

		const trimmedName = d.nameText.trim();
		const update: AdminUpdateBillingTier = {
			camera_limit: cam.value,
			storage_gb: stor.value,
		};
		// Only send `name` if it actually changed — leaves other admin
		// sessions' edits alone if they raced us.
		if (trimmedName !== (tier.product_name ?? '').trim() && trimmedName !== '') {
			update.name = trimmedName;
		}
		try {
			const resp = await adminUpdateBillingTier(tier.price_id, update);
			tiers = resp.tiers ?? [];
			drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
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

	// --- Create ---

	let createOpen = $state<boolean>(false);
	let createForm = $state<{
		name: string;
		cameraLimitText: string;
		storageGBText: string;
		priceDollars: string;
		currency: string;
		interval: 'month' | 'year';
	}>({
		name: '',
		cameraLimitText: '',
		storageGBText: '',
		priceDollars: '',
		currency: 'USD',
		interval: 'month',
	});
	let createSaving = $state<boolean>(false);
	let createError = $state<string | null>(null);

	function openCreate() {
		createForm = {
			name: '',
			cameraLimitText: '',
			storageGBText: '',
			priceDollars: '',
			currency: 'USD',
			interval: 'month',
		};
		createError = null;
		createOpen = true;
	}

	async function submitCreate() {
		createError = null;

		const name = createForm.name.trim();
		if (!name) {
			createError = 'Name is required';
			return;
		}
		const cam = draftToLimit(createForm.cameraLimitText);
		const stor = draftToLimit(createForm.storageGBText);
		if (cam.error || stor.error) {
			createError = cam.error ?? stor.error;
			return;
		}
		const dollars = parseFloat(createForm.priceDollars);
		if (!isFinite(dollars) || dollars <= 0) {
			createError = 'Price must be a positive number';
			return;
		}
		const priceCents = Math.round(dollars * 100);
		const currency = createForm.currency.trim().toLowerCase();
		if (currency.length !== 3) {
			createError = 'Currency must be a 3-letter ISO code';
			return;
		}

		const payload: AdminCreateBillingTier = {
			name,
			camera_limit: cam.value,
			storage_gb: stor.value,
			price_cents: priceCents,
			currency,
			interval: createForm.interval,
		};

		createSaving = true;
		try {
			const resp = await adminCreateBillingTier(payload);
			tiers = resp.tiers ?? [];
			drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
			createOpen = false;
		} catch (e) {
			createError = e instanceof Error ? e.message : 'Create failed';
		} finally {
			createSaving = false;
		}
	}

	// --- Archive ---
	//
	// Archiving is a two-step confirmation flow when the target has
	// active subscribers. First click triggers a server probe which
	// returns 409 + a count; the UI then renders a confirmation prompt
	// that the admin can accept or cancel. Second click (confirm=true)
	// proceeds.

	let archiveConfirm = $state<{
		tier: AdminBillingTier;
		activeSubscribers: number;
	} | null>(null);
	let archiveInFlight = $state<string | null>(null); // price_id currently archiving

	async function archive(tier: AdminBillingTier, confirm: boolean) {
		archiveInFlight = tier.price_id;
		try {
			const result = await adminArchiveBillingTier(tier.price_id, confirm);
			if (result.ok) {
				tiers = result.tiers.tiers ?? [];
				drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
				archiveConfirm = null;
			} else {
				archiveConfirm = {
					tier,
					activeSubscribers: result.conflict.active_subscribers,
				};
			}
		} catch (e) {
			const d = drafts[tier.price_id];
			if (d) {
				drafts[tier.price_id] = {
					...d,
					error: e instanceof Error ? e.message : 'Archive failed',
				};
			}
		} finally {
			archiveInFlight = null;
		}
	}

	// --- Reprice ---
	//
	// Stripe prices are immutable, so "changing the price" is really
	// a three-step atomic server operation: create a new price on the
	// same product, optionally migrate existing subscribers, archive
	// the old price. The dialog gathers the new amount + migration
	// preferences; the server enforces the "don't silently drop
	// paying customers" invariant via the same 409 conflict pattern
	// the archive flow uses.

	let repriceTarget = $state<AdminBillingTier | null>(null);
	let repriceForm = $state<{
		newPriceDollars: string;
		migrate: boolean;
		prorate: boolean;
	}>({ newPriceDollars: '', migrate: true, prorate: true });
	let repriceSaving = $state<boolean>(false);
	let repriceError = $state<string | null>(null);
	let repriceSubscribers = $state<number | null>(null); // null = still loading
	let repriceConfirmDrop = $state<boolean>(false); // second-pass "yes, drop them" flag

	async function openReprice(tier: AdminBillingTier) {
		repriceTarget = tier;
		repriceForm = {
			newPriceDollars: (tier.price_cents / 100).toFixed(2),
			migrate: true,
			prorate: true,
		};
		repriceError = null;
		repriceSubscribers = null;
		repriceConfirmDrop = false;

		try {
			const resp = await adminGetTierSubscribers(tier.price_id);
			repriceSubscribers = resp.active_subscribers;
		} catch (e) {
			repriceError = e instanceof Error ? e.message : 'Failed to load subscriber count';
			repriceSubscribers = 0;
		}
	}

	function closeReprice() {
		repriceTarget = null;
	}

	async function submitReprice() {
		if (!repriceTarget) return;
		repriceError = null;

		const dollars = parseFloat(repriceForm.newPriceDollars);
		if (!isFinite(dollars) || dollars <= 0) {
			repriceError = 'New price must be a positive number';
			return;
		}
		const newPriceCents = Math.round(dollars * 100);
		if (newPriceCents === repriceTarget.price_cents) {
			repriceError = 'New price is the same as the current price';
			return;
		}

		const payload: AdminRepriceBillingTier = {
			price_cents: newPriceCents,
			migrate_subscribers: repriceForm.migrate,
			prorate: repriceForm.migrate && repriceForm.prorate,
			confirm_dropping_subscribers: !repriceForm.migrate && repriceConfirmDrop,
		};

		repriceSaving = true;
		try {
			const result = await adminRepriceBillingTier(repriceTarget.price_id, payload);
			if (result.ok) {
				tiers = result.response.tiers ?? [];
				drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
				const migrated = result.response.migrated_count;
				repriceTarget = null;
				// Show a transient confirmation via the draft error
				// slot of the (now-new) price row? Simpler: log it.
				// The admin already sees the row with its new price
				// on re-render, which is the primary feedback.
				if (migrated > 0) {
					console.info(`Migrated ${migrated} subscriber${migrated === 1 ? '' : 's'}`);
				}
			} else {
				// Server refused to drop subscribers silently. Show
				// the count + an explicit "drop them anyway" toggle.
				repriceSubscribers = result.conflict.active_subscribers;
				repriceConfirmDrop = false;
				repriceError =
					`${result.conflict.active_subscribers} active subscriber` +
					(result.conflict.active_subscribers === 1 ? '' : 's') +
					' will be dropped to the free tier on their next API call.' +
					' Check the confirmation box below to proceed, or tick "Migrate" instead.';
			}
		} catch (e) {
			repriceError = e instanceof Error ? e.message : 'Reprice failed';
		} finally {
			repriceSaving = false;
		}
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
				<div class="min-w-0">
					<h2 class="text-sm font-semibold">Billing Tiers</h2>
					<p class="text-xs text-muted-foreground mt-0.5">
						Per-product entitlements stored as Stripe metadata. Changes take
						effect immediately — no deploy required.
					</p>
				</div>
				<div class="flex items-center gap-1 shrink-0">
					<Button variant="outline" size="sm" onclick={openCreate} title="Create a new tier">
						<Plus class="h-3.5 w-3.5 mr-1" />
						New tier
					</Button>
					<Button variant="ghost" size="icon" onclick={load} title="Reload" disabled={loading}>
						<RefreshCw class={cn('h-4 w-4', loading && 'animate-spin')} />
					</Button>
				</div>
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

							<label class="block">
								<span class="text-xs text-muted-foreground block mb-1">Product name</span>
								<input
									type="text"
									value={draft?.nameText ?? ''}
									disabled={draft?.saving}
									oninput={(e) => {
										if (draft) {
											drafts[tier.price_id] = { ...draft, nameText: e.currentTarget.value };
											markDirty(tier.price_id);
										}
									}}
									class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
								/>
							</label>

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

							<div class="flex items-center justify-end gap-2 flex-wrap">
								<Button
									variant="outline"
									size="sm"
									onclick={() => openReprice(tier)}
									disabled={draft?.saving || archiveInFlight === tier.price_id}
									title="Change the price for this tier"
								>
									<DollarSign class="h-3.5 w-3.5 mr-1.5" />
									Change price
								</Button>
								<Button
									variant="outline"
									size="sm"
									onclick={() => archive(tier, false)}
									disabled={archiveInFlight === tier.price_id || draft?.saving}
									title="Archive this tier"
								>
									<Archive class="h-3.5 w-3.5 mr-1.5" />
									{archiveInFlight === tier.price_id ? 'Archiving…' : 'Archive'}
								</Button>
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

<!-- Create dialog -->
<Dialog bind:open={createOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>New billing tier</DialogTitle>
			<DialogDescription>
				Creates a Stripe product and a recurring price in one step, then
				tags the product with ghostcam limits. Visible to customers
				immediately.
			</DialogDescription>
		</DialogHeader>
		<form
			class="space-y-3 mt-2"
			onsubmit={(e) => {
				e.preventDefault();
				submitCreate();
			}}
		>
			<label class="block">
				<span class="text-xs text-muted-foreground block mb-1">Product name</span>
				<input
					type="text"
					value={createForm.name}
					oninput={(e) => (createForm.name = e.currentTarget.value)}
					placeholder="e.g. Pro"
					required
					class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</label>

			<div class="grid grid-cols-2 gap-3">
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">Camera limit</span>
					<input
						type="text"
						inputmode="numeric"
						placeholder="Unlimited"
						value={createForm.cameraLimitText}
						oninput={(e) => (createForm.cameraLimitText = e.currentTarget.value)}
						class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</label>
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">Storage (GB)</span>
					<input
						type="text"
						inputmode="numeric"
						placeholder="Unlimited"
						value={createForm.storageGBText}
						oninput={(e) => (createForm.storageGBText = e.currentTarget.value)}
						class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</label>
			</div>

			<div class="grid grid-cols-[1fr_auto_auto] gap-3">
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">Price</span>
					<input
						type="text"
						inputmode="decimal"
						placeholder="29.99"
						value={createForm.priceDollars}
						oninput={(e) => (createForm.priceDollars = e.currentTarget.value)}
						required
						class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</label>
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">Currency</span>
					<input
						type="text"
						maxlength="3"
						value={createForm.currency}
						oninput={(e) => (createForm.currency = e.currentTarget.value.toUpperCase())}
						class="w-20 rounded-md border bg-transparent px-3 py-1.5 text-sm uppercase focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</label>
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">Billing</span>
					<select
						value={createForm.interval}
						onchange={(e) => (createForm.interval = e.currentTarget.value as 'month' | 'year')}
						class="rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					>
						<option value="month">Monthly</option>
						<option value="year">Yearly</option>
					</select>
				</label>
			</div>

			{#if createError}
				<p class="text-xs text-destructive break-words">{createError}</p>
			{/if}

			<div class="flex justify-end gap-2 pt-2">
				<Button
					type="button"
					variant="outline"
					onclick={() => (createOpen = false)}
					disabled={createSaving}
				>
					Cancel
				</Button>
				<Button type="submit" disabled={createSaving}>
					{createSaving ? 'Creating…' : 'Create tier'}
				</Button>
			</div>
		</form>
	</DialogContent>
</Dialog>

<!-- Archive confirmation dialog (only shown when the price has live subs) -->
{#if archiveConfirm}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) archiveConfirm = null; }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<AlertTriangle class="h-4 w-4 text-warning" />
					Archive tier with active subscribers?
				</DialogTitle>
				<DialogDescription>
					<span class="font-semibold">{archiveConfirm.tier.product_name}</span>
					has
					<span class="font-semibold">{archiveConfirm.activeSubscribers}</span>
					active subscriber{archiveConfirm.activeSubscribers === 1 ? '' : 's'}.
					Archiving hides the tier from new checkouts but leaves existing
					subscriptions billing at the current price until they cancel.
					Migrate subscribers in the Stripe dashboard first if you need them
					on a different plan.
				</DialogDescription>
			</DialogHeader>
			<div class="flex justify-end gap-2 mt-4">
				<Button
					type="button"
					variant="outline"
					onclick={() => (archiveConfirm = null)}
					disabled={archiveInFlight !== null}
				>
					Cancel
				</Button>
				<Button
					type="button"
					onclick={() => {
						if (archiveConfirm) archive(archiveConfirm.tier, true);
					}}
					disabled={archiveInFlight !== null}
				>
					{archiveInFlight ? 'Archiving…' : 'Archive anyway'}
				</Button>
			</div>
		</DialogContent>
	</Dialog>
{/if}

<!-- Reprice dialog -->
{#if repriceTarget}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) closeReprice(); }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<DollarSign class="h-4 w-4" />
					Change price — {repriceTarget.product_name || repriceTarget.product_id}
				</DialogTitle>
				<DialogDescription>
					Currency and billing interval can't be changed on an existing
					Stripe price — create a new tier if you need those. This updates
					the amount only.
				</DialogDescription>
			</DialogHeader>
			<form
				class="space-y-3 mt-2"
				onsubmit={(e) => {
					e.preventDefault();
					submitReprice();
				}}
			>
				<div class="grid grid-cols-2 gap-3">
					<div>
						<span class="text-xs text-muted-foreground block mb-1">Current price</span>
						<div class="rounded-md border bg-muted/30 px-3 py-1.5 text-sm">
							{repriceTarget.price_cents / 100}
							{repriceTarget.currency.toUpperCase()}/{repriceTarget.interval}
						</div>
					</div>
					<label class="block">
						<span class="text-xs text-muted-foreground block mb-1">New price</span>
						<input
							type="text"
							inputmode="decimal"
							placeholder="29.99"
							value={repriceForm.newPriceDollars}
							oninput={(e) => (repriceForm.newPriceDollars = e.currentTarget.value)}
							required
							class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						/>
					</label>
				</div>

				<div class="rounded-md border border-border/60 bg-muted/20 p-3 space-y-2">
					<div class="text-xs font-semibold flex items-center gap-1.5">
						<span>Existing subscribers</span>
						{#if repriceSubscribers === null}
							<span class="text-muted-foreground font-normal">(checking…)</span>
						{:else}
							<span class="text-muted-foreground font-normal">
								· {repriceSubscribers} active
							</span>
						{/if}
					</div>

					<label class="flex items-start gap-2 text-xs cursor-pointer">
						<input
							type="checkbox"
							checked={repriceForm.migrate}
							onchange={(e) => (repriceForm.migrate = e.currentTarget.checked)}
							class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
						/>
						<span class="flex-1">
							<span class="font-medium block">Migrate these subscribers to the new price</span>
							<span class="block text-muted-foreground mt-0.5">
								Updates each subscription's line item to the new Stripe price
								so they're billed at the new amount on their next invoice.
							</span>
						</span>
					</label>

					{#if repriceForm.migrate}
						<label class="flex items-start gap-2 text-xs cursor-pointer pl-6">
							<input
								type="checkbox"
								checked={repriceForm.prorate}
								onchange={(e) => (repriceForm.prorate = e.currentTarget.checked)}
								class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
							/>
							<span class="flex-1">
								<span class="font-medium block">Prorate the switch</span>
								<span class="block text-muted-foreground mt-0.5">
									Credit or charge the difference for the current billing
									period. Unchecked means the new price starts clean on the
									next invoice.
								</span>
							</span>
						</label>
					{:else if repriceSubscribers !== null && repriceSubscribers > 0}
						<label class="flex items-start gap-2 text-xs cursor-pointer pl-6">
							<input
								type="checkbox"
								checked={repriceConfirmDrop}
								onchange={(e) => (repriceConfirmDrop = e.currentTarget.checked)}
								class="mt-0.5 h-4 w-4 rounded border-border accent-destructive"
							/>
							<span class="flex-1">
								<span class="font-medium block text-destructive">
									Yes, drop existing subscribers to the free tier
								</span>
								<span class="block text-muted-foreground mt-0.5">
									The old price will be archived. Existing subscriptions
									still pointing at it will resolve to the free tier on
									their next API call.
								</span>
							</span>
						</label>
					{/if}
				</div>

				{#if repriceError}
					<p class="text-xs text-destructive break-words">{repriceError}</p>
				{/if}

				<div class="flex justify-end gap-2 pt-2">
					<Button
						type="button"
						variant="outline"
						onclick={closeReprice}
						disabled={repriceSaving}
					>
						Cancel
					</Button>
					<Button
						type="submit"
						disabled={repriceSaving ||
							repriceSubscribers === null ||
							(!repriceForm.migrate &&
								repriceSubscribers > 0 &&
								!repriceConfirmDrop)}
					>
						{repriceSaving ? 'Applying…' : 'Change price'}
					</Button>
				</div>
			</form>
		</DialogContent>
	</Dialog>
{/if}
