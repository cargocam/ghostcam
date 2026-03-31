<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);

	// Display window: last 2 hours
	let windowDuration = 2 * 60 * 60; // 2 hours in seconds

	// Freeze the window end when entering playback so ticks don't shift
	let frozenWindowEnd = $state(Date.now() / 1000);

	$effect(() => {
		if (scrubberStore.mode === 'live') {
			// In live mode, continuously update the frozen end to "now"
			frozenWindowEnd = scrubberStore.playheadTime;
		}
	});

	let windowEnd = $derived(frozenWindowEnd);
	let windowStart = $derived(
		scrubberStore.availableWindow?.start ?? windowEnd - windowDuration,
	);

	let playheadPercent = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0) return 100;
		const pct = ((scrubberStore.playheadTime - windowStart) / range) * 100;
		return Math.max(0, Math.min(100, pct));
	});

	let isLive = $derived(scrubberStore.mode === 'live');

	function formatTime(ts: number): string {
		const d = new Date(ts * 1000);
		return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
	}

	function scrubFromEvent(e: MouseEvent | PointerEvent) {
		if (!trackEl) return;
		const rect = trackEl.getBoundingClientRect();
		const pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
		const time = windowStart + pct * (windowEnd - windowStart);
		scrubberStore.scrubTo(time);
	}

	function onPointerDown(e: PointerEvent) {
		scrubFromEvent(e);
		dragging = true;
		trackEl?.setPointerCapture(e.pointerId);
	}

	function onPointerMove(e: PointerEvent) {
		if (!dragging) return;
		scrubFromEvent(e);
	}

	function onPointerUp(e: PointerEvent) {
		if (!dragging) return;
		dragging = false;
		trackEl?.releasePointerCapture(e.pointerId);
	}

	// Segment bars with cache state for each camera
	type SegmentBar = { left: number; width: number; state: 'cached' | 'uploading' | 'available' };
	let coverageBars = $derived.by(() => {
		const bars: { deviceId: string; segments: SegmentBar[] }[] = [];
		const range = windowEnd - windowStart;
		if (range <= 0) return bars;

		for (const [deviceId] of scrubberStore.cameraCoverage) {
			const segStates = scrubberStore.cameraSegmentStates.get(deviceId);

			if (segStates && segStates.length > 0) {
				// Use per-segment cache state for detailed visualization
				const segs: SegmentBar[] = segStates
					.map((s) => {
						const left = Math.max(0, ((s.start - windowStart) / range) * 100);
						const right = Math.min(100, ((s.end - windowStart) / range) * 100);
						return { left, width: right - left, state: s.state };
					})
					.filter((s) => s.width > 0);
				if (segs.length > 0) {
					bars.push({ deviceId, segments: segs });
				}
			} else {
				// Fallback to coverage data (no cache state info)
				const coverage = scrubberStore.cameraCoverage.get(deviceId);
				if (coverage) {
					const segs: SegmentBar[] = coverage
						.map((s) => {
							const left = Math.max(0, ((s.start - windowStart) / range) * 100);
							const right = Math.min(100, ((s.end - windowStart) / range) * 100);
							return { left, width: right - left, state: 'available' as const };
						})
						.filter((s) => s.width > 0);
					if (segs.length > 0) {
						bars.push({ deviceId, segments: segs });
					}
				}
			}
		}
		return bars;
	});
</script>

<div class="flex items-center gap-3 px-4 py-2 bg-black/60 backdrop-blur-sm border-t border-white/10">
	<!-- Timestamp -->
	<span class="text-xs text-muted-foreground font-mono w-20 shrink-0">
		{formatTime(scrubberStore.playheadTime)}
	</span>

	<!-- Track -->
	<div
		bind:this={trackEl}
		class="relative flex-1 h-8 cursor-pointer select-none touch-none"
		role="slider"
		tabindex="0"
		aria-valuenow={scrubberStore.playheadTime}
		aria-valuemin={windowStart}
		aria-valuemax={windowEnd}
		onpointerdown={onPointerDown}
		onpointermove={onPointerMove}
		onpointerup={onPointerUp}
	>
		<!-- Background track -->
		<div class="absolute inset-x-0 top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-white/10"></div>

		<!-- Coverage indicators: green outline = available, solid green = cached, animated blue = uploading -->
		{#each coverageBars as bar}
			{#each bar.segments as seg}
				<div
					class={cn(
						'absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full',
						seg.state === 'cached' && 'bg-green-500',
						seg.state === 'uploading' && 'bg-blue-500 animate-pulse',
						seg.state === 'available' && 'border border-green-500 bg-transparent',
					)}
					style="left: {seg.left}%; width: {seg.width}%"
				></div>
			{/each}
		{/each}

		<!-- Playhead -->
		<div
			class={cn(
				'absolute top-1/2 -translate-y-1/2 -translate-x-1/2 w-3 h-3 rounded-full transition-transform',
				isLive
					? 'bg-emerald-400 shadow-[0_0_8px_theme(colors.emerald.400/0.6)]'
					: 'bg-sky-400 shadow-[0_0_8px_theme(colors.sky.400/0.6)]',
				dragging && 'scale-150',
			)}
			style="left: {playheadPercent}%"
		></div>

		<!-- Tick marks -->
		<div class="absolute inset-x-0 bottom-0 flex justify-between pointer-events-none">
			{#each Array(5) as _, i}
				{@const t = windowStart + (i / 4) * (windowEnd - windowStart)}
				<span class="text-[9px] text-white/30 font-mono">
					{formatTime(t)}
				</span>
			{/each}
		</div>
	</div>

	<!-- Pause/Play button — always present, disabled in live mode -->
	<button
		class={cn(
			"shrink-0 w-7 h-7 flex items-center justify-center rounded transition-colors",
			isLive
				? "text-white/20 cursor-default"
				: "text-white/80 hover:text-white cursor-pointer",
		)}
		onclick={() => { if (!isLive) scrubberStore.playing ? scrubberStore.pause() : scrubberStore.play(); }}
		disabled={isLive}
		title={isLive ? 'Pause (enter playback first)' : scrubberStore.playing ? 'Pause' : 'Play'}
	>
		{#if !isLive && scrubberStore.playing}
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4">
				<path d="M6 4h4v16H6zM14 4h4v16h-4z"/>
			</svg>
		{:else}
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4">
				<path d="M8 5v14l11-7z"/>
			</svg>
		{/if}
	</button>

	<!-- LIVE indicator / Go Live button — fixed width container -->
	<button
		class={cn(
			"shrink-0 w-14 py-1 text-xs font-medium rounded text-center transition-colors",
			isLive
				? "text-emerald-400 cursor-default"
				: "bg-emerald-500 text-black hover:bg-emerald-400 cursor-pointer",
		)}
		onclick={() => { if (!isLive) scrubberStore.goLive(); }}
		disabled={isLive}
	>
		LIVE
	</button>
</div>
