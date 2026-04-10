<script lang="ts">
	import Hls from 'hls.js';
	import { cn } from '$lib/utils.js';
	import { VideoOff } from 'lucide-svelte';

	let {
		src,
		muted = false,
		seekTo = -1,
		loopStart = -1,
		loopEnd = -1,
		class: className = '',
		onError = undefined,
	}: {
		src: string;
		muted?: boolean;
		seekTo?: number;
		/** Epoch seconds — loop boundaries. When both > 0, video loops within this range. */
		loopStart?: number;
		loopEnd?: number;
		class?: string;
		onError?: (error: string) => void;
	} = $props();

	let videoEl = $state<HTMLVideoElement | undefined>(undefined);
	let hls: Hls | null = null;
	let loading = $state<boolean>(false);
	let noFootage = $state<boolean>(false);
	/** PDT of first fragment in ms — used to map currentTime ↔ epoch time for looping. */
	let firstPDTMs = $state(0);

	// HLS setup — only re-runs when src or seekTo changes
	$effect(() => {
		if (!videoEl || !src) return;
		const mediaEl = videoEl;
		const targetSeek = seekTo;
		loading = false;
		noFootage = false;
		firstPDTMs = 0;

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

				if (targetSeek > 0 && instance.levels?.[0]?.details) {
					const details = instance.levels[0].details;
					const pdt = details.fragments[0]?.programDateTime;
					if (pdt) {
						firstPDTMs = pdt;
						const offsetSec = targetSeek - pdt / 1000;
						if (offsetSec > 0) {
							mediaEl.currentTime = offsetSec;
						}
					}
				}

				mediaEl.play().catch(() => {});
			});
			instance.on(Hls.Events.FRAG_LOADING, () => { loading = true; });
			instance.on(Hls.Events.FRAG_LOADED, () => { loading = false; });
			instance.on(Hls.Events.ERROR, (_event, data) => {
				const errMsg = data.error instanceof Error ? data.error.message : String(data.error ?? '');
				onError?.(`type=${data.type} details=${data.details} fatal=${data.fatal ? '1' : '0'} msg=${errMsg}`);
				if (data.fatal) {
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

	// Loop logic — separate effect so loopStart/loopEnd changes don't reload HLS
	$effect(() => {
		if (!videoEl || loopStart <= 0 || loopEnd <= 0) return;
		const mediaEl = videoEl;
		const ls = loopStart;
		const le = loopEnd;

		const onTimeUpdate = () => {
			if (!firstPDTMs) return;
			const epochNow = firstPDTMs / 1000 + mediaEl.currentTime;
			if (epochNow >= le || epochNow < ls) {
				const startOffset = ls - firstPDTMs / 1000;
				mediaEl.currentTime = Math.max(0, startOffset);
			}
		};
		mediaEl.addEventListener('timeupdate', onTimeUpdate);

		return () => {
			mediaEl.removeEventListener('timeupdate', onTimeUpdate);
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
