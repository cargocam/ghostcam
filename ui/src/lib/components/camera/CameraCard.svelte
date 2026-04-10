<script lang="ts">
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { clipStore } from '$lib/stores/clip.svelte.js';
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

	// Check if the seek target falls within this camera's coverage
	let hasCoverageAtSeek = $derived.by(() => {
		const target = scrubberStore.seekTarget;
		if (target === null) return true; // live mode — always show
		const coverage = scrubberStore.cameraCoverage.get(deviceId);
		if (!coverage || coverage.length === 0) return false;
		// Check if seek time falls within any coverage span (already merged with 30s gap threshold)
		return coverage.some((s) => target >= s.start && target <= s.end);
	});

	// Stable clip manifest range — only updates when clip mode toggles on, not on handle drag.
	// Covers a 10-minute window centered on the initial clip to allow handle movement without reload.
	// Snapshot clip state on mode enter — stable across handle drags to avoid HLS reloads.
	let clipSnapshot = $state<{ from: number; to: number; seekTo: number } | null>(null);
	let prevClipEnabled = false;
	$effect(() => {
		const enabled = clipStore.enabled;
		if (enabled && !prevClipEnabled) {
			untrack(() => {
				const mid = (clipStore.startTime + clipStore.endTime) / 2;
				clipSnapshot = {
					from: Math.floor((mid - 5 * 60) * 1000),
					to: Math.floor((mid + 5 * 60) * 1000),
					seekTo: clipStore.startTime,
				};
			});
		} else if (!enabled) {
			clipSnapshot = null;
		}
		prevClipEnabled = enabled;
	});

	// Live: sliding window manifest, hls.js polls for new segments.
	// VOD: 30-min window from seek point for continuous archive playback.
	// Clip mode: wide VOD manifest, loop boundaries handle precise range.
	// Empty string when no coverage at seek time — HlsPlayer won't load.
	let hlsSrc = $derived.by(() => {
		const id = encodeURIComponent(deviceId);
		if (clipSnapshot) {
			return `/hls/${id}/vod.m3u8?from=${clipSnapshot.from}&to=${clipSnapshot.to}`;
		}
		const target = scrubberStore.seekTarget;
		if (target === null) return `/hls/${id}/live.m3u8`;
		if (!hasCoverageAtSeek) return '';
		const from = Math.floor(target * 1000);
		const to = from + 30 * 60 * 1000;
		return `/hls/${id}/vod.m3u8?from=${from}&to=${to}`;
	});


	let videoElement = $state<HTMLVideoElement | undefined>(undefined);
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
			<HlsPlayer
				src={hlsSrc}
				muted={isMuted}
				seekTo={clipSnapshot ? clipSnapshot.seekTo : (scrubberStore.seekTarget ?? -1)}
				loopStart={clipStore.enabled ? clipStore.startTime : -1}
				loopEnd={clipStore.enabled ? clipStore.endTime : -1}
				onError={(err) => console.warn(`HLS error for ${deviceId}:`, err)}
			/>
		{:else}
			<div class="w-full h-full grid place-items-center bg-black/80">
				<div class="flex flex-col items-center gap-2 text-muted-foreground">
					<VideoOff class="h-8 w-8 opacity-40" />
					<span class="text-xs">No footage</span>
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
				clipStore.enabled ? "bg-yellow-400/20 text-yellow-400" : isPlayback ? "bg-sky-400/20 text-sky-400" : isOnline ? "bg-primary/20 text-primary" : "bg-white/10 text-white/40"
			)}>
				{clipStore.enabled ? 'CLIP' : isPlayback ? 'PLAYBACK' : isOnline ? 'LIVE' : 'OFFLINE'}
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
