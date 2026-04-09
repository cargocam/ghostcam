<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);

	const MIN_WINDOW_SECS = 5 * 60;
	const ZOOMED_WINDOW_SECS = 60; // 1 minute when fully zoomed
	const LIVE_MARGIN_SECS = 5;
	const SEEK_MARGIN_SECS = 30;
	const GAP_THRESHOLD = 30;
	const ZOOM_DELAY_MS = 300; // hold this long before zoom starts
	const ZOOM_DURATION_MS = 600; // animation duration

	let frozenEnd = $state(Date.now() / 1000);
	$effect(() => {
		if (scrubberStore.isLive) frozenEnd = scrubberStore.playheadTime;
	});

	// Zoom state: 0 = normal, 1 = fully zoomed
	let zoomLevel = $state(0);
	let zoomCenter = $state(0); // epoch seconds where the user pressed
	let zoomTimer: ReturnType<typeof setTimeout> | null = null;
	let zoomAnim: number | null = null;

	// Compute window bounds with zoom interpolation
	let margin = $derived(scrubberStore.isLive ? LIVE_MARGIN_SECS : SEEK_MARGIN_SECS);

	let baseEnd = $derived((scrubberStore.isLive ? scrubberStore.playheadTime : frozenEnd) + margin);
	let baseStart = $derived.by(() => {
		const avail = scrubberStore.availableWindow;
		if (avail) {
			const duration = baseEnd - avail.start;
			if (duration < MIN_WINDOW_SECS) return baseEnd - MIN_WINDOW_SECS;
			return avail.start - margin;
		}
		return baseEnd - MIN_WINDOW_SECS;
	});

	// When zoomed, narrow the window around zoomCenter
	let windowStart = $derived.by(() => {
		if (zoomLevel <= 0) return baseStart;
		const halfZoomed = ZOOMED_WINDOW_SECS / 2;
		const halfBase = (baseEnd - baseStart) / 2;
		const half = halfBase + (halfZoomed - halfBase) * zoomLevel;
		return zoomCenter - half;
	});
	let windowEnd = $derived.by(() => {
		if (zoomLevel <= 0) return baseEnd;
		const halfZoomed = ZOOMED_WINDOW_SECS / 2;
		const halfBase = (baseEnd - baseStart) / 2;
		const half = halfBase + (halfZoomed - halfBase) * zoomLevel;
		return zoomCenter + half;
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

	let hoverTime = $state<number | null>(null);
	let hoverPercent = $state<number>(0);

	function onMouseMove(e: MouseEvent) {
		if (dragging) return;
		if (!trackEl) return;
		const rect = trackEl.getBoundingClientRect();
		const pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
		hoverPercent = pct * 100;
		hoverTime = windowStart + pct * (windowEnd - windowStart);
	}

	function onMouseLeave() {
		hoverTime = null;
	}

	function startZoomAnimation() {
		const startTime = performance.now();
		const startLevel = zoomLevel;
		const animate = (now: number) => {
			const elapsed = now - startTime;
			const progress = Math.min(1, elapsed / ZOOM_DURATION_MS);
			// Ease out cubic
			const eased = 1 - Math.pow(1 - progress, 3);
			zoomLevel = startLevel + (1 - startLevel) * eased;
			if (progress < 1) {
				zoomAnim = requestAnimationFrame(animate);
			}
		};
		zoomAnim = requestAnimationFrame(animate);
	}

	function stopZoom() {
		if (zoomTimer) { clearTimeout(zoomTimer); zoomTimer = null; }
		if (zoomAnim) { cancelAnimationFrame(zoomAnim); zoomAnim = null; }
	}

	function onPointerDown(e: PointerEvent) {
		e.preventDefault();
		dragging = true;
		scrubberStore.isLive = false;
		scrubberStore.playheadTime = timeFromEvent(e);
		zoomCenter = timeFromEvent(e);

		// Start zoom after a short hold
		stopZoom();
		zoomTimer = setTimeout(() => {
			startZoomAnimation();
		}, ZOOM_DELAY_MS);

		const onMove = (ev: PointerEvent) => {
			scrubberStore.playheadTime = timeFromEvent(ev);
			// Update zoom center to follow the cursor while zooming
			if (zoomLevel > 0) {
				zoomCenter = timeFromEvent(ev);
			}
		};
		const onUp = (ev: PointerEvent) => {
			dragging = false;
			stopZoom();
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
			scrubberStore.seekTo(timeFromEvent(ev));
		};
		window.addEventListener('pointermove', onMove);
		window.addEventListener('pointerup', onUp);
	}

	function mergeSpans(segments: { start: number; end: number }[]): { left: number; width: number }[] {
		const range = windowEnd - windowStart;
		if (range <= 0 || segments.length === 0) return [];

		const sorted = [...segments].sort((a, b) => a.start - b.start);
		const merged: { start: number; end: number }[] = [{ ...sorted[0] }];
		for (let i = 1; i < sorted.length; i++) {
			const last = merged[merged.length - 1];
			if (sorted[i].start <= last.end + GAP_THRESHOLD) {
				last.end = Math.max(last.end, sorted[i].end);
			} else {
				merged.push({ ...sorted[i] });
			}
		}

		return merged
			.map((s) => {
				const left = Math.max(0, ((s.start - windowStart) / range) * 100);
				const right = Math.min(100, ((s.end - windowStart) / range) * 100);
				return { left, width: right - left };
			})
			.filter((s) => s.width > 0);
	}

	// Union of all cameras
	let unionBars = $derived.by(() => {
		const all: { start: number; end: number }[] = [];
		for (const [, coverage] of scrubberStore.cameraCoverage) {
			for (const s of coverage) all.push(s);
		}
		return mergeSpans(all);
	});

	// Selected camera only
	let selectedBars = $derived.by(() => {
		const id = cameraStore.selectedId;
		if (!id) return [];
		const coverage = scrubberStore.cameraCoverage.get(id);
		if (!coverage) return [];
		return mergeSpans(coverage);
	});

	let hasSelection = $derived(cameraStore.selectedId != null && selectedBars.length > 0);

	// Motion dots: amber markers at motion event timestamps
	let motionDots = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0) return [];
		const dots: { left: number }[] = [];
		for (const [, timestamps] of scrubberStore.motionTimestamps) {
			for (const ts of timestamps) {
				const pct = ((ts - windowStart) / range) * 100;
				if (pct >= 0 && pct <= 100) {
					dots.push({ left: pct });
				}
			}
		}
		return dots;
	});
