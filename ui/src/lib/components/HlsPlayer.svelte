<script lang="ts">
	import Hls from 'hls.js';
	import { cn } from '$lib/utils.js';

	let {
		src,
		seekTime = undefined,
		muted = false,
		class: className = '',
		onManifestParsed = undefined,
		onError = undefined,
		onTimeUpdate = undefined,
	}: {
		src: string;
		seekTime?: number;
		muted?: boolean;
		class?: string;
		onManifestParsed?: (details: { startTime: number; endTime: number }) => void;
		onError?: (error: string) => void;
		/** Called with the current epoch time as video plays. */
		onTimeUpdate?: (epochTime: number) => void;
	} = $props();

	let videoEl = $state<HTMLVideoElement | undefined>(undefined);
	let hls: Hls | null = null;
	let loading = $state<boolean>(false);
	let manifestEpochStart = $state<number | null>(null);
	let manifestEpochEnd = $state<number | null>(null);
	let lastForcedSeekTo = $state<number | null>(null);
	let lastKnownSeekPosition = $state<number>(0);
	let lastRebuildAtMs = $state<number>(0);
	let lastSeekInput = $state<number | null>(null);
	type ManifestLike = {
		fragments: Array<{
			relurl?: string;
			url?: string;
			duration?: number;
			start?: number;
			programDateTime?: number | null;
		}>;
	};

	function updateManifestWindow(details: ManifestLike | undefined) {
		if (!details || details.fragments.length === 0) return;
		const frags = details.fragments;
		const firstFrag = frags[0];
		const lastFrag = frags[frags.length - 1];

		// Use #EXT-X-PROGRAM-DATE-TIME if available (epoch ms from server).
		// hls.js parses these into fragment.programDateTime (ms since epoch).
		const firstPdt = firstFrag.programDateTime;
		const lastPdt = lastFrag.programDateTime;

		if (firstPdt != null && lastPdt != null) {
			const start = firstPdt / 1000;
			const end = (lastPdt / 1000) + (lastFrag.duration ?? 0);
			manifestEpochStart = start;
			manifestEpochEnd = end;
			onManifestParsed?.({ startTime: start, endTime: end });
			return;
		}

		// Fallback: derive from first fragment's media start offset
		const lastStart = lastFrag.start ?? 0;
		const lastDuration = lastFrag.duration ?? 0;
		const end = lastStart + lastDuration;
		// Without PDT, epoch start is unknown — approximate from current time
		const now = Date.now() / 1000;
		manifestEpochStart = now - end;
		manifestEpochEnd = now;
		onManifestParsed?.({ startTime: now - end, endTime: now });
	}

	$effect(() => {
		if (!videoEl || !src) return;
		const mediaEl = videoEl;
		let disposed = false;

		if (Hls.isSupported()) {
			const hardResetMediaElement = () => {
				mediaEl.pause();
				mediaEl.removeAttribute('src');
				mediaEl.load();
			};
			const attachHls = (startPosition?: number) => {
				if (disposed) return;
				const instance = new Hls({
					enableWorker: true,
					autoStartLoad: false,
					// Segments are fetched on-demand from the camera via the server.
					// Over the internet this can take 15-60s, so increase timeouts.
					fragLoadingTimeOut: 60000,
					fragLoadingMaxRetry: 3,
					fragLoadingRetryDelay: 2000,
				});
				hls = instance;
				instance.loadSource(src);
				instance.attachMedia(mediaEl);
				instance.on(Hls.Events.MANIFEST_PARSED, (_event, data) => {
					mediaEl.play().catch(() => {});
					const level = data.levels[0];
					updateManifestWindow(level?.details);
					const targetStart = startPosition ?? -1;
					if (startPosition != null && Number.isFinite(startPosition)) mediaEl.currentTime = startPosition;
					instance.startLoad(targetStart);
				});
				instance.on(Hls.Events.LEVEL_LOADED, (_event, data) => {
					updateManifestWindow(data?.details);
				});
				instance.on(Hls.Events.FRAG_LOADING, () => {
					loading = true;
				});
				instance.on(Hls.Events.FRAG_LOADED, () => {
					loading = false;
				});
				instance.on(Hls.Events.ERROR, (_event, data) => {
					const errMsg = data.error instanceof Error ? data.error.message : String(data.error ?? '');
					const payload = [
						`type=${data.type}`,
						`details=${data.details}`,
						`fatal=${data.fatal ? '1' : '0'}`,
						errMsg ? `msg=${errMsg}` : null,
					].filter(Boolean).join(' ');
					onError?.(payload);

					const sourceEnded =
						data.details === 'bufferAppendError' &&
						errMsg.includes('MediaSource readyState: ended');
					if (sourceEnded) {
						const now = Date.now();
						if (now - lastRebuildAtMs < 600) return;
						lastRebuildAtMs = now;
						const resumeAt = Number.isFinite(lastKnownSeekPosition)
							? lastKnownSeekPosition
							: (Number.isFinite(mediaEl.currentTime) ? mediaEl.currentTime : 0);
						instance.destroy();
						hardResetMediaElement();
						attachHls(resumeAt);
						return;
					}

					if (data.fatal) {
						if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
							instance.startLoad();
						} else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
							instance.recoverMediaError();
						} else {
							instance.destroy();
							if (hls === instance) hls = null;
						}
					}
				});
			};
			attachHls();
		} else if (videoEl.canPlayType('application/vnd.apple.mpegurl')) {
			// Safari native HLS
			videoEl.src = src;
			videoEl.addEventListener('loadedmetadata', () => {
				videoEl?.play().catch(() => {});
			});
		}

		return () => {
			disposed = true;
			if (hls) {
				hls.destroy();
				hls = null;
			}
			mediaEl.pause();
			mediaEl.removeAttribute('src');
			mediaEl.load();
			manifestEpochStart = null;
			manifestEpochEnd = null;
			lastForcedSeekTo = null;
			loading = false;
		};
	});

	// Seek when seekTime changes
	$effect(() => {
		if (seekTime == null || !videoEl) return;
		if (manifestEpochStart == null) return;
		const relativeTime = seekTime - manifestEpochStart;
		if (!Number.isFinite(relativeTime) || relativeTime < 0) return;
		let max = manifestEpochEnd != null ? Math.max(0, manifestEpochEnd - manifestEpochStart - 0.05) : relativeTime;
		if (Number.isFinite(videoEl.duration) && videoEl.duration > 0) {
			max = Math.min(max, Math.max(0, videoEl.duration - 0.05));
		}
		const clamped = Math.min(relativeTime, max);
		lastKnownSeekPosition = clamped;
		const inputJump =
			lastSeekInput == null ? Number.POSITIVE_INFINITY : Math.abs(seekTime - lastSeekInput);
		lastSeekInput = seekTime;
		const drift = Math.abs(videoEl.currentTime - clamped);
		const likelyUserScrub = inputJump > 1.25;
		// Avoid forcing seek on every animation-frame playhead tick (causes lag/jitter).
		// Only hard-seek on clear scrub jumps or when playback drifts far away.
		if (likelyUserScrub || drift > 4) {
			const shouldForceSeek =
				lastForcedSeekTo == null || Math.abs(lastForcedSeekTo - clamped) > 0.5;
			if (shouldForceSeek) {
				videoEl.currentTime = clamped;
				lastForcedSeekTo = clamped;
			}
		}
		videoEl.play().catch(() => {});
	});
</script>

<div class="relative w-full h-full">
	<video
		bind:this={videoEl}
		autoplay
		playsinline
		{muted}
		class={cn('w-full h-full object-cover', className)}
		ontimeupdate={() => {
			if (videoEl && onTimeUpdate && manifestEpochStart != null) {
				onTimeUpdate(manifestEpochStart + videoEl.currentTime);
			}
		}}
	></video>
	{#if loading}
		<div class="absolute inset-0 grid place-items-center pointer-events-none">
			<div class="h-8 w-8 rounded-full border-2 border-white/30 border-t-white/90 animate-spin"></div>
		</div>
	{/if}
</div>
