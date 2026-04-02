class ScrubberStore {
	playheadTime = $state<number>(Date.now() / 1000);
	isLive = $state<boolean>(true);
	/** Committed seek time (epoch seconds). Null = live. */
	seekTarget = $state<number | null>(null);
	cameraCoverage = $state<Map<string, { start: number; end: number; hasMotion?: boolean }[]>>(new Map());
	availableWindow = $state<{ start: number; end: number } | null>(null);

	private animationFrame: number | null = null;

	initialize() { this.startLiveTick(); }
	destroy() { this.stopTick(); }

	/** Click on timeline — seek to that time. */
	seekTo(time: number) {
		this.isLive = false;
		this.seekTarget = time;
		this.playheadTime = time;
		this.stopTick();
	}

	goLive() {
		this.isLive = true;
		this.seekTarget = null;
		this.playheadTime = Date.now() / 1000;
		this.startLiveTick();
	}

	setAvailableWindow(window: { start: number; end: number }) {
		this.availableWindow = window;
	}

	setCameraCoverage(deviceId: string, segments: { start: number; end: number; hasMotion?: boolean }[]) {
		const GAP_THRESHOLD_SEC = 30;
		const sorted = [...segments].sort((a, b) => a.start - b.start);
		const merged: { start: number; end: number; hasMotion?: boolean }[] = [];
		for (const seg of sorted) {
			const last = merged[merged.length - 1];
			// Only merge if same motion state and within gap threshold
			if (last && seg.start - last.end <= GAP_THRESHOLD_SEC && (last.hasMotion ?? false) === (seg.hasMotion ?? false)) {
				last.end = Math.max(last.end, seg.end);
			} else {
				merged.push({ ...seg });
			}
		}
		this.cameraCoverage = new Map(this.cameraCoverage).set(deviceId, merged);
	}

	private startLiveTick() {
		this.stopTick();
		const tick = () => {
			if (this.isLive) {
				this.playheadTime = Date.now() / 1000;
				this.animationFrame = requestAnimationFrame(tick);
			}
		};
		this.animationFrame = requestAnimationFrame(tick);
	}

	private stopTick() {
		if (this.animationFrame != null) {
			cancelAnimationFrame(this.animationFrame);
			this.animationFrame = null;
		}
	}
}

export const scrubberStore = new ScrubberStore();
