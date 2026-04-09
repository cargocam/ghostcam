export type ClipPhase = 'idle' | 'downloading' | 'processing' | 'done' | 'error';

const MAX_CLIP_SECS = 5 * 60; // 5 minutes max
const MIN_CLIP_SECS = 10;

class ClipStore {
	enabled = $state(false);
	startTime = $state<number>(0); // epoch seconds
	endTime = $state<number>(0);   // epoch seconds
	phase = $state<ClipPhase>('idle');
	progress = $state(0); // 0-1
	error = $state<string | null>(null);

	get durationSecs() {
		return Math.max(0, this.endTime - this.startTime);
	}

	get durationLabel() {
		const s = this.durationSecs;
		if (s < 60) return `${Math.round(s)}s`;
		const m = Math.floor(s / 60);
		const sec = Math.round(s % 60);
		return sec > 0 ? `${m}m ${sec}s` : `${m}m`;
	}

	toggle(windowStart: number, windowEnd: number) {
		if (this.enabled) {
			this.cancel();
		} else {
			this.enabled = true;
			const clipDuration = Math.min(windowEnd - windowStart, MAX_CLIP_SECS);
			this.startTime = windowEnd - clipDuration;
			this.endTime = windowEnd;
			this.phase = 'idle';
			this.progress = 0;
			this.error = null;
		}
	}

	cancel() {
		this.enabled = false;
		this.phase = 'idle';
		this.progress = 0;
		this.error = null;
	}

	static get MAX_SECS() { return MAX_CLIP_SECS; }
	static get MIN_SECS() { return MIN_CLIP_SECS; }
}

export const clipStore = new ClipStore();
