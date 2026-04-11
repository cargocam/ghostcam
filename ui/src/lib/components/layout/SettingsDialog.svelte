<script lang="ts">
	import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from '$lib/components/ui/sheet/index.js';
	import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '$lib/components/ui/dialog/index.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import {
		billingStore,
		formatCameraLimit,
		formatStorageLimit,
		formatTierPrice,
	} from '$lib/stores/billing.svelte.js';
	import { devStore } from '$lib/stores/dev.svelte.js';
	import { authStore } from '$lib/stores/auth.svelte.js';
	import { changePassword } from '$lib/auth.js';
	import { Sun, Moon, Monitor, CreditCard, ExternalLink, Trash2, Bug, RefreshCw, Shield } from 'lucide-svelte';

	let {
		open = $bindable(false),
	}: {
		open?: boolean;
	} = $props();

	// Refresh billing data each time settings opens
	$effect(() => {
		if (open) {
			billingStore.load();
		}
	});

	let isFree = $derived(billingStore.currentTier === 'free');
	let paidTiers = $derived(billingStore.paidTiers);

	// Change email dialog state (stubbed — not yet implemented on the backend)
	let changeEmailOpen = $state(false);
	// Delete account dialog state (stubbed — not yet implemented on the backend)
	let deleteAccountOpen = $state(false);

	// Change password dialog state
	let changePasswordOpen = $state(false);
	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let changePasswordError = $state('');
	let changePasswordSubmitting = $state(false);
	let changePasswordSuccess = $state(false);

	function resetChangePasswordForm() {
		currentPassword = '';
		newPassword = '';
		confirmPassword = '';
		changePasswordError = '';
		changePasswordSubmitting = false;
		changePasswordSuccess = false;
	}

	$effect(() => {
		if (!changePasswordOpen) resetChangePasswordForm();
	});

	async function handleChangePassword(e: SubmitEvent) {
		e.preventDefault();
		changePasswordError = '';
		if (newPassword !== confirmPassword) {
			changePasswordError = 'New passwords do not match';
			return;
		}
		if (newPassword.length < 8 || newPassword.length > 128) {
			changePasswordError = 'Password must be 8-128 characters';
			return;
		}
		changePasswordSubmitting = true;
		const result = await changePassword(currentPassword, newPassword);
		changePasswordSubmitting = false;
		if (!result.ok) {
			changePasswordError = result.error ?? 'Failed to change password';
			return;
		}
		authStore.refresh();
		changePasswordSuccess = true;
		setTimeout(() => (changePasswordOpen = false), 1200);
	}

</script>

