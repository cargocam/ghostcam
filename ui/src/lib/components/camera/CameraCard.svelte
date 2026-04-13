<script lang="ts">
	import LivePlayer from '$lib/components/LivePlayer.svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { clipStore } from '$lib/stores/clip.svelte.js';
	import { reportClientLog } from '$lib/stores/dev.svelte.js';
	import { untrack } from 'svelte';
	import { cn } from '$lib/utils.js';
	import { Camera, PictureInPicture2, Volume2, VolumeOff, VideoOff, Settings } from 'lucide-svelte';
	import CameraSettingsDialog from '$lib/components/camera/CameraSettingsDialog.svelte';

	let {
		deviceId,
		name,
		featured = false,
	}: {
		deviceId: string;
		name: string;
		featured?: boolean;
	} = $props();

	let isOnline = $derived(cameraStore.getCamera(deviceId)?.online ?? false);
	let isPlayback = $derived(scrubberStore.seekTarget !== null);
	let isSelected = $derived(cameraStore.selectedId === deviceId);
	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === deviceId));
	let isMuted = $derived(settingsStore.isCameraMuted(deviceId));

	// Find the coverage span (if any) that contains the current playhead.
	// During VOD playback, playheadTime advances in real time so this
	// re-evaluates continuously — when the playhead enters a gap the src
	// goes empty, and when it exits the gap a fresh manifest is loaded.
	// Returns a stable identity string so downstream derivations only
	// recompute on actual span transitions (not every animation frame).
	let coverageSpanKey = $derived.by(() => {
		if (scrubberStore.seekTarget === null) return 'live';
		const t = scrubberStore.playheadTime;
		const coverage = scrubberStore.cameraCoverage.get(deviceId);
		if (!coverage || coverage.length === 0) return 'none';
		const span = coverage.find((s) => t >= s.start && t <= s.end);
		return span ? `${span.start}:${span.end}` : 'none';
	});

	let coverageSpanAtPlayhead = $derived.by(() => {
		if (coverageSpanKey === 'live') return 'live' as const;
		if (coverageSpanKey === 'none') return null;
		const [start, end] = coverageSpanKey.split(':').map(Number);
		return { start, end };
	});

	// Clip manifest window — 10-min VOD centered on clip midpoint.
	// Re-centers when clip bounds escape the buffered window (debounced to avoid reload spam).
	const CLIP_MANIFEST_PADDING_SEC = 5 * 60; // 5 min each side = 10 min total
	let clipSnapshot = $state<{ from: number; to: number; seekTo: number } | null>(null);
	let clipReloadTimer: ReturnType<typeof setTimeout> | null = null;
	let prevClipEnabled = false;

	function buildClipSnapshot(start: number, end: number) {
		const mid = (start + end) / 2;
		return {
			from: Math.floor((mid - CLIP_MANIFEST_PADDING_SEC) * 1000),
			to: Math.floor((mid + CLIP_MANIFEST_PADDING_SEC) * 1000),
			seekTo: start,
		};
	}

	$effect(() => {
		const enabled = clipStore.enabled;
		if (enabled && !prevClipEnabled) {
			// Clip mode just turned on — initial snapshot
			untrack(() => {
				clipSnapshot = buildClipSnapshot(clipStore.startTime, clipStore.endTime);
			});
		} else if (!enabled) {
			clipSnapshot = null;
			if (clipReloadTimer) { clearTimeout(clipReloadTimer); clipReloadTimer = null; }
		}
		prevClipEnabled = enabled;
	});

	// Watch for clip bounds escaping the manifest window
	$effect(() => {
		if (!clipStore.enabled || !clipSnapshot) return;
		const startMs = Math.floor(clipStore.startTime * 1000);
		const endMs = Math.floor(clipStore.endTime * 1000);
		const needsReload = startMs < clipSnapshot.from || endMs > clipSnapshot.to;
		if (!needsReload) return;

		// Debounce: wait for handle drag to settle before reloading
		if (clipReloadTimer) clearTimeout(clipReloadTimer);
		clipReloadTimer = setTimeout(() => {
			clipReloadTimer = null;
			clipSnapshot = buildClipSnapshot(clipStore.startTime, clipStore.endTime);
		}, 500);

		return () => {
			if (clipReloadTimer) { clearTimeout(clipReloadTimer); clipReloadTimer = null; }
		};
	});

	// Live: sliding window manifest, hls.js polls for new segments.
	// VOD: manifest bounded to the current coverage span so hls.js stops
	//      at the gap boundary instead of skipping ahead. When the playhead
	//      re-enters coverage a fresh manifest is loaded automatically.
	// Clip mode: wide VOD manifest, loop boundaries handle precise range.
	// Empty string when no coverage at playhead — shows "No footage".
	let hlsSrc = $derived.by(() => {
		const id = encodeURIComponent(deviceId);
		if (clipSnapshot) {
			return `/hls/${id}/vod.m3u8?from=${clipSnapshot.from}&to=${clipSnapshot.to}`;
		}
		const span = coverageSpanAtPlayhead;
		if (span === 'live') return `/hls/${id}/live.m3u8`;
		if (span === null) return '';
		// Start from the seek point (or span start if we entered mid-gap),
		// end at the span boundary so hls.js stops before the next gap.
		const seekMs = Math.floor((scrubberStore.seekTarget ?? span.start) * 1000);
		const from = Math.max(seekMs, Math.floor(span.start * 1000));
		const to = Math.floor(span.end * 1000);
		return `/hls/${id}/vod.m3u8?from=${from}&to=${to}`;
	});


	let videoElement = $state<HTMLVideoElement | undefined>(undefined);
	let webrtcActive = $state(false);
	let cardEl = $state<HTMLButtonElement | undefined>(undefined);

	function captureSnapshot(e: MouseEvent) {
		e.stopPropagation();
		if (!videoElement || videoElement.videoWidth === 0) return;
		const canvas = document.createElement('canvas');
		canvas.width = videoElement.videoWidth;
		canvas.height = videoElement.videoHeight;
		const ctx = canvas.getContext('2d');
		if (!ctx) return;
		ctx.drawImage(videoElement, 0, 0);
		const link = document.createElement('a');
		link.download = `${name.replace(/[^a-zA-Z0-9]/g, '_')}_${new Date().toISOString().slice(0, 19).replace(/:/g, '-')}.png`;
		link.href = canvas.toDataURL('image/png');
		link.click();
	}

	function togglePiP(e: MouseEvent) {
		e.stopPropagation();
		if (!videoElement) return;
		if (document.pictureInPictureElement === videoElement) {
			document.exitPictureInPicture().catch(() => {});
		} else {
			videoElement.requestPictureInPicture().catch(() => {});
		}
	}

	function toggleMute(e: MouseEvent) {
		e.stopPropagation();
		settingsStore.toggleCameraMute(deviceId);
	}

	let settingsOpen = $state(false);

	function openSettings(e: MouseEvent) {
		e.stopPropagation();
		settingsOpen = true;
	}
