import { SignalingClient } from '$lib/signaling.js';
import type { DataChannelMessage } from '$lib/types.js';

export type OnTrackCallback = (deviceId: string, stream: MediaStream) => void;
export type OnAudioTrackCallback = (deviceId: string, stream: MediaStream) => void;
export type OnDataCallback = (msg: DataChannelMessage) => void;

export class WebRtcSession {
	private pc: RTCPeerConnection | null = null;
	private signaling: SignalingClient;
	private sessionId: string | null = null;
	private dataChannel: RTCDataChannel | null = null;
	/** Track mid → device_id mapping received from data channel */
	private trackDeviceMap = new Map<string, string>();
	/** Video streams received before track_map, keyed by mid */
	private pendingStreams = new Map<string, MediaStream>();
	/** Audio streams received before track_map, keyed by mid */
	private pendingAudioStreams = new Map<string, MediaStream>();

	onTrack: OnTrackCallback | null = null;
	onAudioTrack: OnAudioTrackCallback | null = null;
	onData: OnDataCallback | null = null;
	onConnectionStateChange: ((state: RTCPeerConnectionState) => void) | null = null;
	onTrackEnded: ((deviceId: string) => void) | null = null;

	constructor(signaling: SignalingClient) {
		this.signaling = signaling;
	}

	async connect(groupId: string): Promise<void> {
		// Get camera count so we can create the right number of transceivers
		const cameras = await this.signaling.listCamerasInGroup(groupId);
		const cameraCount = Math.max(cameras.length, 1);

		// No iceServers needed — bridge uses ICE-lite
		this.pc = new RTCPeerConnection({ iceServers: [] });

		this.pc.ontrack = (event) => {
			const transceiver = event.transceiver;
			const mid = transceiver.mid;
			const track = event.track;
			if (!mid || !track) return;

			const stream = new MediaStream([track]);
			const deviceId = this.trackDeviceMap.get(mid);

			// Handle track ended — resolve device from mid at event time
			track.onended = () => {
				const dev = this.trackDeviceMap.get(mid!);
				if (dev && track.kind === 'video' && this.onTrackEnded) {
					this.onTrackEnded(dev);
				}
			};
			track.onmute = () => {
				const dev = this.trackDeviceMap.get(mid!);
				if (dev && track.kind === 'video' && this.onTrackEnded) {
					this.onTrackEnded(dev);
				}
			};

			if (track.kind === 'video') {
				if (deviceId && this.onTrack) {
					this.onTrack(deviceId, stream);
				} else {
					this.pendingStreams.set(mid, stream);
				}
			} else if (track.kind === 'audio') {
				if (deviceId && this.onAudioTrack) {
					this.onAudioTrack(deviceId, stream);
				} else {
					this.pendingAudioStreams.set(mid, stream);
				}
			}
		};

		this.pc.ondatachannel = (event) => {
			this.setupDataChannel(event.channel);
		};

		this.pc.onconnectionstatechange = () => {
			if (this.pc && this.onConnectionStateChange) {
				this.onConnectionStateChange(this.pc.connectionState);
			}
		};

		// Create a data channel — must be in the offer for the bridge to respond
		const dc = this.pc.createDataChannel('telemetry');
		this.setupDataChannel(dc);

		// Add recv-only transceivers: one video + one audio per camera
		for (let i = 0; i < cameraCount; i++) {
			this.pc.addTransceiver('video', { direction: 'recvonly' });
			this.pc.addTransceiver('audio', { direction: 'recvonly' });
		}

		const offer = await this.pc.createOffer();
		await this.pc.setLocalDescription(offer);

		// Wait for ICE gathering to complete (or timeout)
		await this.waitForIceGathering();

		const { session_id, sdp_answer } = await this.signaling.watch(
			groupId,
			this.pc.localDescription!.sdp
		);
		this.sessionId = session_id;

		// If the bridge advertises 127.0.0.1 ICE candidates but the browser has
		// non-loopback candidates (e.g. 10.x.x.x), rewrite the answer to use the
		// browser's local IP. The bridge socket is on 0.0.0.0 so it accepts on any
		// interface — only the advertised address is wrong for cross-interface ICE.
		const fixedSdp = this.rewriteLoopbackCandidates(sdp_answer);

		await this.pc.setRemoteDescription({
			type: 'answer',
			sdp: fixedSdp,
		});
	}

