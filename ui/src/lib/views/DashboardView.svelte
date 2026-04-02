<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { alertsStore } from '$lib/stores/alerts.svelte.js';
	import { type TelemetryEntry } from '$lib/signaling.js';
	import { fetchTelemetryRangeCached, nearestTelemetryEntryWithin } from '$lib/telemetry-history.js';
	import Sparkline from '$lib/components/ui/Sparkline.svelte';
	import { cn } from '$lib/utils.js';

	// Bitrate history for sparkline
	const MAX_HISTORY = 30;
	let bitrateHistory = $state<number[]>([]);

	// Connection uptime
	let uptimeStr = $state('--');
	let uptimeInterval: ReturnType<typeof setInterval> | null = null;
	let historicalTelemetryByDevice = $state<Record<string, TelemetryEntry | null>>({});
	let historicalFetchTimer: ReturnType<typeof setTimeout> | null = null;
	const MAX_DASH_STALENESS_MS = 20 * 60 * 1000;
	let cameras = $derived(cameraStore.cameras);

	$effect(() => {
		// Update online count history every 2s for sparkline
		const iv = setInterval(() => {
			bitrateHistory = [...bitrateHistory.slice(-(MAX_HISTORY - 1)), cameraStore.onlineCount];
		}, 2000);
		return () => clearInterval(iv);
	});

	$effect(() => {
		const isLive = scrubberStore.isLive;
		const playheadTime = Math.floor(scrubberStore.playheadTime);
		const deviceIds = cameras.map((c) => c.device_id);

		if (isLive || deviceIds.length === 0) {
			historicalTelemetryByDevice = {};
			if (historicalFetchTimer) {
				clearTimeout(historicalFetchTimer);
				historicalFetchTimer = null;
			}
			return;
		}

		if (historicalFetchTimer) return;
		historicalFetchTimer = setTimeout(async () => {
			const targetMs = Math.floor(scrubberStore.playheadTime * 1000);
			const fromMs = Math.max(0, targetMs - 3 * 60 * 1000);
			const toMs = targetMs + 3 * 60 * 1000;
			const fallbackFromMs = Math.max(0, targetMs - 20 * 60 * 1000);
			const fallbackToMs = targetMs + 20 * 60 * 1000;
			const nextByDevice: Record<string, TelemetryEntry | null> = {};

			await Promise.all(
				deviceIds.map(async (deviceId) => {
					try {
						const entries = await fetchTelemetryRangeCached(deviceId, fromMs, toMs, 480);
						let nearest = nearestTelemetryEntryWithin(
							entries,
							targetMs,
							MAX_DASH_STALENESS_MS,
						);
						if (!nearest) {
							const fallbackEntries = await fetchTelemetryRangeCached(
								deviceId,
								fallbackFromMs,
								fallbackToMs,
								1200,
							);
							nearest = nearestTelemetryEntryWithin(
								fallbackEntries,
								targetMs,
								MAX_DASH_STALENESS_MS,
							);
						}
						if (!nearest) {
							nextByDevice[deviceId] = null;
							return;
						}
						nextByDevice[deviceId] = nearest;
					} catch {
						nextByDevice[deviceId] = null;
					}
				}),
			);

			historicalTelemetryByDevice = nextByDevice;
			historicalFetchTimer = null;
		}, 260);
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

	let onlineCount = $derived(cameraStore.onlineCount);

	function formatBitrate(kbps: number): string {
		if (kbps >= 1000) return `${(kbps / 1000).toFixed(1)} Mbps`;
		return `${kbps.toFixed(0)} kbps`;
	}

	function statusColor(value: number, warn: number, crit: number): string {
		if (value >= crit) return 'text-destructive';
		if (value >= warn) return 'text-warning';
		return 'text-primary';
	}

	function getTelemetryForCamera(deviceId: string, fallback: typeof cameras[number]['telemetry']) {
		if (scrubberStore.isLive) return fallback;
		const historical = historicalTelemetryByDevice[deviceId];
		if (!historical) return null;
		return {
			cpu_percent: historical.cpu,
			memory_mb: historical.mem,
			temp_celsius: historical.temp,
			uptime_secs: historical.uptime,
		};
	}
</script>

<div class="h-full overflow-y-auto p-4 space-y-4">
	{#if !scrubberStore.isLive}
		<div class="text-xs text-sky-400 font-mono">
			Playback snapshot at {new Date(scrubberStore.playheadTime * 1000).toLocaleTimeString()}
		</div>
	{/if}
	<!-- Top stat cards -->
	<div class="grid grid-cols-2 lg:grid-cols-4 gap-3">
		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Cameras Online</div>
			<div class="text-2xl font-bold mt-1">{onlineCount}</div>
			<div class="text-xs text-muted-foreground">{cameras.length} total</div>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Total Cameras</div>
			<div class="text-2xl font-bold mt-1">{cameras.length}</div>
			<div class="h-6 mt-1">
				<Sparkline data={bitrateHistory} height={24} class="w-full text-primary" />
			</div>
		</div>

		<div class="rounded-lg border bg-card p-4">
			<div class="text-xs text-muted-foreground uppercase tracking-wider">Stream Type</div>
			<div class="text-2xl font-bold mt-1">HLS</div>
			<div class="text-xs text-muted-foreground">S3-backed segments</div>
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
			<div class="text-xs text-muted-foreground uppercase tracking-wider mb-2">Delivery</div>
			<div class="flex items-center gap-2">
				<span class={cn("h-2.5 w-2.5 rounded-full", 'bg-green-500')}></span>
				<span class="text-sm font-medium">S3 / Tigris</span>
			</div>
			<p class="text-xs text-muted-foreground mt-1">Segments served from edge</p>
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
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">CPU</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Memory</th>
							<th class="text-right px-4 py-2 font-medium text-muted-foreground">Temp</th>
						</tr>
					</thead>
					<tbody>
						{#each cameras as camera (camera.device_id)}
							{@const t = getTelemetryForCamera(camera.device_id, camera.telemetry)}
							<tr class="border-b last:border-0 hover:bg-muted/30 transition-colors">
								<td class="px-4 py-2.5 font-medium">
									{cameraConfigStore.getDisplayName(camera.device_id)}
								</td>
								<td class="px-4 py-2.5">
									<span class={cn("inline-flex items-center gap-1.5",
										camera.online ? 'text-green-500' : 'text-red-500'
									)}>
										<span class={cn("h-1.5 w-1.5 rounded-full",
											camera.online ? 'bg-green-500' : 'bg-red-500'
										)}></span>
										{camera.online ? 'Online' : 'Offline'}
									</span>
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t && t.cpu_percent != null}
										<span class={statusColor(t.cpu_percent, 70, 90)}>
											{t.cpu_percent.toFixed(1)}%
										</span>
									{:else}
										<span class="text-muted-foreground">--</span>
									{/if}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t && t.memory_mb != null}
										{t.memory_mb.toFixed(0)} MB
									{:else}
										<span class="text-muted-foreground">--</span>
									{/if}
								</td>
								<td class="px-4 py-2.5 font-mono text-right">
									{#if t && t.temp_celsius != null}
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
