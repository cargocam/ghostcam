/** Per-source video statistics from WebRTC inbound-rtp reports */

export interface VideoStats {
	width: number;
	height: number;
	codec: string;
	framesDecoded: number;
	framesDropped: number;
	/** Estimated bitrate in kbps (based on bytesReceived delta) */
	bitrateKbps: number;
}

class VideoStatsStore {
	private stats = $state<Map<string, VideoStats>>(new Map());

	get(sourceId: string): VideoStats | undefined {
		return this.stats.get(sourceId);
	}

	getAll(): Map<string, VideoStats> {
		return this.stats;
	}

	update(sourceId: string, stats: VideoStats) {
		const next = new Map(this.stats);
		next.set(sourceId, stats);
		this.stats = next;
	}

	remove(sourceId: string) {
		const next = new Map(this.stats);
		next.delete(sourceId);
		this.stats = next;
	}

	get totalBitrateKbps(): number {
		let total = 0;
		for (const s of this.stats.values()) {
			total += s.bitrateKbps;
		}
		return total;
	}

	get totalFramesDecoded(): number {
		let total = 0;
		for (const s of this.stats.values()) {
			total += s.framesDecoded;
		}
		return total;
	}

	get totalFramesDropped(): number {
		let total = 0;
		for (const s of this.stats.values()) {
			total += s.framesDropped;
		}
		return total;
	}
}

export const videoStatsStore = new VideoStatsStore();
