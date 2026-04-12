<script lang="ts">
	import { cn } from '$lib/utils.js';

	type WebRtcState = 'connecting' | 'connected' | 'failed';

	let {
		deviceId,
		class: className = '',
		onStateChange = undefined,
	}: {
		deviceId: string;
		class?: string;
		onStateChange?: (state: WebRtcState) => void;
	} = $props();

	let videoEl = $state<HTMLVideoElement | undefined>(undefined);
	let peerConn = $state<RTCPeerConnection | null>(null);
	let whepSessionId = $state<string | null>(null);
	let retryCount = $state(0);
	let connState = $state<WebRtcState>('connecting');

	const MAX_RETRIES = 3;
	const RETRY_DELAY_MS = 5000;

	function updateState(newState: WebRtcState) {
		connState = newState;
		onStateChange?.(newState);
	}

	async function connect() {
		if (!deviceId) return;
		updateState('connecting');

		try {
			const pc = new RTCPeerConnection({
				// No ICE servers needed — server is ICE-lite with a public IP.
				iceServers: [],
			});
			peerConn = pc;

			// Receive video track from server.
			pc.addTransceiver('video', { direction: 'recvonly' });

			pc.ontrack = (event) => {
				if (videoEl && event.streams[0]) {
					videoEl.srcObject = event.streams[0];
					videoEl.play().catch(() => {});
				}
			};

			pc.onconnectionstatechange = () => {
				const s = pc.connectionState;
				if (s === 'connected') {
					updateState('connected');
					retryCount = 0;
				} else if (s === 'failed' || s === 'disconnected' || s === 'closed') {
					handleDisconnect();
				}
			};

			pc.oniceconnectionstatechange = () => {
				if (pc.iceConnectionState === 'failed') {
					handleDisconnect();
				}
			};

			// Create and send offer.
			const offer = await pc.createOffer();
			await pc.setLocalDescription(offer);

			const res = await fetch(`/api/v1/whep/${encodeURIComponent(deviceId)}`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/sdp' },
				body: offer.sdp,
				credentials: 'include',
			});

			if (!res.ok) {
				throw new Error(`WHEP offer failed: ${res.status}`);
			}

			// Extract session ID from Location header.
			const location = res.headers.get('Location');
			if (location) {
				const parts = location.split('/');
				whepSessionId = parts[parts.length - 1];
			}

			const answerSdp = await res.text();
			await pc.setRemoteDescription({
				type: 'answer',
				sdp: answerSdp,
			});
		} catch (err) {
			console.warn('WebRTC connect failed:', err);
			handleDisconnect();
		}
	}

	function handleDisconnect() {
		cleanup();
		retryCount++;
		if (retryCount >= MAX_RETRIES) {
			updateState('failed');
			return;
		}
		// Retry after delay.
		updateState('connecting');
		setTimeout(() => {
			if (connState === 'failed') return;
			connect();
		}, RETRY_DELAY_MS);
	}

	function cleanup() {
		if (peerConn) {
			peerConn.close();
			peerConn = null;
		}
		if (videoEl) {
			videoEl.srcObject = null;
		}
		// Tell server to clean up the session.
		if (whepSessionId && deviceId) {
			fetch(`/api/v1/whep/${encodeURIComponent(deviceId)}/${whepSessionId}`, {
				method: 'DELETE',
				credentials: 'include',
				keepalive: true,
			}).catch(() => {});
			whepSessionId = null;
		}
	}

	// Connect on mount, cleanup on destroy.
	$effect(() => {
		const id = deviceId; // track dependency
		if (!id) return;
		retryCount = 0;
		connect();
		return () => {
			cleanup();
		};
	});
</script>

<video
	bind:this={videoEl}
	autoplay
	playsinline
	muted
	class={cn('w-full h-full object-cover', className)}
></video>
