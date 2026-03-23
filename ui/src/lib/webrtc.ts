import { watchCamera, unwatchCamera } from '$lib/signaling.js';
import type { TelemetryData } from '$lib/types.js';

/**
 * Remove all a=candidate lines from an SDP before sending to the server.
 * The server is ICE-lite and never initiates connectivity checks against the
 * browser's candidates — it only responds to STUN binding requests it
 * receives. Stripping candidates prevents failures when Firefox uses
 * mDNS-obfuscated addresses (e.g. "a1b2c3d4.local") that str0m can't parse.
 */
function stripCandidates(sdp: string): string {
	return sdp
		.split('\r\n')
		.filter((line) => !line.startsWith('a=candidate:'))
		.join('\r\n');
}

export interface CameraCallbacks {
	onVideoTrack: (stream: MediaStream) => void;
	onAudioTrack: (stream: MediaStream) => void;
	onTelemetry: (data: TelemetryData) => void;
	onDisconnect: () => void;
}

export type ClientMode = 'live' | 'playback' | 'map';

/**
 * Per-camera WebRTC connection. Each camera gets its own RTCPeerConnection.
 */
export class CameraConnection {
	readonly deviceId: string;
	readonly pc: RTCPeerConnection;
	sessionId: string | null = null;

	private callbacks: CameraCallbacks;
	private commandsChannel: RTCDataChannel | null = null;
	private disconnectFired = false;

	constructor(deviceId: string, callbacks: CameraCallbacks) {
		this.deviceId = deviceId;
		this.callbacks = callbacks;

		this.pc = new RTCPeerConnection({
			iceServers: [], // ICE-lite, no STUN/TURN needed
		});

		this.setupTrackHandler();
		this.setupConnectionStateHandler();
	}

	/** Send a client_mode command on the reliable commands data channel. */
	sendClientMode(mode: ClientMode) {
		if (this.commandsChannel?.readyState === 'open') {
			this.commandsChannel.send(JSON.stringify({ type: 'client_mode', mode }));
		}
	}

	private setupTrackHandler() {
		this.pc.ontrack = (event) => {
			const stream = event.streams[0] ?? new MediaStream([event.track]);

			if (event.track.kind === 'video') {
				this.callbacks.onVideoTrack(stream);
			} else if (event.track.kind === 'audio') {
				this.callbacks.onAudioTrack(stream);
			}
		};
	}

	private setupTelemetryChannel() {
		// Pre-negotiated data channel (negotiated: true, id: 1) — both browser and server
		// agree on stream ID 1 without DCEP. This avoids the DATA_CHANNEL_OPEN/ACK round
		// trip, so the channel is open as soon as SCTP connects.
		const ch = this.pc.createDataChannel('telemetry', { negotiated: true, id: 1 });
		ch.onmessage = (msg) => {
			try {
				const raw = JSON.parse(msg.data);
				const data: TelemetryData = {
					device_id: this.deviceId,
					cpu_percent: raw.cpu ?? undefined,
					temp_celsius: raw.temp ?? undefined,
					memory_mb: raw.mem ?? undefined,
					uptime_secs: raw.uptime ?? undefined,
					gps: raw.lat != null && raw.lon != null
						? { latitude: raw.lat, longitude: raw.lon, alt: raw.alt }
						: undefined,
				};
				this.callbacks.onTelemetry(data);
			} catch {
				// Ignore malformed telemetry
			}
		};
	}

	private setupConnectionStateHandler() {
		const notifyDisconnect = () => {
			if (this.disconnectFired) return;
			this.disconnectFired = true;
			this.callbacks.onDisconnect();
		};
		this.pc.onconnectionstatechange = () => {
			if (
				this.pc.connectionState === 'failed' ||
				this.pc.connectionState === 'closed'
			) {
				notifyDisconnect();
			}
		};
		// Firefox may report iceConnectionState='failed' without transitioning
		// connectionState to 'failed'. Watch both to be safe.
		this.pc.oniceconnectionstatechange = () => {
			if (this.pc.iceConnectionState === 'failed') {
				notifyDisconnect();
			}
		};
	}

	async connect(): Promise<void> {
		// Add transceivers for receiving
		this.pc.addTransceiver('video', { direction: 'recvonly' });
		this.pc.addTransceiver('audio', { direction: 'recvonly' });

		// 'commands': reliable ordered channel for client_mode messages.
		this.commandsChannel = this.pc.createDataChannel('commands', { ordered: true });
		this.commandsChannel.onopen = () => {
			this.sendClientMode('live');
		};

		// 'telemetry': pre-negotiated channel (stream ID 1 agreed with server out-of-band).
		this.setupTelemetryChannel();

		const offer = await this.pc.createOffer();
		await this.pc.setLocalDescription(offer);

		// Wait for ICE gathering
		await this.waitForIceGathering();

		const response = await watchCamera(this.deviceId, stripCandidates(this.pc.localDescription!.sdp));
		this.sessionId = response.session_id;

		const fixedSdp = this.rewriteLoopbackCandidates(response.sdp_answer);

		await this.pc.setRemoteDescription({
			type: 'answer',
			sdp: fixedSdp,
		});
	}

	async disconnect(): Promise<void> {
		if (this.sessionId) {
			await unwatchCamera(this.sessionId).catch(() => {});
			this.sessionId = null;
		}
		this.pc.close();
	}

	getStats(): Promise<RTCStatsReport> | null {
		return this.pc?.getStats() ?? null;
	}

	get connectionState(): RTCPeerConnectionState {
		return this.pc.connectionState;
	}

	private rewriteLoopbackCandidates(answerSdp: string): string {
		if (!answerSdp.includes('127.0.0.1')) return answerSdp;

		const localSdp = this.pc?.localDescription?.sdp ?? '';
		// Match standard IPv4 candidates
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

		// Firefox uses mDNS candidates (e.g. "a1b2c3.local") instead of real IPs.
		// Fall back to the page's hostname if it's a routable address (not localhost).
		if (!localIp && typeof window !== 'undefined') {
			const host = window.location.hostname;
			if (host && host !== 'localhost' && host !== '127.0.0.1' && host !== '::1') {
				localIp = host;
			}
		}

		if (!localIp) {
			// Firefox uses mDNS candidates; we can't extract the real LAN IP.
			// When GHOSTCAM_PUBLIC_IP is 127.0.0.1, STUN responses will come from
			// the server's LAN IP (not loopback), causing ICE check failures.
			// Fix: set GHOSTCAM_PUBLIC_IP to the server's LAN IP, or access the
			// UI via the LAN IP (e.g. http://10.0.0.x:5173) instead of localhost.
			console.warn(
				'[webrtc] Cannot rewrite loopback candidate for this browser. ' +
				'If using Firefox, set GHOSTCAM_PUBLIC_IP to your LAN IP.',
			);
			return answerSdp;
		}

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
}
