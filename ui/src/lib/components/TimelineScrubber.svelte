<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);

	const MIN_WINDOW_SECS = 5 * 60;
	const ZOOMED_WINDOW_SECS = 600;
	const LIVE_MARGIN_SECS = 5;
	const SEEK_MARGIN_SECS = 30;
	const GAP_THRESHOLD = 30;
	const ZOOM_DELAY_MS = 1800;
	const ZOOM_DURATION_MS = 600;

	let frozenEnd = $state(Date.now() / 1000);
	$effect(() => {
		if (scrubberStore.isLive) frozenEnd = scrubberStore.playheadTime;
	});

	let zoomTimer: ReturnType<typeof setTimeout> | null = null;
	let zoomAnim: number | null = null;
	/** When non-null, overrides the computed window for zoom animation. */
	let zoomOverride = $state<{ start: number; end: number } | null>(null);

	let margin = $derived(scrubberStore.isLive ? LIVE_MARGIN_SECS : SEEK_MARGIN_SECS);

	let naturalEnd = $derived((scrubberStore.isLive ? scrubberStore.playheadTime : frozenEnd) + margin);
	let naturalStart = $derived.by(() => {
		const avail = scrubberStore.availableWindow;
		if (avail) {
			const duration = naturalEnd - avail.start;
			if (duration < MIN_WINDOW_SECS) return naturalEnd - MIN_WINDOW_SECS;
			return avail.start - margin;
		}
		return naturalEnd - MIN_WINDOW_SECS;
	});

	let windowStart = $derived(zoomOverride?.start ?? naturalStart);
	let windowEnd = $derived(zoomOverride?.end ?? naturalEnd);

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

	function stopZoom() {
		if (zoomTimer) { clearTimeout(zoomTimer); zoomTimer = null; }
		if (zoomAnim) { cancelAnimationFrame(zoomAnim); zoomAnim = null; }
	}

	function animateZoom(fromStart: number, fromEnd: number, toStart: number, toEnd: number, onDone?: () => void) {
		stopZoom();
		const t0 = performance.now();
		const animate = (now: number) => {
			const progress = Math.min(1, (now - t0) / ZOOM_DURATION_MS);
			const ease = 1 - Math.pow(1 - progress, 3);
			zoomOverride = {
				start: fromStart + (toStart - fromStart) * ease,
				end: fromEnd + (toEnd - fromEnd) * ease,
			};
			if (progress < 1) {
				zoomAnim = requestAnimationFrame(animate);
			} else {
				onDone?.();
			}
		};
		zoomAnim = requestAnimationFrame(animate);
	}

	// Edge panning: when dragging near the edge of the zoomed window,
	// slowly shift the window in that direction.
	const PAN_EDGE_ZONE = 0.15; // 15% of track width on each side
	const PAN_SPEED = 30; // seconds per second of panning
	let panFrame: number | null = null;
	let lastPanTime = 0;

	function startEdgePan(getPixelRatio: () => number) {
		stopEdgePan();
		lastPanTime = performance.now();
		const tick = (now: number) => {
			if (!zoomOverride) { panFrame = null; return; }
			const dt = (now - lastPanTime) / 1000;
			lastPanTime = now;
			const ratio = getPixelRatio();
			// ratio: -1 = hard left, +1 = hard right, 0 = center (no pan)
			if (Math.abs(ratio) > 0) {
				const shift = ratio * PAN_SPEED * dt;
				zoomOverride = {
					start: zoomOverride.start + shift,
					end: zoomOverride.end + shift,
				};
				// Update playhead to match the shifted window edge
				scrubberStore.playheadTime += shift;
			}
			panFrame = requestAnimationFrame(tick);
		};
		panFrame = requestAnimationFrame(tick);
	}

	function stopEdgePan() {
		if (panFrame != null) { cancelAnimationFrame(panFrame); panFrame = null; }
	}

	function onPointerDown(e: PointerEvent) {
		e.preventDefault();
		dragging = true;
		scrubberStore.dragging = true;
		scrubberStore.isLive = false;
		scrubberStore.playheadTime = timeFromEvent(e);

		const clickTime = timeFromEvent(e);
		let zoomed = false;

		const snapStart = windowStart;
		const snapEnd = windowEnd;

		stopZoom();
		zoomTimer = setTimeout(() => {
			zoomed = true;
			const halfZoom = ZOOMED_WINDOW_SECS / 2;
			animateZoom(snapStart, snapEnd, clickTime - halfZoom, clickTime + halfZoom);
		}, ZOOM_DELAY_MS);

		let panRatio = 0;
		const getPanRatio = () => panRatio;

		const onMove = (ev: PointerEvent) => {
			scrubberStore.playheadTime = timeFromEvent(ev);

			// Edge panning when zoomed
			if (zoomed && zoomOverride && trackEl) {
				const rect = trackEl.getBoundingClientRect();
				const pct = (ev.clientX - rect.left) / rect.width;
				if (pct < PAN_EDGE_ZONE) {
					panRatio = -(1 - pct / PAN_EDGE_ZONE); // -1 at far left
					if (!panFrame) startEdgePan(getPanRatio);
				} else if (pct > 1 - PAN_EDGE_ZONE) {
					panRatio = (pct - (1 - PAN_EDGE_ZONE)) / PAN_EDGE_ZONE; // +1 at far right
					if (!panFrame) startEdgePan(getPanRatio);
				} else {
					panRatio = 0;
					stopEdgePan();
				}
			}
		};
		const onUp = (ev: PointerEvent) => {
			dragging = false;
			scrubberStore.dragging = false;
			stopZoom();
			stopEdgePan();
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
			scrubberStore.seekTo(timeFromEvent(ev));
			if (zoomed && zoomOverride) {
				const zs = zoomOverride.start;
				const ze = zoomOverride.end;
				animateZoom(zs, ze, naturalStart, naturalEnd, () => {
					zoomOverride = null;
				});
			} else {
				zoomOverride = null;
			}
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
		onclick={() => scrubberStore.goLive()}
		disabled={scrubberStore.isLive}
	>
		LIVE
	</button>
</div>