<Sheet bind:open>
	<SheetContent side="right">
		<SheetHeader>
			<SheetTitle>Settings</SheetTitle>
			<SheetDescription>Viewer preferences</SheetDescription>
		</SheetHeader>

		<div class="mt-6 space-y-6 overflow-y-auto pb-4" style="max-height: calc(100vh - 10rem);">
			<!-- Theme -->
			<div>
				<h3 class="text-sm font-medium mb-3">Theme</h3>
				<div class="flex flex-wrap gap-2">
					<Button
						variant={settingsStore.theme === 'light' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('light')}
					>
						<Sun class="h-4 w-4 mr-1.5" />
						Light
					</Button>
					<Button
						variant={settingsStore.theme === 'dark' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('dark')}
					>
						<Moon class="h-4 w-4 mr-1.5" />
						Dark
					</Button>
					<Button
						variant={settingsStore.theme === 'system' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('system')}
					>
						<Monitor class="h-4 w-4 mr-1.5" />
						System
					</Button>
				</div>
			</div>

			<Separator />

			<!-- Billing -->
			{#if billingStore.loading}
				<div>
					<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
						<CreditCard class="h-4 w-4" />
						Billing
					</h3>
					<!-- Skeleton: same visual footprint as the loaded state
					     so the section doesn't jump around once it settles. -->
					<div class="space-y-2">
						<div class="h-4 w-24 rounded bg-muted animate-pulse"></div>
						<div class="h-2 w-full rounded bg-muted animate-pulse"></div>
						<div class="h-20 w-full rounded-md border bg-muted/40 animate-pulse"></div>
					</div>
				</div>
				<Separator />
			{:else if billingStore.loadError}
				<div>
					<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
						<CreditCard class="h-4 w-4" />
						Billing
					</h3>
					<p class="text-xs text-muted-foreground mb-2">
						{billingStore.loadError}
					</p>
					<Button
						variant="outline"
						size="sm"
						disabled={billingStore.loading}
						onclick={() => billingStore.load(true)}
					>
						<RefreshCw class="h-3.5 w-3.5 mr-1.5" />
						{billingStore.loading ? 'Refreshing…' : 'Retry'}
					</Button>
				</div>
				<Separator />
			{:else if billingStore.billingEnabled && billingStore.tiersLoaded}
				<div>
					<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
						<CreditCard class="h-4 w-4" />
						Billing
					</h3>

					<!-- Current plan + usage -->
					<div class="mb-3">
						<span class="text-sm font-medium">{billingStore.currentTierName}</span>
						{#if billingStore.usage}
							<span class="text-xs text-muted-foreground ml-1.5">
								{billingStore.usage.cameras_count}{billingStore.usage.camera_limit !== null ? `/${billingStore.usage.camera_limit}` : ''} cameras
							</span>
						{/if}
					</div>

					<!-- Storage usage bar -->
					{#if billingStore.usage}
						<div class="mb-3">
							<div class="flex justify-between text-xs text-muted-foreground mb-1">
								<span>Storage</span>
								<span>
									{billingStore.storageUsedGB.toFixed(2)} GB
									{#if billingStore.storageLimitGB != null}
										/ {billingStore.storageLimitGB} GB
									{:else}
										(unlimited)
									{/if}
								</span>
							</div>
							{#if billingStore.storageLimitGB != null}
								<div class="h-2 rounded-full bg-muted overflow-hidden">
									<div
										class="h-full rounded-full transition-all {billingStore.isStorageCapped ? 'bg-destructive' : billingStore.storagePercent > 80 ? 'bg-amber-500' : 'bg-primary'}"
										style="width: {billingStore.storagePercent}%"
									></div>
								</div>
								{#if billingStore.isStorageCapped}
									<p class="text-xs text-destructive mt-1">Storage full. Camera uploads paused.</p>
								{/if}
							{/if}
						</div>
					{/if}

					<!-- Actions -->
					{#if isFree}
						<p class="text-xs text-muted-foreground mb-2">Upgrade to a paid plan</p>
						<div class="space-y-2">
							{#each paidTiers as tier (tier.id)}
								<div class="rounded-md border p-3">
									<div class="flex items-center justify-between gap-2 mb-1">
										<span class="text-sm font-medium">{tier.name}</span>
										<span class="text-xs text-muted-foreground whitespace-nowrap">
											{formatTierPrice(tier)}
										</span>
									</div>
									<div class="text-[11px] text-muted-foreground mb-2">
										{formatCameraLimit(tier)} · {formatStorageLimit(tier)}
									</div>
									<Button
										size="sm"
										class="w-full"
										disabled={billingStore.actionInFlight}
										onclick={() => billingStore.checkout(tier.id)}
									>
										{billingStore.actionInFlight ? 'Opening…' : `Choose ${tier.name}`}
										<ExternalLink class="h-3.5 w-3.5 ml-1.5" />
									</Button>
								</div>
							{/each}
						</div>
					{:else}
						<Button
							variant="outline"
							class="w-full"
							disabled={billingStore.actionInFlight}
							onclick={() => billingStore.openPortal()}
						>
							{billingStore.actionInFlight ? 'Opening…' : 'Manage Subscription'}
							<ExternalLink class="h-3.5 w-3.5 ml-1.5" />
						</Button>
					{/if}
					{#if billingStore.error}
						<p class="text-xs text-destructive mt-2 break-words">{billingStore.error}</p>
					{/if}
				</div>

				<Separator />
			{/if}

			<!-- Admin: visible only for users with a row in the admins
			     table. Status comes from GET /api/v1/auth/me and stays
			     null until the fetch resolves, so this block is hidden
			     briefly on first load (acceptable — admin panel is rare). -->
			{#if authStore.isAdmin === true}
				<div>
					<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
						<Shield class="h-4 w-4" />
						Admin
					</h3>
					<Button
						variant="outline"
						size="sm"
						class="w-full"
						onclick={() => {
							settingsStore.setView('admin');
							open = false;
						}}
					>
						Open admin panel
					</Button>
				</div>

				<Separator />
			{/if}

			<!-- Developer -->
			<div>
				<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
					<Bug class="h-4 w-4" />
					Developer
				</h3>
				<label class="flex items-start gap-3 text-sm cursor-pointer">
					<input
						type="checkbox"
						checked={devStore.clientLogging}
						onchange={(e) => devStore.setClientLogging(e.currentTarget.checked)}
						class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
					/>
					<span class="flex-1">
						<span class="font-medium block">Client error logging</span>
						<span class="block text-xs text-muted-foreground mt-0.5">
							Forward video playback and HLS errors to the server log for
							debugging. Only useful if you're actively diagnosing a problem.
							Off by default.
						</span>
					</span>
				</label>
			</div>

			<Separator />

			<!-- Account -->
			<div>
				<h3 class="text-sm font-medium mb-3">Account</h3>
				<div class="space-y-2 text-sm">
					<div class="flex items-center gap-2">
						<span class="text-muted-foreground truncate" title={authStore.email}>
							{authStore.email || '—'}
						</span>
						<button
							type="button"
							class="hover:underline underline-offset-4"
							onclick={() => (changeEmailOpen = true)}
						>
							update
						</button>
					</div>
					<div>
						<button
							type="button"
							class="hover:underline underline-offset-4"
							onclick={() => (changePasswordOpen = true)}
						>
							change password
						</button>
					</div>
					<div>
						<button
							type="button"
							class="text-destructive hover:underline underline-offset-4"
							onclick={() => (deleteAccountOpen = true)}
						>
							delete account
						</button>
					</div>
					<div>
						<button
							type="button"
							class="text-destructive hover:underline underline-offset-4"
							onclick={async () => {
								await transportStore.logout();
								open = false;
							}}
						>
							log out
						</button>
					</div>
				</div>
			</div>
		</div>

		<!-- Connection status (floating footer) -->
		<div class="absolute inset-x-0 bottom-0 border-t bg-background px-6 py-2 flex items-center gap-1.5 text-xs">
			<span
				class="h-1.5 w-1.5 rounded-full {transportStore.connected ? 'bg-primary' : 'bg-destructive'}"
				aria-hidden="true"
			></span>
			<span class={transportStore.connected ? 'text-muted-foreground' : 'text-destructive'}>
				{transportStore.connected
					? 'Connected'
					: transportStore.connectionState === 'unauthenticated'
						? 'Unauthenticated'
						: 'Disconnected'}
			</span>
			{#if transportStore.error}
				<span class="text-destructive truncate" title={transportStore.error}>· {transportStore.error}</span>
			{/if}
		</div>
	</SheetContent>
</Sheet>

<Dialog bind:open={changePasswordOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Change password</DialogTitle>
			<DialogDescription>Enter your current password and choose a new one.</DialogDescription>
		</DialogHeader>
		<form onsubmit={handleChangePassword} class="space-y-4 mt-2">
			<div>
				<label for="cp-current" class="text-xs text-muted-foreground mb-1 block">Current password</label>
				<input
					id="cp-current"
					type="password"
					autocomplete="current-password"
					bind:value={currentPassword}
					required
					class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</div>
			<div>
				<label for="cp-new" class="text-xs text-muted-foreground mb-1 block">New password</label>
				<input
					id="cp-new"
					type="password"
					autocomplete="new-password"
					bind:value={newPassword}
					required
					minlength="8"
					maxlength="128"
					class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</div>
			<div>
				<label for="cp-confirm" class="text-xs text-muted-foreground mb-1 block">Confirm new password</label>
				<input
					id="cp-confirm"
					type="password"
					autocomplete="new-password"
					bind:value={confirmPassword}
					required
					minlength="8"
					maxlength="128"
					class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				/>
			</div>
			{#if changePasswordError}
				<p class="text-sm text-destructive">{changePasswordError}</p>
			{/if}
			{#if changePasswordSuccess}
				<p class="text-sm text-primary">Password updated.</p>
			{/if}
			<div class="flex justify-end gap-2">
				<Button
					type="button"
					variant="outline"
					onclick={() => (changePasswordOpen = false)}
					disabled={changePasswordSubmitting}
				>
					Cancel
				</Button>
				<Button
					type="submit"
					disabled={changePasswordSubmitting || !currentPassword || !newPassword || !confirmPassword}
				>
					{changePasswordSubmitting ? 'Saving...' : 'Save'}
				</Button>
			</div>
		</form>
	</DialogContent>
</Dialog>

<Dialog bind:open={changeEmailOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Update email</DialogTitle>
			<DialogDescription>
				Email changes aren't available yet. Contact support to update the email on your account.
			</DialogDescription>
		</DialogHeader>
		{#if authStore.email}
			<p class="text-xs text-muted-foreground mt-2">
				Current email: <span class="text-foreground">{authStore.email}</span>
			</p>
		{/if}
		<div class="flex justify-end mt-4">
			<Button type="button" onclick={() => (changeEmailOpen = false)}>Close</Button>
		</div>
	</DialogContent>
</Dialog>

<Dialog bind:open={deleteAccountOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle class="flex items-center gap-2">
				<Trash2 class="h-4 w-4 text-destructive" />
				Delete account
			</DialogTitle>
			<DialogDescription>
				Account deletion isn't available yet. Contact support to permanently delete your
				account and all associated data.
			</DialogDescription>
		</DialogHeader>
		<div class="flex justify-end mt-4">
			<Button type="button" onclick={() => (deleteAccountOpen = false)}>Close</Button>
		</div>
	</DialogContent>
</Dialog>

