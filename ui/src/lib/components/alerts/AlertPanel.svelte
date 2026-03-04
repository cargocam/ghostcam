<script lang="ts">
	import { alertsStore, type AlertType } from '$lib/stores/alerts.svelte.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { ScrollArea } from '$lib/components/ui/scroll-area/index.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { cn } from '$lib/utils.js';

	function alertIcon(type: AlertType): string {
		switch (type) {
			case 'disconnect':
				return '\u{1F534}';
			case 'reconnect':
				return '\u{1F7E2}';
			default:
				return '\u{2139}\u{FE0F}';
		}
	}

	function relativeTime(timestamp: number): string {
		const diff = Math.floor((Date.now() - timestamp) / 1000);
		if (diff < 5) return 'just now';
		if (diff < 60) return `${diff}s ago`;
		const mins = Math.floor(diff / 60);
		if (mins < 60) return `${mins}m ago`;
		const hours = Math.floor(mins / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		return `${days}d ago`;
	}

	// Re-compute relative times every 30s
	let tick = $state(0);
	$effect(() => {
		const interval = setInterval(() => {
			tick++;
		}, 30_000);
		return () => clearInterval(interval);
	});

	// _tick is unused inside the function but forces $derived to re-track it,
	// causing relativeTime values to recompute every 30s
	function getAlerts(_tick: number) {
		return alertsStore.alerts;
	}
	let alerts = $derived(getAlerts(tick));
</script>

<div class="flex flex-col h-full">
	<!-- Header -->
	<div class="flex items-center justify-between px-4 py-3">
		<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
			Alerts
			{#if alertsStore.unreadCount > 0}
				<span class="ml-1.5 text-destructive">{alertsStore.unreadCount}</span>
			{/if}
		</h2>
		<div class="flex items-center gap-1">
			{#if alertsStore.unreadCount > 0}
				<Button variant="ghost" size="sm" class="h-6 text-[10px] px-2" onclick={() => alertsStore.markAllRead()}>
					Mark all read
				</Button>
			{/if}
			{#if alertsStore.alerts.length > 0}
				<Button variant="ghost" size="sm" class="h-6 text-[10px] px-2 text-destructive hover:text-destructive" onclick={() => alertsStore.clearAll()}>
					Clear all
				</Button>
			{/if}
		</div>
	</div>

	<Separator />

	<!-- Alert list -->
	<ScrollArea class="flex-1 min-h-0">
		{#if alerts.length === 0}
			<div class="px-4 py-12 text-xs text-muted-foreground text-center">
				No alerts
			</div>
		{:else}
			<div class="flex flex-col">
				{#each alerts as alert (alert.id)}
					<div
						class={cn(
							"flex items-start gap-2.5 px-4 py-2.5 text-left w-full hover:bg-accent/30 transition-colors border-b border-border/50 cursor-pointer",
							!alert.read && "bg-accent/50"
						)}
						role="button"
						tabindex="0"
						onclick={() => alertsStore.markRead(alert.id)}
						onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') alertsStore.markRead(alert.id); }}
					>
						<span class="text-sm mt-0.5 shrink-0" role="img" aria-label={alert.type}>
							{alertIcon(alert.type)}
						</span>
						<div class="flex-1 min-w-0">
							<div class="flex items-center justify-between gap-2">
								<span class="text-xs font-medium truncate">{alert.cameraName}</span>
								<span class="text-[10px] text-muted-foreground shrink-0">{relativeTime(alert.timestamp)}</span>
							</div>
							<p class="text-[11px] text-muted-foreground mt-0.5 line-clamp-2">{alert.message}</p>
						</div>
						<button
							class="shrink-0 mt-0.5 text-muted-foreground hover:text-foreground text-xs leading-none p-0.5"
							onclick={(e) => { e.stopPropagation(); alertsStore.dismiss(alert.id); }}
							aria-label="Dismiss alert"
						>
							&#x2715;
						</button>
					</div>
				{/each}
			</div>
		{/if}
	</ScrollArea>
</div>
