<script lang="ts">
	import { billingStore } from '$lib/stores/billing.svelte.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { AlertTriangle, HardDrive, X } from 'lucide-svelte';

	/**
	 * Non-intrusive banner surfaced above the main view when the account's
	 * storage cap is either exhausted (HLS uploads paused) or close to it.
	 *
	 * WebRTC live streaming is NOT affected by storage caps — the cap only
	 * pauses segment uploads to S3, which feeds HLS recording/fallback. The
	 * copy below is careful to say "recording" rather than "live view" so
	 * users don't think their cameras have gone dark.
	 */

	interface Props {
		onUpgrade: () => void;
	}
	let { onUpgrade }: Props = $props();

	// A soft warning appears at >=85% so users see it coming before uploads
	// actually stop. The hard banner appears when the cap is reached.
	const WARN_PERCENT = 85;

	let isCapped = $derived(billingStore.isStorageCapped);
	let percent = $derived(billingStore.storagePercent);
	let inWarning = $derived(!isCapped && percent >= WARN_PERCENT);

	// Track which state the user has dismissed so the banner doesn't reappear
	// on every navigation within the same session. A fresh capped state (e.g.
	// after an upgrade rollback) will re-surface because we key by state.
	let dismissedWarning = $state(false);

	// When the situation changes from warning -> capped, clear any prior
	// dismissal so the harder banner is shown even if the warning was hidden.
	$effect(() => {
		if (isCapped) dismissedWarning = false;
	});

	let visible = $derived(isCapped || (inWarning && !dismissedWarning));
</script>

{#if visible}
	<div
		role="status"
		aria-live="polite"
		class="flex items-center gap-2 sm:gap-3 px-3 sm:px-4 py-2 border-b text-sm {isCapped
			? 'bg-destructive/10 border-destructive/30 text-destructive-foreground'
			: 'bg-amber-500/10 border-amber-500/30 text-foreground'}"
	>
		<span class="shrink-0 {isCapped ? 'text-destructive' : 'text-amber-600 dark:text-amber-400'}">
			{#if isCapped}
				<HardDrive class="h-4 w-4" />
			{:else}
				<AlertTriangle class="h-4 w-4" />
			{/if}
		</span>

		<div class="flex-1 min-w-0 truncate">
			{#if isCapped}
				<span class="font-medium">Storage full.</span>
				<span class="hidden sm:inline text-muted-foreground">
					Recording paused — live viewing still works. Upgrade to resume uploads.
				</span>
				<span class="sm:hidden text-muted-foreground">Recording paused. Live view still works.</span>
			{:else}
				<span class="font-medium">Storage at {Math.floor(percent)}%.</span>
				<span class="text-muted-foreground">
					Upgrade soon to keep recording.
				</span>
			{/if}
		</div>

		<Button
			size="sm"
			variant={isCapped ? 'default' : 'outline'}
			class="shrink-0 h-7 px-3 text-xs"
			onclick={onUpgrade}
		>
			Upgrade
		</Button>

		{#if !isCapped}
			<Button
				variant="ghost"
				size="icon"
				class="h-7 w-7 shrink-0"
				aria-label="Dismiss"
				onclick={() => (dismissedWarning = true)}
			>
				<X class="h-3.5 w-3.5" />
			</Button>
		{/if}
	</div>
{/if}
