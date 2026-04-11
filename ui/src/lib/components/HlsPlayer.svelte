<script lang="ts">
	import Hls from 'hls.js';
	import { cn } from '$lib/utils.js';
	import { VideoOff, Play } from 'lucide-svelte';

	let {
		src,
		muted = false,
		seekTo = -1,
		loopStart = -1,
		loopEnd = -1,
		loopSeekRevision = 0,
		mode = 'live',
		class: className = '',
		onError = undefined,
		videoEl = $bindable<HTMLVideoElement | undefined>(undefined),
	}: {
		src: string;
		muted?: boolean;
		seekTo?: number;
		/** Epoch seconds — loop boundaries. When both > 0, video loops within this range. */
		loopStart?: number;
		loopEnd?: number;
		/** Bumped to force seek to loopStart (e.g. after handle release). */
		loopSeekRevision?: number;
		/** What kind of stream this is. Controls the "no footage" message
		 *  so live failures say "camera has no recent footage" while VOD
		 *  failures say "no recording for this time range". */
		mode?: 'live' | 'vod' | 'clip';
		class?: string;
		onError?: (error: string) => void;
		/** Exposed so parents can call webkitEnterFullscreen / requestPictureInPicture /
		 *  draw snapshots. Bound via `bind:videoEl`. */
		videoEl?: HTMLVideoElement;
	} = $props();

	// Human-readable label for the "no footage" state. The diagnostic
	// string (HTTP status, hls.js detail) still renders below it.
	let noFootageHeadline = $derived(
		mode === 'live'
			? 'Camera has no recent footage'
			: 'No recording for this time range'
	);

	let hls: Hls | null = null;
	let loading = $state<boolean>(false);
	let noFootage = $state<boolean>(false);
	/** Short diagnostic shown under "No footage" so real errors aren't invisible. */
	let noFootageDetail = $state<string>('');
	/** PDT of first fragment in ms — used to map currentTime ↔ epoch time for looping. */
	let firstPDTMs = $state(0);
	/** Set when an auto-play() promise rejected (mobile autoplay policy, typically
	 *  after a mid-session src change like entering clip mode). A tap-to-play
	 *  overlay is rendered and calling play() from the click restores the gesture. */
	let needsUserPlay = $state<boolean>(false);

	function tryAutoplay(el: HTMLVideoElement | undefined) {
		if (!el) return;
		el.play().then(() => {
			// play() resolved, but verify the element actually started —
			// decode errors and some MSE hiccups can re-pause the video
			// immediately after play() resolves.
			requestAnimationFrame(() => {
				needsUserPlay = el.paused;
			});
		}).catch(() => {
			// Chrome's mobile autoplay policy rejects play() called outside a
			// user gesture when the media isn't muted. Surface a tap-to-play
			// overlay; the resulting click is a user gesture so play()
			// succeeds from there.
			if (el.paused) needsUserPlay = true;
		});
	}

	function manualResume() {
		needsUserPlay = false;
		videoEl?.play().catch(() => {
			// Shouldn't normally reject (we have a gesture), but if it does
			// the pause listener will re-show the overlay.
		});
	}

	// Reflect the video's actual paused state in `needsUserPlay` so the
	// tap-to-resume overlay appears whenever playback halts (autoplay
	// rejection, decode error, reaching VOD end, etc.).
	$effect(() => {
		if (!videoEl) return;
		const el = videoEl;
		const onPause = () => { needsUserPlay = true; };
		const onPlaying = () => { needsUserPlay = false; };
		el.addEventListener('pause', onPause);
		el.addEventListener('playing', onPlaying);
		return () => {
			el.removeEventListener('pause', onPause);
			el.removeEventListener('playing', onPlaying);
		};
	});

	// HLS setup — only re-runs when src or seekTo changes
	$effect(() => {
		if (!videoEl || !src) return;
		const mediaEl = videoEl;
		const targetSeek = seekTo;
		loading = false;
		noFootage = false;
		noFootageDetail = '';
		firstPDTMs = 0;
		needsUserPlay = false;

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

				tryAutoplay(mediaEl);
			});
			instance.on(Hls.Events.FRAG_LOADING, () => { loading = true; });
			instance.on(Hls.Events.FRAG_LOADED, () => { loading = false; });
			instance.on(Hls.Events.ERROR, (_event, data) => {
				const errMsg = data.error instanceof Error ? data.error.message : String(data.error ?? '');
				const status = (data.response && data.response.code) || '';
				onError?.(`type=${data.type} details=${data.details} fatal=${data.fatal ? '1' : '0'} status=${status} msg=${errMsg}`);
				if (data.fatal) {
					if (
						data.details === Hls.ErrorDetails.MANIFEST_LOAD_ERROR ||
						data.details === Hls.ErrorDetails.MANIFEST_PARSING_ERROR ||
						data.details === Hls.ErrorDetails.MANIFEST_INCOMPATIBLE_CODECS_ERROR
					) {
						noFootage = true;
						noFootageDetail = status
							? `HTTP ${status} · ${data.details}`
							: data.details;
						loading = false;
						instance.destroy();
						if (hls === instance) hls = null;
					} else if (data.type === Hls.ErrorTypes.NETWORK_ERROR) {
						instance.startLoad();
					} else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
						instance.recoverMediaError();
					} else {
						noFootage = true;
						noFootageDetail = `${data.type} · ${data.details}`;
						instance.destroy();
						if (hls === instance) hls = null;
					}
				}
			});
		} else if (videoEl.canPlayType('application/vnd.apple.mpegurl')) {
			// Native HLS path — used on iOS Safari, where hls.js doesn't work
			// (iOS doesn't expose MediaSource on iPhone). We still need to
			// feed firstPDTMs so the loop/clip logic works; HTMLMediaElement
			// populates getStartDate() from #EXT-X-PROGRAM-DATE-TIME.
			videoEl.src = src;
			const onMetadata = () => {
				if (!videoEl) return;
				const getStartDate = (videoEl as any).getStartDate as undefined | (() => Date);
				if (typeof getStartDate === 'function') {
					const start = getStartDate.call(videoEl);
					const t = start && start instanceof Date ? start.getTime() : NaN;
					if (!Number.isNaN(t) && t > 0) {
						firstPDTMs = t;
						if (targetSeek > 0) {
							const offsetSec = targetSeek - t / 1000;
							if (offsetSec > 0) videoEl.currentTime = offsetSec;
						}
					}
				}
				tryAutoplay(videoEl);
			};
			const onNativeError = () => {
				if (!videoEl) return;
				const err = videoEl.error;
				const code = err?.code ?? 0;
				const codeName = ({
					1: 'MEDIA_ERR_ABORTED',
					2: 'MEDIA_ERR_NETWORK',
					3: 'MEDIA_ERR_DECODE',
					4: 'MEDIA_ERR_SRC_NOT_SUPPORTED',
				} as Record<number, string>)[code] ?? `code ${code}`;
				noFootage = true;
				noFootageDetail = `native · ${codeName}`;
				loading = false;
				onError?.(`type=nativeError code=${code} msg=${err?.message ?? ''}`);
			};
			videoEl.addEventListener('loadedmetadata', onMetadata);
			videoEl.addEventListener('error', onNativeError);
			return () => {
				videoEl?.removeEventListener('loadedmetadata', onMetadata);
				videoEl?.removeEventListener('error', onNativeError);
				if (hls) { hls.destroy(); hls = null; }
			};
		}

		return () => {
			if (hls) { hls.destroy(); hls = null; }
		};
	});

	// Loop logic — rAF for tight boundary clamping (timeupdate is too infrequent)
	$effect(() => {
		if (!videoEl || loopStart <= 0 || loopEnd <= 0) return;
		const mediaEl = videoEl;
		const ls = loopStart;
		const le = loopEnd;
		let raf: number;

		const tick = () => {
			if (firstPDTMs) {
				const epochNow = firstPDTMs / 1000 + mediaEl.currentTime;
				if (epochNow >= le || epochNow < ls) {
					const startOffset = ls - firstPDTMs / 1000;
					mediaEl.currentTime = Math.max(0, startOffset);
				}
			}
			raf = requestAnimationFrame(tick);
		};
		raf = requestAnimationFrame(tick);

		return () => cancelAnimationFrame(raf);
	});

	// Reset playback to clip start on handle release
	$effect(() => {
		const _rev = loopSeekRevision; // track changes
		if (!videoEl || loopStart <= 0 || !firstPDTMs || _rev === 0) return;
		const startOffset = loopStart - firstPDTMs / 1000;
		videoEl.currentTime = Math.max(0, startOffset);
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
			<div class="flex flex-col items-center gap-1.5 text-muted-foreground px-4 text-center">
				<VideoOff class="h-8 w-8 opacity-40" />
				<span class="text-xs">{noFootageHeadline}</span>
				{#if noFootageDetail}
					<span class="text-[10px] font-mono opacity-60 break-all">{noFootageDetail}</span>
				{/if}
			</div>
		</div>
	{:else if needsUserPlay}
		<button
			type="button"
			class="absolute inset-0 grid place-items-center bg-black/50 backdrop-blur-sm"
			onclick={(e) => { e.stopPropagation(); manualResume(); }}
			aria-label="Resume playback"
		>
			<span class="flex items-center justify-center h-14 w-14 rounded-full bg-white/90 text-black shadow-lg">
				<Play class="h-7 w-7 fill-current ml-0.5" />
			</span>
		</button>
	{:else if loading}
		<div class="absolute inset-0 grid place-items-center pointer-events-none">
			<div class="h-8 w-8 rounded-full border-2 border-white/30 border-t-white/90 animate-spin"></div>
		</div>
	{/if}
</div>
