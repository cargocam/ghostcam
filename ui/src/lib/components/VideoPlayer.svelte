<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { thumbnailStore } from '$lib/stores/thumbnails.svelte.js';

	let {
		deviceId,
		videoElement = $bindable(undefined),
		active = true,
		muted = true,
	}: {
		deviceId: string;
		videoElement?: HTMLVideoElement;
		active?: boolean;
		muted?: boolean;
	} = $props();

	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === deviceId));
	let videoStream = $derived(camera?.videoStream);
	let audioStream = $derived(camera?.audioStream);

	// Combine video + audio tracks into a single MediaStream
	let combinedStream = $derived.by(() => {
		if (!videoStream) return undefined;
		const tracks = [...videoStream.getTracks()];
		if (audioStream) {
			tracks.push(...audioStream.getTracks());
		}
		return new MediaStream(tracks);
	});

	// Track IDs for change detection
	let currentTrackIds = $state('');

	// Set srcObject when combined stream changes (compare track IDs to avoid restart)
	$effect(() => {
		if (!videoElement || !combinedStream) return;
		const newIds = combinedStream.getTracks().map((t) => t.id).sort().join(',');
		if (newIds !== currentTrackIds) {
			videoElement.srcObject = combinedStream;
			currentTrackIds = newIds;
			videoElement.play().catch(() => {});
		}
	});

	// Sync muted prop to video element
	$effect(() => {
		if (videoElement) {
			videoElement.muted = muted;
		}
	});

	// Thumbnail capture every 2s
	$effect(() => {
		if (!videoElement || !active) return;
		const interval = setInterval(() => {
			if (videoElement && videoElement.videoWidth > 0) {
				const canvas = document.createElement('canvas');
				canvas.width = 160;
				canvas.height = 90;
				const ctx = canvas.getContext('2d');
				if (ctx) {
					ctx.drawImage(videoElement, 0, 0, 160, 90);
					thumbnailStore.set(deviceId, canvas.toDataURL('image/jpeg', 0.5));
				}
			}
		}, 2000);
		return () => clearInterval(interval);
	});
</script>

<video
	bind:this={videoElement}
	autoplay
	playsinline
	muted={muted}
	class="absolute inset-0 w-full h-full object-cover"
></video>
