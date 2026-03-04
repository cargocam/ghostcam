<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { thumbnailStore } from '$lib/stores/thumbnails.svelte.js';

	let {
		deviceId,
		videoElement = $bindable(undefined),
		active = true,
	}: {
		deviceId: string;
		videoElement?: HTMLVideoElement;
		active?: boolean;
	} = $props();

	let camera = $derived(cameraStore.cameras.find((c) => c.device_id === deviceId));
	let stream = $derived(camera?.stream);

	// Set srcObject when stream changes
	$effect(() => {
		if (!videoElement || !stream) return;
		if (videoElement.srcObject !== stream) {
			videoElement.srcObject = stream;
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
	muted
	class="absolute inset-0 w-full h-full object-cover"
></video>
