<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { clipStore } from '$lib/stores/clip.svelte.js';
	import ClipDownloadBar from '$lib/components/ClipDownloadBar.svelte';
	import { Scissors } from 'lucide-svelte';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);
	/** -1 = panning left, 0 = not panning, 1 = panning right */
	let panDirection = $state(0);

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
	let isZoomed = $derived(zoomOverride !== null);

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

	function formatTimeShort(ts: number): string {
		return new Date(ts * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
	}

	// Track width-aware label count: use 3 labels when the track is narrow
	// (e.g. mobile), 5 otherwise.
	let trackWidth = $state(0);
	$effect(() => {
		if (!trackEl) return;
		const ro = new ResizeObserver((entries) => {
			trackWidth = entries[0].contentRect.width;
		});
		ro.observe(trackEl);
		return () => ro.disconnect();
	});
	let labelCount = $derived(trackWidth > 0 && trackWidth < 260 ? 3 : 5);

	function timeFromEvent(e: MouseEvent | PointerEvent): number {
		if (!trackEl) return scrubberStore.playheadTime;
		const rect = trackEl.getBoundingClientRect();
		const pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
		return windowStart + pct * (windowEnd - windowStart);
	}

	let hoverTime = $state<number | null>(null);
	let hoverPercent = $state<number>(0);

	function onMouseMove(e: MouseEvent) {
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
	const PAN_EDGE_ZONE = 0.0375; // ~4% of track width on each side
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

		const clickTime = timeFromEvent(e);
		let zoomed = false;
		let hasMoved = false;

		// In clip mode: click/drag sets the left cutoff
		if (clipStore.enabled) {
			clipStore.startTime = clickTime;
			// Keep endTime at least MIN_CLIP_SECS away
			if (clipStore.endTime - clickTime < 10) {
				clipStore.endTime = clickTime + 10;
			}
		} else {
			scrubberStore.playheadTime = clickTime;
		}

		const snapStart = windowStart;
		const snapEnd = windowEnd;

		// Zoom only triggers when holding still (no mouse movement)
		stopZoom();
		zoomTimer = setTimeout(() => {
			if (!hasMoved) {
				zoomed = true;
				const halfZoom = ZOOMED_WINDOW_SECS / 2;
				animateZoom(snapStart, snapEnd, clickTime - halfZoom, clickTime + halfZoom);
			}
		}, ZOOM_DELAY_MS);

		let panRatio = 0;
		const getPanRatio = () => panRatio;

		const onMove = (ev: PointerEvent) => {
			hasMoved = true;
			// Cancel zoom timer on movement
			if (zoomTimer && !zoomed) {
				clearTimeout(zoomTimer);
				zoomTimer = null;
			}

			const t = timeFromEvent(ev);

			if (clipStore.enabled) {
				clipStore.startTime = Math.min(t, clipStore.endTime - 10);
			} else {
				scrubberStore.playheadTime = t;
			}

			// Edge panning when zoomed
			if (zoomed && zoomOverride && trackEl) {
				const rect = trackEl.getBoundingClientRect();
				const pct = (ev.clientX - rect.left) / rect.width;
				if (pct < PAN_EDGE_ZONE) {
					panRatio = -(1 - pct / PAN_EDGE_ZONE);
					panDirection = -1;
					if (!panFrame) startEdgePan(getPanRatio);
				} else if (pct > 1 - PAN_EDGE_ZONE) {
					panRatio = (pct - (1 - PAN_EDGE_ZONE)) / PAN_EDGE_ZONE;
					panDirection = 1;
					if (!panFrame) startEdgePan(getPanRatio);
				} else {
					panRatio = 0;
					panDirection = 0;
					stopEdgePan();
				}
			}
		};
		const onUp = (ev: PointerEvent) => {
			dragging = false;
			scrubberStore.dragging = false;
			panDirection = 0;
			stopZoom();
			stopEdgePan();
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);

			if (clipStore.enabled) {
				// Stay zoomed in clip mode
			} else {
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

	// Zoom to playhead on clip mode enter, zoom out on exit
	let wasClipEnabled = false;
	$effect(() => {
		if (clipStore.enabled && !wasClipEnabled) {
			const c = scrubberStore.playheadTime;
			const h = ZOOMED_WINDOW_SECS / 2;
			animateZoom(naturalStart, naturalEnd, c - h, c + h);
		} else if (!clipStore.enabled && wasClipEnabled && zoomOverride) {
			animateZoom(zoomOverride.start, zoomOverride.end, naturalStart, naturalEnd, () => { zoomOverride = null; });
		}
		wasClipEnabled = clipStore.enabled;
	});

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

	// Latest timestamp covered by any camera (or the selected camera
	// specifically). Used to snap clip mode onto real footage rather than
	// the "now" window when the camera has stopped uploading — otherwise
	// a user in live mode clicking the scissors would get a clip range
	// that the server 404's because it falls entirely inside a gap.
	function latestCoverageEnd(): number | null {
		let best: number | null = null;
		const selectedId = cameraStore.selectedId;
		const iter = selectedId
			? [[selectedId, scrubberStore.cameraCoverage.get(selectedId) ?? []] as const]
			: scrubberStore.cameraCoverage;
		for (const [, spans] of iter) {
			for (const s of spans) {
				if (best === null || s.end > best) best = s.end;
			}
		}
		return best;
	}

	function enterClipMode() {
		if (clipStore.enabled) {
			clipStore.toggle(windowStart, windowEnd);
			return;
		}
		const latest = latestCoverageEnd();
		// If the default scrubber window has no real footage in it but we
		// do have older coverage, anchor the clip to the end of that
		// older coverage instead. If we have no coverage at all, fall
		// back to the scrubber window (server will 404 but the user
		// gets clear feedback via the "No recording" overlay).
		if (latest !== null && latest < windowStart) {
			const end = latest + 5; // +5s so the last frame is inside the clip
			const start = end - 300; // default clip = 5 min
			clipStore.toggle(start, end);
			// Move the scrubber playhead to the clip start so the zoom
			// animation lands on real footage.
			scrubberStore.playheadTime = start;
			scrubberStore.isLive = false;
			return;
		}
		clipStore.toggle(windowStart, windowEnd);
	}

	// Clip handles
	let clipStartPct = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0 || !clipStore.enabled) return 0;
		return Math.max(0, Math.min(100, ((clipStore.startTime - windowStart) / range) * 100));
	});
	let clipEndPct = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0 || !clipStore.enabled) return 0;
		return Math.max(0, Math.min(100, ((clipStore.endTime - windowStart) / range) * 100));
	});

	let draggingClipHandle = $state<'start' | 'end' | null>(null);

	function onClipHandleDown(handle: 'start' | 'end', e: PointerEvent) {
		e.stopPropagation();
		e.preventDefault();
		draggingClipHandle = handle;
		const MIN_CLIP_SECS = 10;
		const MAX_CLIP_SECS = 5 * 60;
		const onMove = (ev: PointerEvent) => {
			const t = timeFromEvent(ev);
			if (handle === 'start') {
				const min = clipStore.endTime - MAX_CLIP_SECS;
				const max = clipStore.endTime - MIN_CLIP_SECS;
				clipStore.startTime = Math.max(min, Math.min(t, max));
			} else {
				const min = clipStore.startTime + MIN_CLIP_SECS;
				const max = clipStore.startTime + MAX_CLIP_SECS;
				clipStore.endTime = Math.min(max, Math.max(t, min));
			}
			// Keep hover tooltip visible during handle drag
			if (trackEl) {
				const rect = trackEl.getBoundingClientRect();
				hoverPercent = Math.max(0, Math.min(100, ((ev.clientX - rect.left) / rect.width) * 100));
				hoverTime = t;
			}
		};
		const onUp = () => {
			draggingClipHandle = null;
			clipStore.seekRevision++;
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
		};
		window.addEventListener('pointermove', onMove);
		window.addEventListener('pointerup', onUp);
	}
