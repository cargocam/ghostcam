<script lang="ts">
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import { Button } from '$lib/components/ui/button/index.js';
	import { ArrowLeft, Maximize, Minimize, Volume2, VolumeOff, Camera, PictureInPicture2 } from 'lucide-svelte';
	import { cn } from '$lib/utils.js';
	import { formatUptime } from '$lib/utils/format.js';

	let cameraId = $derived(settingsStore.focusedCameraId);
	let camera = $derived(cameraId ? cameraStore.cameras.find((c) => c.device_id === cameraId) : null);
	let displayName = $derived(camera ? cameraConfigStore.getDisplayName(camera.device_id, camera.device_name) : '');
	let isMuted = $derived(cameraId ? settingsStore.isCameraMuted(cameraId) : true);

	let showOverlay = $state(true);
	let overlayTimer: ReturnType<typeof setTimeout> | null = null;
	let isFullscreen = $state(false);
	let containerEl = $state<HTMLDivElement | undefined>(undefined);
	let videoElement = $state<HTMLVideoElement | undefined>(undefined);

	function goBack() {
		settingsStore.closeCameraView();
	}

	function resetOverlayTimer() {
		showOverlay = true;
		if (overlayTimer) clearTimeout(overlayTimer);
		overlayTimer = setTimeout(() => { showOverlay = false; }, 3000);
	}

	function toggleFullscreen() {
		if (!containerEl) return;
		if (document.fullscreenElement) {
			document.exitFullscreen();
		} else {
			containerEl.requestFullscreen();
		}
	}

	function toggleMute() {
		if (cameraId) {
			settingsStore.toggleCameraMute(cameraId);
		}
	}

	$effect(() => {
		function onFullscreenChange() { isFullscreen = !!document.fullscreenElement; }
		document.addEventListener('fullscreenchange', onFullscreenChange);
		return () => document.removeEventListener('fullscreenchange', onFullscreenChange);
	});

	$effect(() => {
		function onKeyDown(e: KeyboardEvent) {
			if (settingsStore.currentView !== 'camera') return;
			if (e.key === 'Escape') {
				if (isFullscreen) document.exitFullscreen();
				else goBack();
			} else if (e.key === 'f' || e.key === 'F') toggleFullscreen();
			else if (e.key === 'm' || e.key === 'M') toggleMute();
			else if (e.key === 's' || e.key === 'S') captureSnapshot();
			else if (e.key === 'p' || e.key === 'P') togglePiP();
		}
		window.addEventListener('keydown', onKeyDown);
		return () => window.removeEventListener('keydown', onKeyDown);
	});

	function captureSnapshot() {
		if (!videoElement || videoElement.videoWidth === 0) return;
		const canvas = document.createElement('canvas');
		canvas.width = videoElement.videoWidth;
		canvas.height = videoElement.videoHeight;
		const ctx = canvas.getContext('2d');
		if (!ctx) return;
		ctx.drawImage(videoElement, 0, 0);
		const link = document.createElement('a');
		link.download = `${(displayName || 'camera').replace(/[^a-zA-Z0-9]/g, '_')}_${new Date().toISOString().slice(0, 19).replace(/:/g, '-')}.png`;
		link.href = canvas.toDataURL('image/png');
		link.click();
	}

	function togglePiP() {
		if (!videoElement) return;
		if (document.pictureInPictureElement === videoElement) {
			document.exitPictureInPicture().catch(() => {});
		} else {
			videoElement.requestPictureInPicture().catch(() => {});
		}
	}

	$effect(() => {
		resetOverlayTimer();
		return () => { if (overlayTimer) clearTimeout(overlayTimer); };
	});
</script>

{#if cameraId && camera}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		bind:this={containerEl}
		class="relative h-full w-full bg-black overflow-hidden"
		onpointermove={resetOverlayTimer}
		onclick={resetOverlayTimer}
	>
		<div class="absolute inset-0">
			<HlsPlayer src={`/hls/${encodeURIComponent(cameraId)}/live.m3u8`} muted={isMuted} />
		</div>

		<!-- Top overlay -->
		<div
			class={cn(
				"absolute inset-x-0 top-0 z-10 bg-gradient-to-b from-black/80 to-transparent transition-opacity duration-300",
				showOverlay ? "opacity-100" : "opacity-0 pointer-events-none"
			)}
		>
			<div class="flex items-center justify-between px-4 py-3">
				<div class="flex items-center gap-3">
					<Button variant="ghost" size="icon" class="text-white hover:bg-white/10" onclick={goBack}>
						<ArrowLeft class="h-5 w-5" />
					</Button>
					<div class="flex items-center gap-2">
						<span class={cn(
							"h-2.5 w-2.5 rounded-full",
							camera.online ? "bg-primary animate-pulse" : "bg-destructive"
						)}></span>
						<span class="text-sm font-medium text-white">{displayName}</span>
						<span class={cn(
							"text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded",
							camera.online ? "bg-primary/20 text-primary" : "bg-destructive/20 text-destructive"
						)}>
							{camera.online ? 'LIVE' : 'OFF'}
						</span>
					</div>
				</div>
				<div class="flex items-center gap-1">
					<Button variant="ghost" size="icon" class="text-white hover:bg-white/10" onclick={captureSnapshot} title="Snapshot (S)">
						<Camera class="h-4 w-4" />
					</Button>
					<Button variant="ghost" size="icon" class="text-white hover:bg-white/10" onclick={togglePiP} title="Picture-in-Picture (P)">
						<PictureInPicture2 class="h-4 w-4" />
					</Button>
					<Button variant="ghost" size="icon" class="text-white hover:bg-white/10" onclick={toggleMute} title="Mute (M)">
						{#if isMuted}
							<VolumeOff class="h-5 w-5" />
						{:else}
							<Volume2 class="h-5 w-5" />
						{/if}
					</Button>
					<Button variant="ghost" size="icon" class="text-white hover:bg-white/10" onclick={toggleFullscreen} title="Fullscreen (F)">
						{#if isFullscreen}
							<Minimize class="h-5 w-5" />
						{:else}
							<Maximize class="h-5 w-5" />
						{/if}
					</Button>
				</div>
			</div>
		</div>

		<!-- Bottom overlay -->
		<div
			class={cn(
				"absolute inset-x-0 bottom-0 z-10 bg-gradient-to-t from-black/80 to-transparent transition-opacity duration-300",
				showOverlay ? "opacity-100" : "opacity-0 pointer-events-none"
			)}
		>
			<div class="px-4 pb-3 pt-8">
				{#if camera.telemetry}
					<div class="flex items-center gap-4 text-xs text-white/70 font-mono">
						<span>CPU {(camera.telemetry.cpu_percent ?? 0).toFixed(1)}%</span>
						<span>{(camera.telemetry.memory_mb ?? 0).toFixed(0)}MB</span>
						<span>{(camera.telemetry.temp_celsius ?? 0).toFixed(0)}&deg;C</span>
						<span>Up {formatUptime(camera.telemetry.uptime_secs ?? 0)}</span>
					</div>
				{/if}
			</div>
		</div>

		<div
			class={cn(
				"absolute bottom-4 right-4 z-10 text-[10px] text-white/30 transition-opacity duration-300",
				showOverlay ? "opacity-100" : "opacity-0"
			)}
		>
			F fullscreen &middot; M mute &middot; S snapshot &middot; P pip &middot; Esc back
		</div>
	</div>
{:else}
	<div class="flex h-full items-center justify-center text-muted-foreground">
		Camera not found
	</div>
{/if}