	/** Called when track_map data channel message arrives */
	handleTrackMap(tracks: { mid: string; device_id: string; kind: string }[]) {
		for (const t of tracks) {
			this.trackDeviceMap.set(t.mid, t.device_id);
		}
		// Flush any pending video streams
		for (const [mid, stream] of this.pendingStreams) {
			const deviceId = this.trackDeviceMap.get(mid);
			if (deviceId && this.onTrack) {
				this.onTrack(deviceId, stream);
			}
		}
		this.pendingStreams.clear();
		// Flush any pending audio streams
		for (const [mid, stream] of this.pendingAudioStreams) {
			const deviceId = this.trackDeviceMap.get(mid);
			if (deviceId && this.onAudioTrack) {
				this.onAudioTrack(deviceId, stream);
			}
		}
		this.pendingAudioStreams.clear();
	}

	private setupDataChannel(channel: RTCDataChannel) {
		this.dataChannel = channel;
		channel.onmessage = (e) => {
			try {
				const msg: DataChannelMessage = JSON.parse(e.data);
				// Intercept track_map to update internal mapping
				if (msg.type === 'track_map') {
					this.handleTrackMap(msg.tracks);
				}
				// Intercept renegotiate — handle SDP offer/answer internally
				if (msg.type === 'renegotiate') {
					this.handleRenegotiate(msg.sdp_offer);
					return; // Don't propagate to data handler
				}
				if (this.onData) {
					this.onData(msg);
				}
			} catch {}
		};
	}

	/** Handle a server-initiated renegotiation offer (new/removed tracks). */
	private async handleRenegotiate(sdpOffer: string): Promise<void> {
		if (!this.pc || !this.dataChannel || this.dataChannel.readyState !== 'open') return;

		try {
			await this.pc.setRemoteDescription({ type: 'offer', sdp: sdpOffer });
			const answer = await this.pc.createAnswer();
			await this.pc.setLocalDescription(answer);

			const msg = JSON.stringify({
				type: 'sdp_answer',
				sdp_answer: this.pc.localDescription!.sdp,
			});
			this.dataChannel.send(msg);
		} catch (err) {
			console.error('Renegotiation failed:', err);
		}
	}

	/**
	 * Replace 127.0.0.1 in the answer's ICE candidates with the browser's
	 * local IP extracted from the offer. The bridge binds 0.0.0.0 so it
	 * accepts on any interface — only the advertised address is wrong.
	 */
	private rewriteLoopbackCandidates(answerSdp: string): string {
		if (!answerSdp.includes('127.0.0.1')) return answerSdp;

		// Extract a non-loopback IP from our local candidates
		const localSdp = this.pc?.localDescription?.sdp ?? '';
		const candidateRe = /a=candidate:\S+ \d+ udp \d+ ([\d.]+) /gi;
		let localIp: string | null = null;
		let match: RegExpExecArray | null;
		while ((match = candidateRe.exec(localSdp)) !== null) {
			const ip = match[1];
			if (ip && !ip.startsWith('127.') && !ip.startsWith('0.')) {
				localIp = ip;
				break;
			}
		}

		if (!localIp) return answerSdp;

		// Replace 127.0.0.1 only in a=candidate lines
		return answerSdp
			.split('\r\n')
			.map((line) => {
				if (line.startsWith('a=candidate:')) {
					return line.replace(/127\.0\.0\.1/g, localIp!);
				}
				return line;
			})
			.join('\r\n');
	}

	private waitForIceGathering(): Promise<void> {
		return new Promise((resolve) => {
			if (!this.pc) return resolve();
			if (this.pc.iceGatheringState === 'complete') return resolve();

			const timeout = setTimeout(resolve, 2000);
			this.pc.onicegatheringstatechange = () => {
				if (this.pc?.iceGatheringState === 'complete') {
					clearTimeout(timeout);
					resolve();
				}
			};
		});
	}

	async disconnect(): Promise<void> {
		if (this.sessionId) {
			try {
				await this.signaling.endSession(this.sessionId);
			} catch {}
			this.sessionId = null;
		}
		if (this.pc) {
			this.pc.close();
			this.pc = null;
		}
		this.dataChannel = null;
		this.trackDeviceMap.clear();
		this.pendingStreams.clear();
		this.pendingAudioStreams.clear();
	}

	getStats(): Promise<RTCStatsReport> | null {
		return this.pc?.getStats() ?? null;
	}

	/** Expose track mid → device_id map for stats correlation */
	getTrackDeviceMap(): Map<string, string> {
		return this.trackDeviceMap;
	}

	get connectionState(): RTCPeerConnectionState | null {
		return this.pc?.connectionState ?? null;
	}
}
