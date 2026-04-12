<script lang="ts">
	import HlsPlayer from '$lib/components/HlsPlayer.svelte';
	import WebRtcPlayer from '$lib/components/WebRtcPlayer.svelte';
	import { cn } from '$lib/utils.js';

	let {
		deviceId,
		src,
		muted = false,
		seekTo = -1,
		loopStart = -1,
		loopEnd = -1,
		loopSeekRevision = 0,
		mode = 'live' as 'live' | 'vod' | 'clip',
		class: className = '',
		onError = undefined,
		videoEl = $bindable<HTMLVideoElement | undefined>(undefined),
		/** Exposes whether WebRTC is active so parents can show latency indicators. */
		webrtcActive = $bindable<boolean>(false),
	}: {
		deviceId: string;
		src: string;
		muted?: boolean;
		seekTo?: number;
		loopStart?: number;
		loopEnd?: number;
		loopSeekRevision?: number;
		mode?: 'live' | 'vod' | 'clip';
		class?: string;
		onError?: (error: string) => void;
		videoEl?: HTMLVideoElement;
		webrtcActive?: boolean;
	} = $props();

	let webrtcState = $state<'connecting' | 'connected' | 'failed'>('connecting');
	let showWebRtc = $derived(mode === 'live' && webrtcState !== 'failed');

	// Expose whether WebRTC is providing the live video.
	$effect(() => {
		webrtcActive = webrtcState === 'connected' && mode === 'live';
	});
</script>

<div class={cn('relative w-full h-full', className)}>
	<!-- HLS always runs underneath — instant fallback if WebRTC fails. -->
	<div class={cn('absolute inset-0', showWebRtc && webrtcState === 'connected' ? 'invisible' : '')}>
		<HlsPlayer
			{src}
			{muted}
			{seekTo}
			{loopStart}
			{loopEnd}
			{loopSeekRevision}
			{mode}
			{onError}
			bind:videoEl
		/>
	</div>

	<!-- WebRTC overlay for live mode only. -->
	{#if mode === 'live' && webrtcState !== 'failed'}
		<div class={cn('absolute inset-0', webrtcState !== 'connected' ? 'invisible' : '')}>
			<WebRtcPlayer
				{deviceId}
				onStateChange={(s) => { webrtcState = s; }}
			/>
		</div>
	{/if}
</div>
