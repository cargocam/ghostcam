<script lang="ts">
	import Hls from 'hls.js';
	import { cn } from '$lib/utils.js';
	import { VideoOff } from 'lucide-svelte';

	let {
		src,
		muted = false,
		class: className = '',
		onError = undefined,
	}: {
		src: string;
		muted?: boolean;
		class?: string;
		onError?: (error: string) => void;
	} = $props();

	let videoEl = $state<HTMLVideoElement | undefined>(undefined);
	let hls: Hls | null = null;
	let loading = $state<boolean>(false);
	let noFootage = $state<boolean>(false);

	$effect(() => {
		if (!videoEl || !src) return;
		const mediaEl = videoEl;
		loading = false;
		noFootage = false;

		if (Hls.isSupported()) {
			const instance = new Hls({
				enableWorker: true,
				liveSyncDurationCount: 3,
				liveMaxLatencyDurationCount: 6,
			});
			hls = instance;
			instance.loadSource(src);
			instance.attachMedia(mediaEl);
			instance.on(Hls.Events.MANIFEST_PARSED, () => {
				noFootage = false;
				mediaEl.play().catch(() => {});
			});
			instance.on(Hls.Events.FRAG_LOADING, () => { loading = true; });
			instance.on(Hls.Events.FRAG_LOADED, () => { loading = false; });
			instance.on(Hls.Events.ERROR, (_event, data) => {
				const errMsg = data.error instanceof Error ? data.error.message : String(data.error ?? '');
				onError?.(`type=${data.type} details=${data.details} fatal=${data.fatal ? '1' : '0'} msg=${errMsg}`);
				if (data.fatal) {
					// Manifest 404 = no footage at this time
					if (data.details === Hls.ErrorDetails.MANIFEST_LOAD_ERROR) {
						noFootage = true;
						loading = false;
						instance.destroy();
						if (hls === instance) hls = null;
					} else if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
						instance.startLoad();
					} else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
						instance.recoverMediaError();
					} else {
						instance.destroy();
						if (hls === instance) hls = null;
					}
				}
			});
		} else if (videoEl.canPlayType('application/vnd.apple.mpegurl')) {
			videoEl.src = src;
			videoEl.addEventListener('loadedmetadata', () => videoEl?.play().catch(() => {}));
		}

		return () => {
			if (hls) { hls.destroy(); hls = null; }
		};
	});
</script>

<div class="relative w-full h-full">
	<video
		bind:this={videoEl}
		autoplay
		playsinline
		{muted}
		class={cn('w-full h-full object-cover', noFootage && 'hidden', className)}
	></video>
	{#if noFootage}
		<div class="absolute inset-0 grid place-items-center bg-black/80">
			<div class="flex flex-col items-center gap-2 text-muted-foreground">
				<VideoOff class="h-8 w-8 opacity-40" />
				<span class="text-xs">No footage</span>
			</div>
		</div>
	{:else if loading}
		<div class="absolute inset-0 grid place-items-center pointer-events-none">
			<div class="h-8 w-8 rounded-full border-2 border-white/30 border-t-white/90 animate-spin"></div>
		</div>
	{/if}
</div>
