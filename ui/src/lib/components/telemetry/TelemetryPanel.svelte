<script lang="ts">
	import { untrack } from 'svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import Sparkline from '$lib/components/ui/Sparkline.svelte';
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
		const cpu = telemetry.cpu_percent ?? 0;
		const mem = telemetry.memory_mb ?? 0;
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

	function formatBitrate(bytesPerSec: number): string {
		const kbps = (bytesPerSec * 8) / 1000;
		if (kbps >= 1000) return `${(kbps / 1000).toFixed(1)} Mbps`;
		return `${kbps.toFixed(0)} kbps`;
	}

	// Returns true if the telemetry carries any health signal worth
	// rendering. Avoids drawing an empty "Health" section on cameras
	// running an older firmware that doesn't emit these fields.
	function hasHealth(t: typeof telemetry): boolean {
		if (!t) return false;
		return (
			t.segment_upload_p95_ms != null ||
			(t.segment_queue_depth ?? 0) > 0 ||
			(t.segment_upload_retries ?? 0) > 0 ||
			(t.live_ws_dropped_frames ?? 0) > 0 ||
			(t.live_ws_bytes_per_sec ?? 0) > 0 ||
			t.disk_used_pct != null ||
			(t.network_recovery_attempts ?? 0) > 0 ||
			t.modem_rat != null ||
			(t.event_loop_lag_ms ?? 0) > 5 ||
			(t.gpsd_query_ms ?? 0) > 100
		);
	}
</script>

{#if telemetry}
	<div class="space-y-3 px-4 pb-4">
		<!-- CPU + sparkline -->
		<div class="flex items-center gap-2">
			<span class="text-[10px] uppercase tracking-wider text-muted-foreground w-8 shrink-0">CPU</span>
			<Sparkline data={cpuHistory} height={18} class="flex-1" />
			<span class={cn("text-xs font-mono font-medium shrink-0", statusColor(telemetry.cpu_percent ?? 0, 70, 90))}>
				{(telemetry.cpu_percent ?? 0).toFixed(1)}%
			</span>
		</div>

		<!-- Memory + sparkline -->
		<div class="flex items-center gap-2">
			<span class="text-[10px] uppercase tracking-wider text-muted-foreground w-8 shrink-0">MEM</span>
			<Sparkline data={memHistory} height={18} class="flex-1" />
			<span class="text-xs font-mono font-medium shrink-0">
				{(telemetry.memory_mb ?? 0).toFixed(0)} MB
			</span>
		</div>

		<!-- Stats grid -->
		<div class="grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs">
			{#if (telemetry.temp_celsius ?? 0) > 0}
				<div class="flex justify-between">
					<span class="text-muted-foreground">Temp</span>
					<span class={cn("font-mono", statusColor(telemetry.temp_celsius ?? 0, 70, 85))}>
						{(telemetry.temp_celsius ?? 0).toFixed(1)}&deg;C
					</span>
				</div>
			{/if}

			<div class="flex justify-between">
				<span class="text-muted-foreground">Uptime</span>
				<span class="font-mono">{formatUptime(telemetry.uptime_secs ?? 0)}</span>
			</div>
		</div>

		<!-- Health metrics. Surfaced selectively: most rows only render
			 when they carry a signal — non-zero retries, recoveries,
			 dropped frames — so a healthy camera shows a short, clean
			 tile. Always-on rows (disk, p95) only render when the field
			 is present at all. -->
		{#if hasHealth(telemetry)}
			<div class="border-t border-border/30 pt-3 space-y-1.5 text-xs">
				<div class="text-[10px] uppercase tracking-wider text-muted-foreground">Health</div>
				{#if telemetry.segment_upload_p95_ms != null}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Upload p95</span>
						<span class={cn("font-mono", statusColor(telemetry.segment_upload_p95_ms, 1000, 3000))}>
							{telemetry.segment_upload_p95_ms} ms
						</span>
					</div>
				{/if}
				{#if (telemetry.segment_queue_depth ?? 0) > 0}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Upload queue</span>
						<span class={cn("font-mono", statusColor(telemetry.segment_queue_depth ?? 0, 10, 30))}>
							{telemetry.segment_queue_depth}
						</span>
					</div>
				{/if}
				{#if (telemetry.segment_upload_retries ?? 0) > 0}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Upload retries</span>
						<span class="font-mono text-warning">{telemetry.segment_upload_retries}</span>
					</div>
				{/if}
				{#if (telemetry.live_ws_dropped_frames ?? 0) > 0}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Live drops</span>
						<span class="font-mono text-warning">{telemetry.live_ws_dropped_frames}</span>
					</div>
				{/if}
				{#if (telemetry.live_ws_bytes_per_sec ?? 0) > 0}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Live</span>
						<span class="font-mono">{formatBitrate(telemetry.live_ws_bytes_per_sec ?? 0)}</span>
					</div>
				{/if}
				{#if telemetry.disk_used_pct != null}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Disk</span>
						<span class={cn("font-mono", statusColor(telemetry.disk_used_pct, 85, 95))}>
							{telemetry.disk_used_pct}%
						</span>
					</div>
				{/if}
				{#if (telemetry.network_recovery_attempts ?? 0) > 0}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Net recoveries</span>
						<span class="font-mono text-warning">{telemetry.network_recovery_attempts}</span>
					</div>
				{/if}
				{#if telemetry.modem_rat}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Modem</span>
						<span class="font-mono">{telemetry.modem_rat}</span>
					</div>
				{/if}
				{#if (telemetry.event_loop_lag_ms ?? 0) > 5}
					<div class="flex justify-between">
						<span class="text-muted-foreground">Loop lag</span>
						<span class={cn("font-mono", statusColor(telemetry.event_loop_lag_ms ?? 0, 50, 200))}>
							{telemetry.event_loop_lag_ms} ms
						</span>
					</div>
				{/if}
				{#if (telemetry.gpsd_query_ms ?? 0) > 100}
					<div class="flex justify-between">
						<span class="text-muted-foreground">gpsd query</span>
						<span class={cn("font-mono", statusColor(telemetry.gpsd_query_ms ?? 0, 200, 1000))}>
							{telemetry.gpsd_query_ms} ms
						</span>
					</div>
				{/if}
			</div>
		{/if}
	</div>
{:else}
	<div class="px-4 py-6 text-xs text-muted-foreground text-center">
		Waiting for telemetry...
	</div>
{/if}