</script>

<button
	bind:this={cardEl}
	class={cn(
		"relative overflow-hidden rounded-lg bg-black group cursor-pointer transition-all w-full aspect-video",
		isSelected ? "ring-2 ring-primary" : "ring-1 ring-border/50 hover:ring-border",
		featured ? "col-span-2 row-span-2" : ""
	)}
	onclick={() => cameraStore.select(deviceId)}
	ondblclick={() => settingsStore.openCameraView(deviceId)}
>
	<div class="absolute inset-0">
		{#if hlsSrc}
			<LivePlayer
				{deviceId}
				src={hlsSrc}
				muted={isMuted}
				seekTo={clipSnapshot ? clipSnapshot.seekTo : (scrubberStore.seekTarget ?? -1)}
				loopStart={clipStore.enabled ? clipStore.startTime : -1}
				loopEnd={clipStore.enabled ? clipStore.endTime : -1}
				loopSeekRevision={clipStore.seekRevision}
				mode={clipStore.enabled ? 'clip' : isPlayback ? 'vod' : 'live'}
				bind:videoEl={videoElement}
				bind:webrtcActive
				onError={(err) => {
					console.warn(`HLS error for ${deviceId}:`, err);
					reportClientLog({
						level: 'error',
						source: 'hls',
						message: err,
						context: {
							device_id: deviceId,
							mode: clipStore.enabled ? 'clip' : isPlayback ? 'vod' : 'live',
							src: hlsSrc,
						},
					});
				}}
			/>
		{:else}
			<div class="w-full h-full grid place-items-center bg-black/80">
				<div class="flex flex-col items-center gap-2 text-muted-foreground px-4 text-center">
					<VideoOff class="h-8 w-8 opacity-40" />
					{#if camera?.recording_mode === 'never'}
						<span class="text-xs max-w-[22ch]">
							Streaming-only mode — no recordings to scrub. Enable recording
							in camera settings to use the timeline.
						</span>
					{:else}
						<span class="text-xs">No footage</span>
					{/if}
				</div>
			</div>
		{/if}
	</div>

	<!-- Top gradient overlay -->
	<div class="absolute inset-x-0 top-0 h-12 bg-gradient-to-b from-black/70 to-transparent pointer-events-none">
		<div class="flex items-center justify-between px-3 pt-2">
			<div class="flex items-center gap-2">
				<span class={cn(
					"h-2 w-2 rounded-full",
					isPlayback ? "bg-sky-400" : isOnline ? "bg-primary animate-pulse" : "bg-muted-foreground/40"
				)}></span>
				<span class="text-xs font-medium text-white/90 drop-shadow-sm">{name}</span>
			</div>
			<span class={cn(
				"text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded",
				clipStore.enabled ? "bg-yellow-400/20 text-yellow-400" : isPlayback ? "bg-sky-400/20 text-sky-400" : isOnline ? (webrtcActive ? "bg-primary/20 text-primary" : "bg-orange-400/20 text-orange-400") : "bg-white/10 text-white/40"
			)}>
				{clipStore.enabled ? 'CLIP' : isPlayback ? 'PLAYBACK' : isOnline ? (webrtcActive ? 'LIVE' : 'DELAYED') : 'OFFLINE'}
			</span>
		</div>
	</div>

	<!-- Bottom gradient overlay -->
	<div class="absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t from-black/70 to-transparent pointer-events-none">
		<div class="flex items-center justify-between px-3 pb-2 absolute bottom-0 inset-x-0">
			{#if camera?.telemetry}
				<div class="flex items-center gap-3 text-[10px] text-white/70 font-mono">
					<span>CPU {(camera.telemetry.cpu_percent ?? 0).toFixed(0)}%</span>
					<span>{(camera.telemetry.memory_mb ?? 0).toFixed(0)}MB</span>
					<span>{(camera.telemetry.temp_celsius ?? 0).toFixed(0)}&deg;C</span>
				</div>
			{/if}
		</div>
	</div>

	<!-- Action buttons (hover) -->
	<div class="absolute bottom-2 right-2 flex items-center gap-1.5 opacity-0 group-hover:opacity-100 transition-opacity">
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="cursor-pointer rounded bg-black/50 p-1" onclick={captureSnapshot} title="Save snapshot">
			<Camera class="h-3 w-3 text-white/70" />
		</div>
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="cursor-pointer rounded bg-black/50 p-1" onclick={togglePiP} title="Picture-in-Picture">
			<PictureInPicture2 class="h-3 w-3 text-white/70" />
		</div>
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="cursor-pointer rounded bg-black/50 p-1" onclick={toggleMute} title="Toggle audio">
			{#if isMuted}
				<VolumeOff class="h-3 w-3 text-white/70" />
			{:else}
				<Volume2 class="h-3 w-3 text-white/70" />
			{/if}
		</div>
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="cursor-pointer rounded bg-black/50 p-1" onclick={openSettings} title="Camera settings">
			<Settings class="h-3 w-3 text-white/70" />
		</div>
	</div>
</button>

<CameraSettingsDialog bind:open={settingsOpen} {deviceId} />
