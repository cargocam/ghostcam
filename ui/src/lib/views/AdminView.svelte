<script lang="ts">
	import { onMount } from 'svelte';
	import type {
		AdminBillingTier,
		AdminCamera,
		AdminUser,
	} from '$lib/api-types';
	import {
		adminListBillingTiers,
		adminUpdateBillingTier,
		adminCreateBillingTier,
		adminArchiveBillingTier,
		adminRepriceBillingTier,
		adminListUsers,
		adminCreateUser,
		adminUpdateUser,
		adminResetUserPassword,
		adminDeleteUser,
		adminListCameras,
		adminReassignCamera,
		adminDeleteCamera,
		type AdminUpdateBillingTier,
		type AdminCreateBillingTier,
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
		Trash2,
		KeyRound,
		Copy,
		Ban,
		Undo2,
		Shield,
		Users,
		Camera as CameraIcon,
		ArrowRightLeft,
	} from 'lucide-svelte';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cn } from '$lib/utils.js';

	// Admin view. Designed as a top-level surface with a list of cards so
	// additional admin modules can drop in without restructuring the page.
	// Right now there's one card: Billing Tiers (Stripe product metadata
	// editor). Future cards should follow the same section pattern.

	type Draft = {
		nameText: string;
		priceDollarsText: string; // dollars as typed, "9.99"
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
			priceDollarsText: (t.price_cents / 100).toFixed(2),
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

		// Parse and validate the price field. Empty / non-positive is an
		// error — the admin view is Stripe-products-only, never the free
		// tier, so every row must have a real price.
		const priceDollars = parseFloat(d.priceDollarsText);
		if (!isFinite(priceDollars) || priceDollars <= 0) {
			drafts[tier.price_id] = {
				...d,
				error: 'Price must be a positive number',
			};
			return;
		}
		const newPriceCents = Math.round(priceDollars * 100);
		const priceChanged = newPriceCents !== tier.price_cents;

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
			// Step 1: update product name + metadata on the current price.
			// Metadata lives on the Stripe product, so the update persists
			// even when step 2 creates a new price on the same product.
			let resp = await adminUpdateBillingTier(tier.price_id, update);
			tiers = resp.tiers ?? [];

			// Step 2: if the price changed, reprice via the atomic
			// "create new price + migrate subscribers + archive old"
			// flow. Default to migrate + prorate — that's the safe
			// "I'm bumping our prices" intent. Admins who want to
			// drop subscribers instead should archive the tier and
			// create a new one.
			if (priceChanged) {
				const result = await adminRepriceBillingTier(tier.price_id, {
					price_cents: newPriceCents,
					migrate_subscribers: true,
					prorate: true,
					confirm_dropping_subscribers: false,
				});
				if (!result.ok) {
					// Shouldn't happen with migrate=true, but surface it
					// instead of silently dropping rows if the server
					// ever changes that invariant.
					throw new Error(
						`Reprice refused: ${result.conflict.active_subscribers} active ` +
							`subscriber${result.conflict.active_subscribers === 1 ? '' : 's'} ` +
							`would be dropped.`,
					);
				}
				tiers = result.response.tiers ?? [];
			}
			drafts = Object.fromEntries(tiers.map((t) => [t.price_id, makeDraft(t)]));
		} catch (e) {
			drafts[tier.price_id] = {
				...d,
				saving: false,
				error: e instanceof Error ? e.message : 'Save failed',
			};
		}
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

	// ====================================================================
	// Users section
	// ====================================================================

	let users = $state<AdminUser[]>([]);
	let usersLoading = $state<boolean>(true);
	let usersError = $state<string | null>(null);
	let userActionInFlight = $state<string | null>(null); // user_id currently acting

	async function loadUsers() {
		usersLoading = true;
		usersError = null;
		try {
			const resp = await adminListUsers();
			users = resp.users ?? [];
		} catch (e) {
			usersError = e instanceof Error ? e.message : 'Failed to load users';
		} finally {
			usersLoading = false;
		}
	}

	// Toggle disabled flag on a user.
	async function toggleDisabled(user: AdminUser) {
		userActionInFlight = user.user_id;
		try {
			await adminUpdateUser(user.user_id, {
				disabled: user.disabled_at == null,
			});
			await loadUsers();
		} catch (e) {
			usersError = e instanceof Error ? e.message : 'Failed to update user';
		} finally {
			userActionInFlight = null;
		}
	}

	// Create-user dialog
	let createUserOpen = $state<boolean>(false);
	let createUserForm = $state<{ email: string; displayName: string }>({
		email: '',
		displayName: '',
	});
	let createUserSaving = $state<boolean>(false);
	let createUserError = $state<string | null>(null);

	// One-time password reveal (shared between create-user and reset-password flows)
	let passwordReveal = $state<{
		email: string;
		password: string;
		context: 'create' | 'reset';
	} | null>(null);

	function openCreateUser() {
		createUserForm = { email: '', displayName: '' };
		createUserError = null;
		createUserOpen = true;
	}

	async function submitCreateUser() {
		createUserError = null;
		const email = createUserForm.email.trim();
		if (!email) {
			createUserError = 'Email is required';
			return;
		}
		createUserSaving = true;
		try {
			const resp = await adminCreateUser({
				email,
				display_name: createUserForm.displayName.trim(),
			});
			createUserOpen = false;
			passwordReveal = {
				email: resp.user.email,
				password: resp.generated_password,
				context: 'create',
			};
			await loadUsers();
		} catch (e) {
			const msg = e instanceof Error ? e.message : 'Create failed';
			createUserError = msg === 'email_exists' ? 'A user with that email already exists' : msg;
		} finally {
			createUserSaving = false;
		}
	}

	// Reset-password action
	async function resetPassword(user: AdminUser) {
		userActionInFlight = user.user_id;
		try {
			const resp = await adminResetUserPassword(user.user_id);
			passwordReveal = {
				email: user.email,
				password: resp.generated_password,
				context: 'reset',
			};
		} catch (e) {
			usersError = e instanceof Error ? e.message : 'Reset failed';
		} finally {
			userActionInFlight = null;
		}
	}

	// Delete-user confirmation
	let deleteUserConfirm = $state<AdminUser | null>(null);

	async function submitDeleteUser(user: AdminUser) {
		userActionInFlight = user.user_id;
		try {
			await adminDeleteUser(user.user_id);
			deleteUserConfirm = null;
			await loadUsers();
			// Cameras view may be stale — owner deletion affects filter options.
			await loadCameras();
		} catch (e) {
			const msg = e instanceof Error ? e.message : 'Delete failed';
			usersError = humanDeleteError(msg);
		} finally {
			userActionInFlight = null;
		}
	}

	function humanDeleteError(code: string): string {
		switch (code) {
			case 'cannot_delete_admin':
				return 'Cannot delete an admin. Demote them via direct DB query first.';
			case 'self_delete_forbidden':
				return 'You cannot delete yourself.';
			case 'already_deleted':
				return 'That user is already soft-deleted.';
			default:
				return code;
		}
	}

	// Clipboard helper for the password reveal dialog.
	async function copyToClipboard(text: string): Promise<boolean> {
		try {
			await navigator.clipboard.writeText(text);
			return true;
		} catch {
			return false;
		}
	}
	let copyFeedback = $state<string | null>(null);
	async function copyRevealedPassword() {
		if (!passwordReveal) return;
		const ok = await copyToClipboard(passwordReveal.password);
		copyFeedback = ok ? 'Copied' : 'Copy failed — select and copy manually';
		setTimeout(() => (copyFeedback = null), 2500);
	}

	// ====================================================================
	// Cameras section
	// ====================================================================

	let cameras = $state<AdminCamera[]>([]);
	let camerasLoading = $state<boolean>(true);
	let camerasError = $state<string | null>(null);
	let cameraActionInFlight = $state<string | null>(null);

	async function loadCameras() {
		camerasLoading = true;
		camerasError = null;
		try {
			const resp = await adminListCameras();
			cameras = resp.cameras ?? [];
		} catch (e) {
			camerasError = e instanceof Error ? e.message : 'Failed to load cameras';
		} finally {
			camerasLoading = false;
		}
	}

	// Reassign dialog state
	let reassignTarget = $state<AdminCamera | null>(null);
	let reassignToUserID = $state<string>('');
	let reassignSaving = $state<boolean>(false);
	let reassignError = $state<string | null>(null);

	function openReassign(cam: AdminCamera) {
		reassignTarget = cam;
		reassignToUserID = cam.user_id;
		reassignError = null;
	}

	async function submitReassign() {
		if (!reassignTarget) return;
		if (!reassignToUserID) {
			reassignError = 'Pick a user';
			return;
		}
		reassignSaving = true;
		reassignError = null;
		try {
			const result = await adminReassignCamera(reassignTarget.device_id, {
				user_id: reassignToUserID,
			});
			if (result.ok) {
				reassignTarget = null;
				await loadCameras();
			} else {
				reassignError =
					`Target user is at their camera limit ` +
					`(${result.conflict.camera_count}/${result.conflict.camera_limit}). ` +
					`Archive a camera from that user before reassigning.`;
			}
		} catch (e) {
			reassignError = e instanceof Error ? e.message : 'Reassign failed';
		} finally {
			reassignSaving = false;
		}
	}

	// Delete camera confirmation
	let deleteCameraConfirm = $state<AdminCamera | null>(null);

	async function submitDeleteCamera(cam: AdminCamera) {
		cameraActionInFlight = cam.device_id;
		try {
			await adminDeleteCamera(cam.device_id);
			deleteCameraConfirm = null;
			await loadCameras();
		} catch (e) {
			camerasError = e instanceof Error ? e.message : 'Delete failed';
		} finally {
			cameraActionInFlight = null;
		}
	}

	// Filter: which owner's cameras to show (empty = all)
	let cameraOwnerFilter = $state<string>('');
	const filteredCameras = $derived<AdminCamera[]>(
		cameraOwnerFilter === ''
			? cameras
			: cameras.filter((c) => c.user_id === cameraOwnerFilter),
	);

	// Formatter helpers
	function formatDate(ts: number | null | undefined): string {
		if (ts == null || ts === 0) return '—';
		return new Date(ts * 1000).toLocaleDateString();
	}
	function formatLastSeen(ts: number | null | undefined): string {
		if (ts == null || ts === 0) return 'never';
		const secs = Math.floor(Date.now() / 1000) - ts;
		if (secs < 60) return `${secs}s ago`;
		if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
		if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
		return `${Math.floor(secs / 86400)}d ago`;
	}

	onMount(() => {
		load();
		loadUsers();
		loadCameras();
	});
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
							<div class="flex items-center justify-between gap-3">
								<div class="text-[11px] text-muted-foreground font-mono truncate min-w-0">{tier.price_id}</div>
								{#if tier.configured}
									<span class="shrink-0 inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-primary/15 text-primary">
										<CheckCircle2 class="h-3 w-3" />
										Configured
									</span>
								{:else}
									<span class="shrink-0 inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-warning/15 text-warning">
										<AlertTriangle class="h-3 w-3" />
										Unconfigured
									</span>
								{/if}
							</div>

							<div class="grid grid-cols-2 gap-3">
								<label class="block">
									<span class="text-xs text-muted-foreground block mb-1">Tier name</span>
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
								<label class="block">
									<span class="text-xs text-muted-foreground block mb-1">
										Price ({tier.currency.toUpperCase()}/{tier.interval})
									</span>
									<input
										type="text"
										inputmode="decimal"
										placeholder="29.99"
										value={draft?.priceDollarsText ?? ''}
										disabled={draft?.saving}
										oninput={(e) => {
											if (draft) {
												drafts[tier.price_id] = { ...draft, priceDollarsText: e.currentTarget.value };
												markDirty(tier.price_id);
											}
										}}
										class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
									/>
								</label>
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

							<div class="flex items-center justify-end gap-2 flex-wrap">
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

		<!-- Users section -->
		<section class="rounded-lg border bg-card">
			<div class="flex items-center justify-between gap-3 px-4 py-3 border-b">
				<div class="min-w-0">
					<h2 class="text-sm font-semibold flex items-center gap-1.5">
						<Users class="h-3.5 w-3.5" />
						Users
					</h2>
					<p class="text-xs text-muted-foreground mt-0.5">
						Everyone on the platform. Admin grants and hard deletes are
						DB-only operations by design.
					</p>
				</div>
				<div class="flex items-center gap-1 shrink-0">
					<Button variant="outline" size="sm" onclick={openCreateUser} title="Create a new user">
						<Plus class="h-3.5 w-3.5 mr-1" />
						New user
					</Button>
					<Button
						variant="ghost"
						size="icon"
						onclick={loadUsers}
						title="Reload"
						disabled={usersLoading}
					>
						<RefreshCw class={cn('h-4 w-4', usersLoading && 'animate-spin')} />
					</Button>
				</div>
			</div>

			{#if usersLoading && users.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">Loading…</div>
			{:else if usersError}
				<div class="p-6 space-y-3">
					<p class="text-sm text-destructive">{usersError}</p>
					<Button variant="outline" size="sm" onclick={loadUsers}>Retry</Button>
				</div>
			{:else if users.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">No users.</div>
			{:else}
				<div class="divide-y">
					{#each users as user (user.user_id)}
						{@const isDisabled = user.disabled_at != null}
						{@const isDeleted = user.deleted_at != null}
						<div class="p-4 flex items-start justify-between gap-3 flex-wrap">
							<div class="min-w-0 flex-1">
								<div class="flex items-center gap-2 flex-wrap">
									<span class="text-sm font-medium truncate">{user.email}</span>
									{#if user.is_admin}
										<span class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-primary/15 text-primary">
											<Shield class="h-3 w-3" />
											Admin
										</span>
									{/if}
									{#if isDeleted}
										<span class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-destructive/15 text-destructive">
											<Trash2 class="h-3 w-3" />
											Deleted
										</span>
									{:else if isDisabled}
										<span class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded bg-warning/15 text-warning">
											<Ban class="h-3 w-3" />
											Disabled
										</span>
									{/if}
								</div>
								<div class="text-xs text-muted-foreground mt-0.5 flex items-center gap-3 flex-wrap">
									<span class="font-mono text-[11px]">{user.user_id}</span>
									<span>tier: <span class="font-mono">{user.tier}</span></span>
									<span>{user.camera_count} camera{user.camera_count === 1 ? '' : 's'}</span>
									<span>created {formatDate(user.created_at)}</span>
								</div>
							</div>
							<div class="flex items-center gap-1.5 shrink-0 flex-wrap justify-end">
								{#if !isDeleted}
									<Button
										variant="outline"
										size="sm"
										onclick={() => toggleDisabled(user)}
										disabled={userActionInFlight === user.user_id}
										title={isDisabled ? 'Re-enable login' : 'Disable login'}
									>
										{#if isDisabled}
											<Undo2 class="h-3.5 w-3.5 mr-1.5" />
											Enable
										{:else}
											<Ban class="h-3.5 w-3.5 mr-1.5" />
											Disable
										{/if}
									</Button>
									<Button
										variant="outline"
										size="sm"
										onclick={() => resetPassword(user)}
										disabled={userActionInFlight === user.user_id}
										title="Generate a new password for this user"
									>
										<KeyRound class="h-3.5 w-3.5 mr-1.5" />
										Reset password
									</Button>
									<Button
										variant="outline"
										size="sm"
										onclick={() => (deleteUserConfirm = user)}
										disabled={userActionInFlight === user.user_id || user.is_admin}
										title={user.is_admin
											? 'Demote this admin via DB before deleting'
											: 'Soft-delete this user'}
									>
										<Trash2 class="h-3.5 w-3.5 mr-1.5" />
										Delete
									</Button>
								{/if}
							</div>
						</div>
					{/each}
				</div>
			{/if}
		</section>

		<!-- Cameras section -->
		<section class="rounded-lg border bg-card">
			<div class="flex items-center justify-between gap-3 px-4 py-3 border-b">
				<div class="min-w-0">
					<h2 class="text-sm font-semibold flex items-center gap-1.5">
						<CameraIcon class="h-3.5 w-3.5" />
						Cameras
					</h2>
					<p class="text-xs text-muted-foreground mt-0.5">
						Every camera across every user. Reassigning respects the target
						user's tier limit.
					</p>
				</div>
				<div class="flex items-center gap-2 shrink-0">
					<select
						class="rounded-md border bg-transparent px-2 py-1 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						value={cameraOwnerFilter}
						onchange={(e) => (cameraOwnerFilter = e.currentTarget.value)}
						title="Filter by owner"
					>
						<option value="">All owners</option>
						{#each users as user (user.user_id)}
							{#if user.camera_count > 0}
								<option value={user.user_id}>{user.email}</option>
							{/if}
						{/each}
					</select>
					<Button
						variant="ghost"
						size="icon"
						onclick={loadCameras}
						title="Reload"
						disabled={camerasLoading}
					>
						<RefreshCw class={cn('h-4 w-4', camerasLoading && 'animate-spin')} />
					</Button>
				</div>
			</div>

			{#if camerasLoading && cameras.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">Loading…</div>
			{:else if camerasError}
				<div class="p-6 space-y-3">
					<p class="text-sm text-destructive">{camerasError}</p>
					<Button variant="outline" size="sm" onclick={loadCameras}>Retry</Button>
				</div>
			{:else if filteredCameras.length === 0}
				<div class="p-8 text-center text-sm text-muted-foreground">
					{cameraOwnerFilter
						? 'No cameras for that owner.'
						: 'No cameras enrolled yet.'}
				</div>
			{:else}
				<div class="divide-y">
					{#each filteredCameras as cam (cam.device_id)}
						<div class="p-4 flex items-start justify-between gap-3 flex-wrap">
							<div class="min-w-0 flex-1">
								<div class="text-sm font-medium truncate">
									{cam.display_name || cam.device_id}
								</div>
								<div class="text-xs text-muted-foreground mt-0.5 flex items-center gap-3 flex-wrap">
									<span class="font-mono text-[11px]">{cam.device_id}</span>
									<span>owner: {cam.owner_email}</span>
									<span>last seen: {formatLastSeen(cam.last_seen_at)}</span>
								</div>
							</div>
							<div class="flex items-center gap-1.5 shrink-0 flex-wrap justify-end">
								<Button
									variant="outline"
									size="sm"
									onclick={() => openReassign(cam)}
									disabled={cameraActionInFlight === cam.device_id}
									title="Reassign to a different user"
								>
									<ArrowRightLeft class="h-3.5 w-3.5 mr-1.5" />
									Reassign
								</Button>
								<Button
									variant="outline"
									size="sm"
									onclick={() => (deleteCameraConfirm = cam)}
									disabled={cameraActionInFlight === cam.device_id}
									title="Delete this camera"
								>
									<Trash2 class="h-3.5 w-3.5 mr-1.5" />
									Delete
								</Button>
							</div>
						</div>
					{/each}
				</div>
			{/if}
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

<!-- Create user dialog -->
<Dialog bind:open={createUserOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>New user</DialogTitle>
			<DialogDescription>
				Creates a free-tier account. The server generates the initial
				password — copy it out once from the next dialog and hand it off
				to the user.
			</DialogDescription>
		</DialogHeader>
		<form
			class="space-y-3 mt-2"
			onsubmit={(e) => {
				e.preventDefault();
				submitCreateUser();
			}}
		>
			<label class="block">
				<span class="text-xs text-muted-foreground block mb-1">Email</span>
				<input
					type="email"
					value={createUserForm.email}
					oninput={(e) => (createUserForm.email = e.currentTarget.value)}
					placeholder="user@example.com"
					required
					class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</label>
			<label class="block">
				<span class="text-xs text-muted-foreground block mb-1">Display name (optional)</span>
				<input
					type="text"
					value={createUserForm.displayName}
					oninput={(e) => (createUserForm.displayName = e.currentTarget.value)}
					placeholder="Defaults to the email address"
					class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</label>

			{#if createUserError}
				<p class="text-xs text-destructive break-words">{createUserError}</p>
			{/if}

			<div class="flex justify-end gap-2 pt-2">
				<Button
					type="button"
					variant="outline"
					onclick={() => (createUserOpen = false)}
					disabled={createUserSaving}
				>
					Cancel
				</Button>
				<Button type="submit" disabled={createUserSaving}>
					{createUserSaving ? 'Creating…' : 'Create user'}
				</Button>
			</div>
		</form>
	</DialogContent>
</Dialog>

<!-- One-time password reveal dialog (shared between create + reset) -->
{#if passwordReveal}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) passwordReveal = null; }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<KeyRound class="h-4 w-4" />
					{passwordReveal.context === 'create' ? 'User created' : 'Password reset'}
				</DialogTitle>
				<DialogDescription>
					This is the only time this password will be shown. Copy it now
					and hand it off securely.
				</DialogDescription>
			</DialogHeader>
			<div class="space-y-3 mt-2">
				<div>
					<span class="text-xs text-muted-foreground block mb-1">Email</span>
					<div class="rounded-md border bg-muted/30 px-3 py-1.5 text-sm font-mono break-all">
						{passwordReveal.email}
					</div>
				</div>
				<div>
					<span class="text-xs text-muted-foreground block mb-1">Password</span>
					<div class="flex items-stretch gap-2">
						<div class="flex-1 rounded-md border bg-muted/30 px-3 py-1.5 text-sm font-mono break-all">
							{passwordReveal.password}
						</div>
						<Button
							type="button"
							variant="outline"
							size="sm"
							onclick={copyRevealedPassword}
							title="Copy to clipboard"
						>
							<Copy class="h-3.5 w-3.5 mr-1.5" />
							Copy
						</Button>
					</div>
					{#if copyFeedback}
						<p class="text-xs text-muted-foreground mt-1">{copyFeedback}</p>
					{/if}
				</div>
				<div class="flex justify-end pt-2">
					<Button type="button" onclick={() => (passwordReveal = null)}>
						Done
					</Button>
				</div>
			</div>
		</DialogContent>
	</Dialog>
{/if}

<!-- Delete user confirmation -->
{#if deleteUserConfirm}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) deleteUserConfirm = null; }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<AlertTriangle class="h-4 w-4 text-warning" />
					Soft-delete user?
				</DialogTitle>
				<DialogDescription>
					<span class="font-semibold">{deleteUserConfirm.email}</span>
					will be marked as deleted and disabled. Their cameras will stop
					uploading and their Stripe subscription will be canceled. The
					row stays in the database until a developer hard-deletes it via
					psql.
				</DialogDescription>
			</DialogHeader>
			<div class="flex justify-end gap-2 mt-4">
				<Button
					type="button"
					variant="outline"
					onclick={() => (deleteUserConfirm = null)}
					disabled={userActionInFlight !== null}
				>
					Cancel
				</Button>
				<Button
					type="button"
					onclick={() => {
						if (deleteUserConfirm) submitDeleteUser(deleteUserConfirm);
					}}
					disabled={userActionInFlight !== null}
				>
					{userActionInFlight ? 'Deleting…' : 'Delete'}
				</Button>
			</div>
		</DialogContent>
	</Dialog>
{/if}

<!-- Reassign camera dialog -->
{#if reassignTarget}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) reassignTarget = null; }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<ArrowRightLeft class="h-4 w-4" />
					Reassign {reassignTarget.display_name || reassignTarget.device_id}
				</DialogTitle>
				<DialogDescription>
					Move this camera to a different user. The target user must have
					room under their tier's camera limit.
				</DialogDescription>
			</DialogHeader>
			<form
				class="space-y-3 mt-2"
				onsubmit={(e) => {
					e.preventDefault();
					submitReassign();
				}}
			>
				<div>
					<span class="text-xs text-muted-foreground block mb-1">Current owner</span>
					<div class="rounded-md border bg-muted/30 px-3 py-1.5 text-sm">
						{reassignTarget.owner_email}
					</div>
				</div>
				<label class="block">
					<span class="text-xs text-muted-foreground block mb-1">New owner</span>
					<select
						value={reassignToUserID}
						onchange={(e) => (reassignToUserID = e.currentTarget.value)}
						class="w-full rounded-md border bg-transparent px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					>
						{#each users as user (user.user_id)}
							{#if user.deleted_at == null}
								<option value={user.user_id}>{user.email}</option>
							{/if}
						{/each}
					</select>
				</label>

				{#if reassignError}
					<p class="text-xs text-destructive break-words">{reassignError}</p>
				{/if}

				<div class="flex justify-end gap-2 pt-2">
					<Button
						type="button"
						variant="outline"
						onclick={() => (reassignTarget = null)}
						disabled={reassignSaving}
					>
						Cancel
					</Button>
					<Button type="submit" disabled={reassignSaving}>
						{reassignSaving ? 'Moving…' : 'Reassign'}
					</Button>
				</div>
			</form>
		</DialogContent>
	</Dialog>
{/if}

<!-- Delete camera confirmation -->
{#if deleteCameraConfirm}
	<Dialog
		bind:open={
			() => true,
			(v) => { if (!v) deleteCameraConfirm = null; }
		}
	>
		<DialogContent>
			<DialogHeader>
				<DialogTitle class="flex items-center gap-2">
					<AlertTriangle class="h-4 w-4 text-warning" />
					Delete camera?
				</DialogTitle>
				<DialogDescription>
					<span class="font-semibold">{deleteCameraConfirm.display_name || deleteCameraConfirm.device_id}</span>
					will be removed from the database. Segments already in S3 will
					be cleaned up on the next retention prune.
				</DialogDescription>
			</DialogHeader>
			<div class="flex justify-end gap-2 mt-4">
				<Button
					type="button"
					variant="outline"
					onclick={() => (deleteCameraConfirm = null)}
					disabled={cameraActionInFlight !== null}
				>
					Cancel
				</Button>
				<Button
					type="button"
					onclick={() => {
						if (deleteCameraConfirm) submitDeleteCamera(deleteCameraConfirm);
					}}
					disabled={cameraActionInFlight !== null}
				>
					{cameraActionInFlight ? 'Deleting…' : 'Delete'}
				</Button>
			</div>
		</DialogContent>
	</Dialog>
{/if}
