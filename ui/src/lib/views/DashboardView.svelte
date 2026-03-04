<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { videoStatsStore } from '$lib/stores/videoStats.svelte.js';
	import { alertsStore } from '$lib/stores/alerts.svelte.js';
	import Sparkline from '$lib/components/ui/Sparkline.svelte';
	import { cn } from '$lib/utils.js';

	// Bitrate history for sparkline
	const MAX_HISTORY = 30;
	let bitrateHistory = $state<number[]>([]);

	// Connection uptime
	let uptimeStr = $state('--');
	let uptimeInterval: ReturnType<typeof setInterval> | null = null;

	$effect(() => {
		// Update bitrate history every 2s
		const iv = setInterval(() => {
			const kbps = videoStatsStore.totalBitrateKbps;
			bitrateHistory = [...bitrateHistory.slice(-(MAX_HISTORY - 1)), kbps];
		}, 2000);
		return () => clearInterval(iv);
	});

	$effect(() => {
		// Update uptime display
		if (uptimeInterval) clearInterval(uptimeInterval);
		const update = () => {
			const at = transportStore.connectedAt;
			if (!at) { uptimeStr = '--'; return; }
			const secs = Math.floor((Date.now() - at) / 1000);
			const h = Math.floor(secs / 3600);
			const m = Math.floor((secs % 3600) / 60);
			const s = secs % 60;
			uptimeStr = h > 0 ? `${h}h ${m}m ${s}s` : `${m}m ${s}s`;
		};
		update();
		uptimeInterval = setInterval(update, 1000);
		return () => { if (uptimeInterval) clearInterval(uptimeInterval); };
	});

	let cameras = $derived(cameraStore.cameras);
	let onlineCount = $derived(cameraStore.onlineCount);
	let totalBitrate = $derived(videoStatsStore.totalBitrateKbps);
	let totalDecoded = $derived(videoStatsStore.totalFramesDecoded);
	let totalDropped = $derived(videoStatsStore.totalFramesDropped);
	let dropRate = $derived(totalDecoded > 0 ? (totalDropped / (totalDecoded + totalDropped)) * 100 : 0);

	function formatBitrate(kbps: number): string {
		if (kbps >= 1000) return `${(kbps / 1000).toFixed(1)} Mbps`;
		return `${kbps.toFixed(0)} kbps`;
	}

	function statusColor(value: number, warn: number, crit: number): string {
		if (value >= crit) return 'text-destructive';
		if (value >= warn) return 'text-warning';
		return 'text-primary';
	}
</script>

