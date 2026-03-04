<script lang="ts">
	import { untrack } from 'svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import Sparkline from './Sparkline.svelte';
	import { cn } from '$lib/utils.js';

	let { sourceId }: { sourceId: string } = $props();

	// Keep recent history for sparklines
	const MAX_HISTORY = 60;
	let cpuHistory = $state<number[]>([]);
	let memHistory = $state<number[]>([]);

	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === sourceId));
	let telemetry = $derived(camera?.telemetry ?? null);

	// Track history as telemetry updates
	$effect(() => {
		if (!telemetry) return;
		const cpu = telemetry.cpu_percent;
		const mem = telemetry.memory_mb;
		untrack(() => {
			cpuHistory = [...cpuHistory.slice(-(MAX_HISTORY - 1)), cpu];
			memHistory = [...memHistory.slice(-(MAX_HISTORY - 1)), mem];
		});
	});

	function formatUptime(secs: number): string {
		const h = Math.floor(secs / 3600);
		const m = Math.floor((secs % 3600) / 60);
		if (h > 24) {
			const d = Math.floor(h / 24);
			return `${d}d ${h % 24}h`;
		}
		return `${h}h ${m}m`;
	}

	function statusColor(value: number, warn: number, crit: number): string {
		if (value >= crit) return 'text-destructive';
		if (value >= warn) return 'text-warning';
		return 'text-primary';
	}
</script>

{#if telemetry}
	<div class="space-y-3 px-4 pb-4">
		<!-- CPU + sparkline -->
		<div class="flex items-center gap-2">
			<span class="text-[10px] uppercase tracking-wider text-muted-foreground w-8 shrink-0">CPU</span>
			<Sparkline data={cpuHistory} height={18} class="flex-1" />
			<span class={cn("text-xs font-mono font-medium shrink-0", statusColor(telemetry.cpu_percent, 70, 90))}>
				{telemetry.cpu_percent.toFixed(1)}%
			</span>
		</div>

		<!-- Memory + sparkline -->
		<div class="flex items-center gap-2">
			<span class="text-[10px] uppercase tracking-wider text-muted-foreground w-8 shrink-0">MEM</span>
			<Sparkline data={memHistory} height={18} class="flex-1" />
			<span class="text-xs font-mono font-medium shrink-0">
				{telemetry.memory_mb.toFixed(0)} MB
			</span>
		</div>

		<!-- Stats grid -->
		<div class="grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs">
			{#if telemetry.temp_celsius > 0}
				<div class="flex justify-between">
					<span class="text-muted-foreground">Temp</span>
					<span class={cn("font-mono", statusColor(telemetry.temp_celsius, 70, 85))}>
						{telemetry.temp_celsius.toFixed(1)}&deg;C
					</span>
				</div>
			{/if}

			<div class="flex justify-between">
				<span class="text-muted-foreground">Uptime</span>
				<span class="font-mono">{formatUptime(telemetry.uptime_secs)}</span>
			</div>
		</div>
	</div>
{:else}
	<div class="px-4 py-6 text-xs text-muted-foreground text-center">
		Waiting for telemetry...
	</div>
{/if}
