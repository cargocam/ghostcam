<script lang="ts">
	import { onMount } from 'svelte';
	import VideoPlayer from '$lib/components/VideoPlayer.svelte';
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cn } from '$lib/utils.js';
	import { Camera, PictureInPicture2, Volume2, VolumeOff } from 'lucide-svelte';

	let {
		deviceId,
		name,
		connected,
		featured = false,
	}: {
		deviceId: string;
		name: string;
		connected: boolean;
		featured?: boolean;
	} = $props();

	let focusedLayoutTargetId = $derived.by(() => {
		if (settingsStore.currentView !== 'live' || settingsStore.gridLayout !== '1+5') {
			return null;
		}
		return cameraStore.selectedId ?? cameraStore.cameras[0]?.device_id ?? null;
	});

	let isPlaybackMode = $derived.by(() => {
		if (scrubberStore.mode !== 'playback') return false;
		if (settingsStore.currentView !== 'live' || settingsStore.gridLayout !== '1+5') return true;
		return deviceId === focusedLayoutTargetId;
	});

	// HLS manifest URL — used for both playback scrubbing and live fallback
	let hlsSrc = $derived(`/hls/${encodeURIComponent(deviceId)}/playlist.m3u8`);
	let playbackSeekTime = $derived(isPlaybackMode ? scrubberStore.playheadTime : undefined);

	// Use HLS live view when there's no WebRTC stream (e.g. Docker Desktop on Mac,
	// where UDP port forwarding from host → container is broken).
	let hasWebRtcStream = $derived(!!camera?.videoStream);

	let isSelected = $derived(cameraStore.selectedId === deviceId);
	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === deviceId));
	let isMuted = $derived(settingsStore.isCameraMuted(deviceId));
	let videoElement = $state<HTMLVideoElement | undefined>(undefined);
	let cardEl = $state<HTMLButtonElement | undefined>(undefined);
	let isVisible = $state(true);
	let playbackWindow = $state<{ start: number; end: number } | null>(null);
	let hlsLastError = $state<string | null>(null);
	let SHOW_PLAYBACK_DEBUG = $derived(settingsStore.debugMode);

	let noFootage = $derived.by(() => {
		if (!isPlaybackMode) return false;
		if (!playbackWindow) return false;
		return scrubberStore.playheadTime < playbackWindow.start || scrubberStore.playheadTime > playbackWindow.end;
	});

	onMount(() => {
		if (!cardEl || typeof IntersectionObserver === 'undefined') return;
		const observer = new IntersectionObserver(
			([entry]) => { isVisible = entry.isIntersecting; },
			{ threshold: 0 }
		);
		observer.observe(cardEl);
		return () => observer.disconnect();
	});

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
	<!-- Live video feed: WebRTC if connected, HLS live fallback otherwise -->
	{#if !isPlaybackMode && connected}
		<div class="absolute inset-0">
			{#if hasWebRtcStream}
				<VideoPlayer {deviceId} bind:videoElement active={isVisible} muted={isMuted} />
			{:else}
				<HlsPlayer src={hlsSrc} muted={isMuted} />
			{/if}
		</div>
	{/if}

	<!-- Playback video feed (HLS) — visible only during playback -->
	{#if isPlaybackMode && hlsSrc && !noFootage}
		<div class="absolute inset-0">
			<HlsPlayer
				src={hlsSrc}
				seekTime={playbackSeekTime}
				muted={isMuted}
				onManifestParsed={(details) => {
					hlsLastError = null;
					playbackWindow = { start: details.startTime, end: details.endTime };
					// Only expand the available window — don't overwrite per-segment coverage
					// bars which are already populated from the coverage API with gap-aware data.
					const currentWindow = scrubberStore.availableWindow;
					scrubberStore.setAvailableWindow(
						currentWindow
							? {
									start: Math.min(currentWindow.start, details.startTime),
									end: Math.max(currentWindow.end, details.endTime),
								}
							: { start: details.startTime, end: details.endTime },
					);
				}}
				onError={(err) => {
					hlsLastError = err;
					console.warn(`HLS error for ${deviceId}:`, err);
				}}
			/>
		</div>
	{/if}

	{#if SHOW_PLAYBACK_DEBUG}
		<div class="absolute left-2 top-14 z-20 rounded bg-black/70 px-2 py-1 text-[10px] font-mono text-white/80 pointer-events-none">
			<div>{isPlaybackMode ? 'mode=playback' : 'mode=live'} selected={isSelected ? '1' : '0'}</div>
			<div>playhead={scrubberStore.playheadTime.toFixed(2)} inWindow={noFootage ? '0' : '1'}</div>
			<div>
				window={playbackWindow ? `${playbackWindow.start.toFixed(2)}..${playbackWindow.end.toFixed(2)}` : 'none'}
			</div>
			{#if hlsLastError}
				<div class="text-red-300">hlsError={hlsLastError}</div>
			{/if}
		</div>
	{/if}

	{#if noFootage}
		<div class="absolute inset-0 z-10 grid place-items-center bg-black/70 text-center px-3">
			<div class="text-xs text-white/85">
				<div class="font-semibold mb-1">No footage at this time</div>
				<div class="text-white/60 font-mono">
					Available: {new Date((playbackWindow?.start ?? 0) * 1000).toLocaleTimeString()} - {new Date((playbackWindow?.end ?? 0) * 1000).toLocaleTimeString()}
				</div>
			</div>
		</div>
	{/if}

	<!-- Top gradient overlay -->
	<div class="absolute inset-x-0 top-0 h-12 bg-gradient-to-b from-black/70 to-transparent pointer-events-none">
		<div class="flex items-center justify-between px-3 pt-2">
			<div class="flex items-center gap-2">
				<span class={cn(
					"h-2 w-2 rounded-full",
					connected ? "bg-primary animate-pulse" : "bg-destructive"
				)}></span>
				<span class="text-xs font-medium text-white/90 drop-shadow-sm">{name}</span>
			</div>
			<span class={cn(
				"text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded",
				isPlaybackMode
					? "bg-sky-500/20 text-sky-400"
					: connected ? "bg-primary/20 text-primary" : "bg-destructive/20 text-destructive"
			)}>
				{isPlaybackMode ? 'PLAYBACK' : connected ? 'LIVE' : 'OFF'}
			</span>
		</div>
	</div>

	<!-- Bottom gradient overlay -->
	<div class="absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t from-black/70 to-transparent pointer-events-none">
		<div class="flex items-center justify-between px-3 pb-2 absolute bottom-0 inset-x-0">
			{#if !isPlaybackMode && camera?.telemetry}
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
	</div>
</button>