</script>

<div class="flex items-center gap-3 px-4 py-2 bg-background/95 backdrop-blur-sm border-t border-border">
	<span class="text-xs text-muted-foreground font-mono w-20 shrink-0">
		{formatTime(scrubberStore.playheadTime)}
	</span>

	<div
		bind:this={trackEl}
		class="relative flex-1 h-8 cursor-pointer select-none touch-none"
		role="slider"
		tabindex="0"
		aria-valuenow={scrubberStore.playheadTime}
		onpointerdown={onPointerDown}
		onmousemove={onMouseMove}
		onmouseleave={onMouseLeave}
	>
		<!-- Track background -->
		<div class="absolute inset-x-0 top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-muted"></div>

		<!-- Union coverage (semi-transparent when a camera is selected) -->
		{#each unionBars as bar}
			<div
				class="absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-emerald-500"
				class:opacity-25={hasSelection}
				class:opacity-70={!hasSelection}
				style="left: {bar.left}%; width: {bar.width}%"
			></div>
		{/each}

		<!-- Selected camera coverage (solid, on top) -->
		{#if hasSelection}
			{#each selectedBars as bar}
				<div
					class="absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-emerald-400"
					style="left: {bar.left}%; width: {bar.width}%"
				></div>
			{/each}
		{/if}

		<!-- Motion dots (amber) -->
		{#each motionDots as dot}
			<div
				class="absolute top-1/2 -translate-y-1/2 w-1.5 h-1.5 rounded-full bg-amber-400"
				style="left: {dot.left}%"
				title="Motion detected"
			></div>
		{/each}

		<!-- Hover tooltip -->
		{#if hoverTime !== null && !dragging}
			<div
				class="absolute bottom-5 -translate-x-1/2 whitespace-nowrap rounded bg-popover px-2 py-1 text-[11px] font-mono text-popover-foreground shadow-lg pointer-events-none border border-border z-20"
				style="left: {hoverPercent}%"
			>
				{formatTime(hoverTime)}
			</div>
		{/if}

		<!-- Playhead + tooltip -->
		<div
			class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 z-10"
			style="left: {playheadPercent}%"
		>
			{#if dragging}
				<div class="absolute bottom-5 left-1/2 -translate-x-1/2 whitespace-nowrap rounded bg-popover px-2 py-1 text-[11px] font-mono text-popover-foreground shadow-lg pointer-events-none border border-border">
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
				<span class="text-[9px] text-muted-foreground/50 font-mono">{formatTime(t)}</span>
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
		onclick={() => { zoomLevel = 0; scrubberStore.goLive(); }}
		disabled={scrubberStore.isLive}
	>
		LIVE
	</button>
</div>
