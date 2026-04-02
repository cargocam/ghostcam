<script lang="ts">
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cn } from '$lib/utils.js';

	let trackEl = $state<HTMLDivElement | undefined>(undefined);
	let dragging = $state(false);

	const MIN_WINDOW_SECS = 5 * 60;
	const MARGIN_SECS = 30;

	// Freeze the window edge when seeking so timeline doesn't shift
	let frozenEnd = $state(Date.now() / 1000);
	$effect(() => {
		if (scrubberStore.isLive) frozenEnd = scrubberStore.playheadTime;
	});

	let windowEnd = $derived((scrubberStore.isLive ? scrubberStore.playheadTime : frozenEnd) + MARGIN_SECS);
	let windowStart = $derived.by(() => {
		const avail = scrubberStore.availableWindow;
		if (avail) {
			const duration = windowEnd - avail.start;
			if (duration < MIN_WINDOW_SECS) return windowEnd - MIN_WINDOW_SECS;
			return avail.start - MARGIN_SECS;
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
		// Move playhead visually but don't commit yet
		scrubberStore.isLive = false;
		scrubberStore.playheadTime = timeFromEvent(e);

		const onMove = (ev: PointerEvent) => {
			scrubberStore.playheadTime = timeFromEvent(ev);
		};
		const onUp = (ev: PointerEvent) => {
			dragging = false;
			window.removeEventListener('pointermove', onMove);
			window.removeEventListener('pointerup', onUp);
			// Commit on release — this triggers manifest reload
			scrubberStore.seekTo(timeFromEvent(ev));
		};
		window.addEventListener('pointermove', onMove);
		window.addEventListener('pointerup', onUp);
	}

	// Union all camera coverage into bars, preserving motion state
	let coverageBars = $derived.by(() => {
		const range = windowEnd - windowStart;
		if (range <= 0) return [];

		// Collect all segments from all cameras
		const all: { start: number; end: number; hasMotion: boolean }[] = [];
		for (const [, coverage] of scrubberStore.cameraCoverage) {
			for (const s of coverage) all.push({ start: s.start, end: s.end, hasMotion: s.hasMotion ?? false });
		}
		if (all.length === 0) return [];

		// Sort and merge overlapping/adjacent segments (only if same motion state)
		all.sort((a, b) => a.start - b.start);
		const merged: { start: number; end: number; hasMotion: boolean }[] = [{ ...all[0] }];
		for (let i = 1; i < all.length; i++) {
			const last = merged[merged.length - 1];
			if (all[i].start <= last.end + 30 && all[i].hasMotion === last.hasMotion) {
				last.end = Math.max(last.end, all[i].end);
			} else {
				merged.push({ ...all[i] });
			}
		}

		// Convert to percentages
		return merged
			.map((s) => {
				const left = Math.max(0, ((s.start - windowStart) / range) * 100);
				const right = Math.min(100, ((s.end - windowStart) / range) * 100);
				return { left, width: right - left, hasMotion: s.hasMotion };
			})
			.filter((s) => s.width > 0);
	});
</script>

<div class="flex items-center gap-3 px-4 py-2 bg-black/60 backdrop-blur-sm border-t border-white/10">
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
	>
		<div class="absolute inset-x-0 top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-white/10"></div>

		{#each coverageBars as bar}
			<div
				class="absolute top-1/2 -translate-y-1/2 h-1.5 rounded-full {bar.hasMotion ? 'bg-amber-500/60' : 'bg-green-500/60'}"
				style="left: {bar.left}%; width: {bar.width}%"
				title={bar.hasMotion ? 'Motion detected' : 'Recording'}
			></div>
		{/each}

		<!-- Playhead + tooltip -->
		<div
			class="absolute top-1/2 -translate-y-1/2 -translate-x-1/2"
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
