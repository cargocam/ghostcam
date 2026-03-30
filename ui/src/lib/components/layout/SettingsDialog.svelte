<script lang="ts">
	import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from '$lib/components/ui/sheet/index.js';
	import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '$lib/components/ui/dialog/index.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { billingStore } from '$lib/stores/billing.svelte.js';
	import { Sun, Moon, Monitor, Bug, CreditCard, ExternalLink, AlertTriangle } from 'lucide-svelte';

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
	let hasPricingTable = $derived(billingStore.stripePublicKey && billingStore.stripePricingTableId);
	let upgradeOpen = $state(false);

	// Load Stripe Pricing Table script once
	let stripeScriptLoaded = $state(false);
	$effect(() => {
		if (hasPricingTable && !stripeScriptLoaded) {
			if (!document.querySelector('script[src*="pricing-table.js"]')) {
				const script = document.createElement('script');
				script.src = 'https://js.stripe.com/v3/pricing-table.js';
				script.async = true;
				document.head.appendChild(script);
			}
			stripeScriptLoaded = true;
		}
	});
</script>

<Sheet bind:open>
	<SheetContent side="right">
		<SheetHeader>
			<SheetTitle>Settings</SheetTitle>
			<SheetDescription>Viewer preferences</SheetDescription>
		</SheetHeader>

		<div class="mt-6 space-y-6 overflow-y-auto" style="max-height: calc(100vh - 8rem);">
			<!-- Theme -->
			<div>
				<h3 class="text-sm font-medium mb-3">Theme</h3>
				<div class="flex gap-2">
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

			<!-- Debug mode -->
			<div>
				<h3 class="text-sm font-medium mb-3">Developer</h3>
				<Button
					variant={settingsStore.debugMode ? 'default' : 'outline'}
					size="sm"
					onclick={() => (settingsStore.debugMode = !settingsStore.debugMode)}
				>
					<Bug class="h-4 w-4 mr-1.5" />
					Debug Overlay
				</Button>
				<p class="text-xs text-muted-foreground mt-1.5">Show WebRTC stats on camera cards</p>
			</div>

			<Separator />

			<!-- Billing -->
			{#if billingStore.billingEnabled}
				<div>
					<h3 class="text-sm font-medium mb-3 flex items-center gap-1.5">
						<CreditCard class="h-4 w-4" />
						Billing
					</h3>

					{#if billingStore.isPastDue}
						<div class="flex items-center gap-2 px-3 py-2 mb-3 rounded-md bg-destructive/10 text-destructive text-xs">
							<AlertTriangle class="h-3.5 w-3.5 flex-shrink-0" />
							<span>
								Payment past due.
								{#if billingStore.subscription?.grace_expires_at}
									Service suspended after {new Date(billingStore.subscription.grace_expires_at * 1000).toLocaleDateString()}.
								{/if}
							</span>
						</div>
					{/if}

					{#if billingStore.isSuspended}
						<div class="flex items-center gap-2 px-3 py-2 mb-3 rounded-md bg-destructive/10 text-destructive text-xs">
							<AlertTriangle class="h-3.5 w-3.5 flex-shrink-0" />
							<span>Account suspended. Update payment to restore access.</span>
						</div>
					{/if}

					<!-- Current plan + usage -->
					<div class="mb-3">
						<span class="text-sm font-medium capitalize">{billingStore.currentTier}</span>
						{#if billingStore.usage}
							<span class="text-xs text-muted-foreground ml-1.5">
								{billingStore.usage.cameras_count}{billingStore.usage.camera_limit !== null ? `/${billingStore.usage.camera_limit}` : ''} cameras
							</span>
						{/if}
					</div>

					<!-- Actions -->
					{#if isFree}
						<Button class="w-full" onclick={() => upgradeOpen = true}>
							Upgrade
						</Button>
					{:else}
						<Button variant="outline" class="w-full" onclick={() => billingStore.openPortal()}>
							Manage Subscription
							<ExternalLink class="h-3.5 w-3.5 ml-1.5" />
						</Button>
					{/if}
				</div>

				<Separator />
			{/if}

			<!-- Connection status -->
			<div>
				<h3 class="text-sm font-medium mb-2">Connection</h3>
				<div class="flex items-center gap-2 text-sm">
					<span class={transportStore.connected ? 'text-primary' : 'text-destructive'}>
						{transportStore.connected ? 'Connected' : 'Disconnected'}
					</span>
					<span class="text-xs text-muted-foreground">({transportStore.connectionState})</span>
					{#if transportStore.error}
						<span class="text-xs text-destructive">{transportStore.error}</span>
					{/if}
				</div>
			</div>
		</div>
	</SheetContent>
</Sheet>

{#if hasPricingTable}
	<Dialog bind:open={upgradeOpen}>
		<DialogContent style="max-width: min(1100px, 90vw);">
			<DialogHeader>
				<DialogTitle>Choose a plan</DialogTitle>
				<DialogDescription>Select a plan to upgrade your account.</DialogDescription>
			</DialogHeader>
			<div class="mt-2">
				{@html `<stripe-pricing-table pricing-table-id="${billingStore.stripePricingTableId}" publishable-key="${billingStore.stripePublicKey}" theme="night"></stripe-pricing-table>`}
			</div>
		</DialogContent>
	</Dialog>
{/if}
