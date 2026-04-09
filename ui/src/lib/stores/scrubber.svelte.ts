class ScrubberStore {
	playheadTime = $state<number>(Date.now() / 1000);
	isLive = $state<boolean>(true);
	/** Committed seek time (epoch seconds). Null = live. */
	seekTarget = $state<number | null>(null);
	cameraCoverage = $state<Map<string, { start: number; end: number; hasMotion?: boolean }[]>>(new Map());
	/** Timestamps (epoch seconds) of motion events per camera, for timeline dots. */
	motionTimestamps = $state<Map<string, number[]>>(new Map());
	availableWindow = $state<{ start: number; end: number } | null>(null);

	private animationFrame: number | null = null;
	/** Wall-clock time when playback started (for VOD tick offset). */
	private playbackStartWall: number = 0;
	private playbackStartTime: number = 0;
	/** When true, the tick doesn't override playheadTime (user is dragging). */
	dragging = $state(false);

	initialize() { this.startTick(); }
	destroy() { this.stopTick(); }

	/** Click on timeline — seek to that time. */
	seekTo(time: number) {
		this.isLive = false;
		this.seekTarget = time;
		this.playheadTime = time;
		// Start advancing the playhead in real time from the seek point
		this.playbackStartWall = Date.now() / 1000;
		this.playbackStartTime = time;
		this.startTick();
	}

	goLive() {
		this.isLive = true;
		this.seekTarget = null;
		this.playheadTime = Date.now() / 1000;
		this.startTick();
	}

	setAvailableWindow(window: { start: number; end: number }) {
		this.availableWindow = window;
	}

	setCameraCoverage(deviceId: string, segments: { start: number; end: number; hasMotion?: boolean }[]) {
		const GAP_THRESHOLD_SEC = 30;
		const sorted = [...segments].sort((a, b) => a.start - b.start);
		const merged: { start: number; end: number; hasMotion: boolean }[] = [];
		for (const seg of sorted) {
			const last = merged[merged.length - 1];
			if (last && seg.start - last.end <= GAP_THRESHOLD_SEC) {
				last.end = Math.max(last.end, seg.end);
				if (seg.hasMotion) last.hasMotion = true;
			} else {
				merged.push({ start: seg.start, end: seg.end, hasMotion: seg.hasMotion ?? false });
			}
		}
		this.cameraCoverage = new Map(this.cameraCoverage).set(deviceId, merged);

		// Collect motion segment midpoints for timeline dots
		const motionTs = sorted
			.filter((s) => s.hasMotion)
			.map((s) => (s.start + s.end) / 2);
		const existing = this.motionTimestamps.get(deviceId) ?? [];
		// Dedupe by rounding to nearest second
		const seen = new Set(existing.map((t) => Math.round(t)));
		for (const t of motionTs) {
			if (!seen.has(Math.round(t))) {
				existing.push(t);
				seen.add(Math.round(t));
			}
		}
		this.motionTimestamps = new Map(this.motionTimestamps).set(deviceId, existing);
	}

	private startTick() {
		this.stopTick();
		const tick = () => {
			if (!this.dragging) {
				if (this.isLive) {
					this.playheadTime = Date.now() / 1000;
				} else if (this.seekTarget !== null) {
					const elapsed = Date.now() / 1000 - this.playbackStartWall;
					this.playheadTime = this.playbackStartTime + elapsed;
				}
			}
			this.animationFrame = requestAnimationFrame(tick);
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
