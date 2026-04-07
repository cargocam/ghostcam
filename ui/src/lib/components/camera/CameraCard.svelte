<script lang="ts">
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { cn } from '$lib/utils.js';
	import { Camera, PictureInPicture2, Volume2, VolumeOff, Settings } from 'lucide-svelte';
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

	let isOnline = $derived(cameraStore.isOnline(deviceId));
	let isSelected = $derived(cameraStore.selectedId === deviceId);
	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === deviceId));
	let isMuted = $derived(settingsStore.isCameraMuted(deviceId));

	// Live: sliding window manifest, hls.js polls for new segments.
	// VOD: tight window from seek point, finite playlist.
	let hlsSrc = $derived.by(() => {
		const id = encodeURIComponent(deviceId);
		const target = scrubberStore.seekTarget;
		if (target === null) return `/hls/${id}/live.m3u8`;
		const from = Math.floor(target * 1000);
		const to = from + 2 * 60 * 1000;
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
		<HlsPlayer
			src={hlsSrc}
			muted={isMuted}
			onError={(err) => console.warn(`HLS error for ${deviceId}:`, err)}
		/>
	</div>

	<!-- Top gradient overlay -->
	<div class="absolute inset-x-0 top-0 h-12 bg-gradient-to-b from-black/70 to-transparent pointer-events-none">
		<div class="flex items-center justify-between px-3 pt-2">
			<div class="flex items-center gap-2">
				<span class={cn(
					"h-2 w-2 rounded-full",
					isOnline ? "bg-primary animate-pulse" : "bg-muted-foreground/40"
				)}></span>
				<span class="text-xs font-medium text-white/90 drop-shadow-sm">{name}</span>
			</div>
			<span class={cn(
				"text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded",
				isOnline ? "bg-primary/20 text-primary" : "bg-white/10 text-white/40"
			)}>
				{isOnline ? 'LIVE' : 'OFFLINE'}
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