</script>

<div class="flex items-center gap-2 sm:gap-3 px-2 sm:px-4 py-2 bg-background/95 backdrop-blur-sm border-t border-border">
	<span class="text-[11px] sm:text-xs text-muted-foreground font-mono shrink-0 tabular-nums">
		<span class="hidden sm:inline">{formatTime(scrubberStore.playheadTime)}</span>
		<span class="sm:hidden">{formatTimeShort(scrubberStore.playheadTime)}</span>
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

		<!-- Pan edge indicators (visible when zoomed, above the rail) -->
		{#if isZoomed}
			<div
				class="absolute left-1 -top-2 text-2 font-bold pointer-events-none select-none z-20 transition-opacity duration-150"
				class:opacity-100={panDirection === -1}
				class:opacity-30={panDirection !== -1}
			>
				<span class="text-muted-foreground">&laquo;</span>
			</div>
			<div
				class="absolute right-1 -top-2 text-2 font-bold pointer-events-none select-none z-20 transition-opacity duration-150"
				class:opacity-100={panDirection === 1}
				class:opacity-30={panDirection !== 1}
			>
				<span class="text-muted-foreground">&raquo;</span>
			</div>
		{/if}

		<!-- Union coverage (semi-transparent when a camera is selected) -->
		{#each unionBars as bar}
			<div
				class="absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-primary"
				class:opacity-25={hasSelection}
				class:opacity-70={!hasSelection}
				style="left: {bar.left}%; width: {bar.width}%"
			></div>
		{/each}

		<!-- Selected camera coverage (solid, on top) -->
		{#if hasSelection}
			{#each selectedBars as bar}
				<div
					class="absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-primary"
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

		<!-- Clip selection region + handles -->
		{#if clipStore.enabled}
			<div
				class="absolute top-1/2 -translate-y-1/2 h-4 bg-yellow-400/20 border-y border-yellow-400/40 z-10"
				style="left: {clipStartPct}%; width: {clipEndPct - clipStartPct}%"
			></div>
			<!-- Start handle -->
			<div
				class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 w-1 h-5 bg-yellow-400 rounded-sm cursor-ew-resize z-20 hover:bg-yellow-300"
				style="left: {clipStartPct}%"
				onpointerdown={(e) => onClipHandleDown('start', e)}
			></div>
			<!-- End handle -->
			<div
				class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 w-1 h-5 bg-yellow-400 rounded-sm cursor-ew-resize z-20 hover:bg-yellow-300"
				style="left: {clipEndPct}%"
				onpointerdown={(e) => onClipHandleDown('end', e)}
			></div>
		{/if}

		<!-- Hover tooltip -->
		{#if hoverTime !== null}
			<div
				class="absolute bottom-5 -translate-x-1/2 whitespace-nowrap rounded bg-popover px-2 py-1 text-[11px] font-mono text-popover-foreground shadow-lg pointer-events-none border border-border z-20"
				style="left: {hoverPercent}%"
			>
				{formatTime(hoverTime)}
			</div>
		{/if}

		<!-- Playhead (hidden in clip mode) -->
		{#if !clipStore.enabled}
			<div
				class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2 z-10"
				style="left: {playheadPercent}%"
			>
				<div
					class={cn(
						'w-3 h-3 rounded-full transition-transform',
						scrubberStore.isLive
							? 'bg-primary shadow-[0_0_8px_rgba(16,185,129,0.6)]'
							: 'bg-sky-400 shadow-[0_0_8px_theme(colors.sky.400/0.6)]',
						dragging && 'scale-150',
					)}
				></div>
			</div>
		{/if}

		<!-- Time labels -->
		<div class="absolute inset-x-0 bottom-0 flex justify-between pointer-events-none">
			{#each Array(labelCount) as _, i}
				{@const t = windowStart + (i / (labelCount - 1)) * (windowEnd - windowStart)}
				<span class="text-[9px] text-muted-foreground/50 font-mono tabular-nums">{formatTimeShort(t)}</span>
			{/each}
		</div>
	</div>

	<button
		class={cn(
			"shrink-0 w-14 py-1 text-xs font-medium rounded text-center transition-colors",
			scrubberStore.isLive
				? "text-primary cursor-default"
				: "bg-primary text-black hover:brightness-110 cursor-pointer",
		)}
		onclick={() => scrubberStore.goLive()}
		disabled={scrubberStore.isLive}
	>
		LIVE
	</button>

	<button
		class={cn(
			"shrink-0 p-1.5 rounded transition-colors",
			clipStore.enabled
				? "bg-yellow-400/20 text-yellow-400"
				: "text-muted-foreground hover:text-foreground hover:bg-accent",
		)}
		onclick={() => enterClipMode()}
		title="Clip mode"
	>
		<Scissors class="h-3.5 w-3.5" />
	</button>
</div>
<ClipDownloadBar />
