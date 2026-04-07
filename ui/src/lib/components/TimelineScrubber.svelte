<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { cameraColor } from '$lib/utils/colors.js';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);

	const MIN_WINDOW_SECS = 5 * 60;
	const LIVE_MARGIN_SECS = 5;
	const SEEK_MARGIN_SECS = 30;

	// Freeze the window edge when seeking so timeline doesn't shift
	let frozenEnd = $state(Date.now() / 1000);
	$effect(() => {
		if (scrubberStore.isLive) frozenEnd = scrubberStore.playheadTime;
	});

	let margin = $derived(scrubberStore.isLive ? LIVE_MARGIN_SECS : SEEK_MARGIN_SECS);
	let windowEnd = $derived((scrubberStore.isLive ? scrubberStore.playheadTime : frozenEnd) + margin);
	let windowStart = $derived.by(() => {
		const avail = scrubberStore.availableWindow;
		if (avail) {
			const duration = windowEnd - avail.start;
			if (duration < MIN_WINDOW_SECS) return windowEnd - MIN_WINDOW_SECS;
			return avail.start - margin;
		}
		return windowEnd - MIN_WINDOW_SECS;
	});

	let playheadPercent = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0) return 100;
		return Math.max(0, Math.min(100, ((scrubberStore.playheadTime - windowStart) / range) * 100));
	});

	function formatTime(ts: number): string {
		return new Date(ts * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
	}

	function timeFromEvent(e: MouseEvent | PointerEvent): number {
		if (!trackEl) return scrubberStore.playheadTime;
		const rect = trackEl.getBoundingClientRect();
		const pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
		return windowStart + pct * (windowEnd - windowStart);
	}

	function onPointerDown(e: PointerEvent) {
		e.preventDefault();
		dragging = true;
		scrubberStore.isLive = false;
		scrubberStore.playheadTime = timeFromEvent(e);

		const onMove = (ev: PointerEvent) => {
			scrubberStore.playheadTime = timeFromEvent(ev);
		};
		const onUp = (ev: PointerEvent) => {
			dragging = false;
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
			scrubberStore.seekTo(timeFromEvent(ev));
		};
		window.addEventListener('pointermove', onMove);
		window.addEventListener('pointerup', onUp);
	}

	// Per-camera coverage bars: each camera gets its own row of bars with a unique color
	let cameraIds = $derived(Array.from(scrubberStore.cameraCoverage.keys()));
	let cameraCount = $derived(cameraIds.length);
	let trackHeight = $derived(Math.max(6, cameraCount > 0 ? 12 : 6));

	let perCameraBars = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0) return [];

		return cameraIds.map((deviceId, idx) => {
			const coverage = scrubberStore.cameraCoverage.get(deviceId) ?? [];
			const camIdx = cameraStore.cameras.findIndex(c => c.device_id === deviceId);
			const color = cameraColor(camIdx >= 0 ? camIdx : idx);
			const name = cameraConfigStore.getDisplayName(deviceId);
			const laneHeight = cameraCount > 1 ? 1 / cameraCount : 1;
			const laneTop = cameraCount > 1 ? idx * laneHeight : 0;

			const bars = coverage
				.map((s) => {
					const left = Math.max(0, ((s.start - windowStart) / range) * 100);
					const right = Math.min(100, ((s.end - windowStart) / range) * 100);
					return { left, width: right - left };
				})
				.filter((s) => s.width > 0);

			return { deviceId, color, name, bars, laneTop, laneHeight };
		});
	});
</script>

<div class="flex items-center gap-3 px-4 py-2 bg-black/60 backdrop-blur-sm border-t border-white/10">
	<span class="text-xs text-muted-foreground font-mono w-20 shrink-0">
		{formatTime(scrubberStore.playheadTime)}
	</span>

	<div
		bind:this={trackEl}
		class="relative flex-1 cursor-pointer select-none touch-none"
		style="height: {trackHeight * 2 + 16}px"
		role="slider"
		tabindex="0"
		aria-valuenow={scrubberStore.playheadTime}
		onpointerdown={onPointerDown}
	>
		<!-- Track background -->
		<div class="absolute inset-x-0 top-1/2 -translate-y-1/2 rounded-full bg-white/10" style="height: {trackHeight}px"></div>

		<!-- Per-camera stacked bars -->
		{#each perCameraBars as cam}
			{#each cam.bars as bar}
				<div
					class="absolute rounded-sm"
					style="
						left: {bar.left}%;
						width: {bar.width}%;
						top: calc(50% - {trackHeight / 2}px + {cam.laneTop * trackHeight}px);
						height: {cam.laneHeight * trackHeight}px;
						background: {cam.color};
						opacity: 0.7;
					"
					title="{cam.name}"
				></div>
			{/each}
		{/each}

		<!-- Playhead + tooltip -->
		<div
			class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 z-10"
			style="left: {playheadPercent}%"
		>
			{#if dragging}
				<div class="absolute bottom-5 left-1/2 -translate-x-1/2 whitespace-nowrap rounded bg-black/90 px-2 py-1 text-[11px] font-mono text-white shadow-lg pointer-events-none">
					{formatTime(scrubberStore.playheadTime)}
				</div>
			{/if}
			<div
				class={cn(
					'w-3 h-3 rounded-full transition-transform',
					scrubberStore.isLive
						? 'bg-emerald-400 shadow-[0_0_8px_theme(colors.emerald.400/0.6)]'
						: 'bg-sky-400 shadow-[0_0_8px_theme(colors.sky.400/0.6)]',
					dragging && 'scale-150',
				)}
			></div>
		</div>

		<!-- Time labels -->
		<div class="absolute inset-x-0 bottom-0 flex justify-between pointer-events-none">
			{#each Array(5) as _, i}
				{@const t = windowStart + (i / 4) * (windowEnd - windowStart)}
				<span class="text-[9px] text-white/30 font-mono">{formatTime(t)}</span>
			{/each}
		</div>
	</div>

	<button
		class={cn(
			"shrink-0 w-14 py-1 text-xs font-medium rounded text-center transition-colors",
			scrubberStore.isLive
				? "text-emerald-400 cursor-default"
				: "bg-emerald-500 text-black hover:bg-emerald-400 cursor-pointer",
		)}
		onclick={() => scrubberStore.goLive()}
		disabled={scrubberStore.isLive}
	>
		LIVE
	</button>
</div>