<div class="h-full overflow-y-auto p-4 space-y-4">
	<!-- Top stat cards -->
	<div class="grid grid-cols-2 lg:grid-cols-4 gap-3">
		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Cameras Online</div>
			<div class="text-2xl font-bold mt-1">{onlineCount}</div>
			<div class="text-xs text-muted-foreground">{cameras.length} total</div>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Total Bandwidth</div>
			<div class="text-2xl font-bold mt-1">{formatBitrate(totalBitrate)}</div>
			<div class="h-6 mt-1">
				<Sparkline data={bitrateHistory} height={24} class="w-full text-primary" />
			</div>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Frames Decoded</div>
			<div class="text-2xl font-bold mt-1">{totalDecoded.toLocaleString()}</div>
			<div class="text-xs text-muted-foreground">{totalDropped} dropped</div>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Connection Uptime</div>
			<div class="text-2xl font-bold mt-1 font-mono">{uptimeStr}</div>
			<div class="text-xs {transportStore.connected ? 'text-primary' : 'text-destructive'}">
				{transportStore.connectionState}
			</div>
		</div>
	</div>

	<!-- Status row -->
	<div class="grid grid-cols-1 lg:grid-cols-3 gap-3">
		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider mb-2">Connection</div>
			<div class="flex items-center gap-2">
				<span class={cn("h-2.5 w-2.5 rounded-full", transportStore.connected ? 'bg-green-500' : 'bg-red-500')}></span>
				<span class="text-sm font-medium capitalize">{transportStore.connectionState}</span>
			</div>
			{#if transportStore.error}
				<p class="text-xs text-destructive mt-1">{transportStore.error}</p>
			{/if}
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider mb-2">Stream Health</div>
			<div class="flex items-center gap-2">
				<span class={cn("h-2.5 w-2.5 rounded-full",
					dropRate < 1 ? 'bg-green-500' : dropRate < 5 ? 'bg-yellow-500' : 'bg-red-500'
				)}></span>
				<span class="text-sm font-medium">
					{dropRate < 1 ? 'Excellent' : dropRate < 5 ? 'Good' : 'Degraded'}
				</span>
			</div>
			<p class="text-xs text-muted-foreground mt-1">{dropRate.toFixed(2)}% frame drop rate</p>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider mb-2">Alerts</div>
			<div class="flex items-center gap-2">
				<span class="text-sm font-medium">{alertsStore.unreadCount} unread</span>
			</div>
			<p class="text-xs text-muted-foreground mt-1">{alertsStore.alerts.length} total</p>
		</div>
	</div>

	<!-- Camera table -->
	<div class="rounded-lg border bg-card overflow-hidden">
		<div class="px-4 py-3 border-b">
			<h3 class="text-sm font-semibold">Camera Details</h3>
		</div>
		{#if cameras.length === 0}
			<div class="px-4 py-8 text-center text-sm text-muted-foreground">
				No cameras connected
			</div>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full text-xs">
					<thead>
						<tr class="border-b bg-muted/50">
							<th class="text-left px-4 py-2 font-medium text-muted-foreground">Camera</th>
							<th class="text-left px-4 py-2 font-medium text-muted-foreground">Status</th>
							<th class="text-left px-4 py-2 font-medium text-muted-foreground">Resolution</th>
							<th class="text-left px-4 py-2 font-medium text-muted-foreground">Codec</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Bitrate</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Dropped</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">CPU</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Memory</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Temp</th>
						</tr>
					</thead>
					<tbody>
						{#each cameras as camera (camera.device_id)}
							{@const stats = videoStatsStore.get(camera.device_id)}
							{@const t = camera.telemetry}
							<tr class="border-b last:border-0 hover:bg-muted/30 transition-colors">
								<td class="px-4 py-2.5 font-medium">
									{cameraConfigStore.getDisplayName(camera.device_id)}
								</td>
								<td class="px-4 py-2.5">
									<span class={cn("inline-flex items-center gap-1.5",
										camera.connected ? 'text-green-500' : 'text-red-500'
									)}>
										<span class={cn("h-1.5 w-1.5 rounded-full",
											camera.connected ? 'bg-green-500' : 'bg-red-500'
										)}></span>
										{camera.connected ? 'Online' : 'Offline'}
									</span>
								</td>
								<td class="px-4 py-2.5 font-mono text-muted-foreground">
									{#if stats && stats.width > 0}
										{stats.width}x{stats.height}
									{:else}
										--
									{/if}
								</td>
								<td class="px-4 py-2.5 font-mono text-muted-foreground uppercase">
									{stats?.codec || '--'}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{stats ? formatBitrate(stats.bitrateKbps) : '--'}
								</td>
								<td class="px-4 py-2.5 font-mono text-right text-muted-foreground">
									{stats?.framesDropped ?? '--'}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t}
										<span class={statusColor(t.cpu_percent, 70, 90)}>
											{t.cpu_percent.toFixed(1)}%
										</span>
									{:else}
										<span class="text-muted-foreground">--</span>
									{/if}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t}
										{t.memory_mb.toFixed(0)} MB
									{:else}
										<span class="text-muted-foreground">--</span>
									{/if}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t && t.temp_celsius > 0}
										<span class={statusColor(t.temp_celsius, 70, 85)}>
											{t.temp_celsius.toFixed(0)}&deg;C
										</span>
									{:else}
										<span class="text-muted-foreground">--</span>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</div>
</div>
