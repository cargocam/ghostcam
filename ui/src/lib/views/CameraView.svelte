<script lang="ts">
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { clipStore } from '$lib/stores/clip.svelte.js';
	import { reportClientLog } from '$lib/stores/dev.svelte.js';
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import { Button } from '$lib/components/ui/button/index.js';
	import { ArrowLeft, Maximize, Minimize, Volume2, VolumeOff, Camera, PictureInPicture2 } from 'lucide-svelte';
	import { cn } from '$lib/utils.js';
	import { formatUptime } from '$lib/utils/format.js';

	let cameraId = $derived(settingsStore.focusedCameraId);
	let camera = $derived(cameraId ? cameraStore.cameras.find((c) => c.device_id === cameraId) : null);
	let displayName = $derived(camera ? cameraConfigStore.getDisplayName(camera.device_id, camera.device_name) : '');
	let isMuted = $derived(cameraId ? settingsStore.isCameraMuted(cameraId) : true);
	let isPlayback = $derived(scrubberStore.seekTarget !== null);

	// Mirror CameraCard's stream-selection logic so double-tapping into the
	// focused view doesn't reset a historical scrub or clip back to a
	// (possibly empty) live manifest. Clip mode → clip VOD manifest,
	// scrubbed seek → VOD manifest at the seek position, otherwise live.
	const CLIP_PADDING_SEC = 5 * 60;
	let clipSnapshot = $derived.by(() => {
		if (!clipStore.enabled) return null;
		const mid = (clipStore.startTime + clipStore.endTime) / 2;
		return {
			from: Math.floor((mid - CLIP_PADDING_SEC) * 1000),
			to: Math.floor((mid + CLIP_PADDING_SEC) * 1000),
			seekTo: clipStore.startTime,
		};
	});

	let hlsSrc = $derived.by(() => {
		if (!cameraId) return '';
		const id = encodeURIComponent(cameraId);
		if (clipSnapshot) {
			return `/hls/${id}/vod.m3u8?from=${clipSnapshot.from}&to=${clipSnapshot.to}`;
		}
		const target = scrubberStore.seekTarget;
		if (target === null) return `/hls/${id}/live.m3u8`;
		const from = Math.floor(target * 1000);
		const to = from + 30 * 60 * 1000;
		return `/hls/${id}/vod.m3u8?from=${from}&to=${to}`;
	});

	let streamMode = $derived<'live' | 'vod' | 'clip'>(
		clipStore.enabled ? 'clip' : isPlayback ? 'vod' : 'live'
	);

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
		if (document.fullscreenElement) {
			document.exitFullscreen();
			return;
		}

		const v = videoElement as HTMLVideoElement & {
			webkitEnterFullscreen?: () => void;
			webkitDisplayingFullscreen?: boolean;
		} | undefined;

		// Touch devices (phones, tablets) get native video-element fullscreen.
		// The custom overlay UI is less useful there and the native video
		// controls are what users expect on mobile, plus it's the only path
		// that works on iOS Safari (container-level fullscreen is desktop-only
		// on WebKit). On Android it also avoids the case where the custom
		// overlay is already filling the visible area via h-[100svh] so
		// container fullscreen is visually a no-op.
		const isTouch =
			typeof window !== 'undefined' &&
			(window.matchMedia('(hover: none)').matches || 'ontouchstart' in window);

		if (isTouch && v) {
			if (typeof v.requestFullscreen === 'function') {
				v.requestFullscreen().catch(() => {
					if (v.webkitEnterFullscreen) v.webkitEnterFullscreen();
				});
			} else if (v.webkitEnterFullscreen) {
				v.webkitEnterFullscreen();
			}
			return;
		}

		// Desktop: prefer container-level fullscreen to keep the custom
		// overlay (snapshot, PiP, back buttons) visible over the video.
		if (!containerEl) return;
		containerEl.requestFullscreen().catch(() => {
			if (v?.requestFullscreen) v.requestFullscreen().catch(() => {});
			else if (v?.webkitEnterFullscreen) v.webkitEnterFullscreen();
		});
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
			<HlsPlayer
				src={hlsSrc}
				muted={isMuted}
				seekTo={clipSnapshot ? clipSnapshot.seekTo : (scrubberStore.seekTarget ?? -1)}
				loopStart={clipStore.enabled ? clipStore.startTime : -1}
				loopEnd={clipStore.enabled ? clipStore.endTime : -1}
				loopSeekRevision={clipStore.seekRevision}
				mode={streamMode}
				bind:videoEl={videoElement}
				onError={(err) => {
					console.warn(`HLS error (CameraView) for ${cameraId}:`, err);
					reportClientLog({
						level: 'error',
						source: 'hls',
						message: err,
						context: {
							device_id: cameraId ?? '',
							mode: streamMode,
							src: hlsSrc,
							surface: 'camera-view',
						},
					});
				}}
			/>
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
							camera.online ? "bg-primary animate-pulse" : "bg-muted-foreground/40"
						)}></span>
						<span class="text-sm font-medium text-white">{displayName}</span>
						<span class={cn(
							"text-[10px] uppercase tracking-wider font-semibold px-1.5 py-0.5 rounded",
							camera.online ? "bg-primary/20 text-primary" : "bg-white/10 text-white/40"
						)}>
							{camera.online ? 'LIVE' : 'OFFLINE'}
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
