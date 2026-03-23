type ScrubberMode = 'live' | 'playback';
type ModeChangeCallback = (mode: ScrubberMode, playheadTime: number) => void;

class ScrubberStore {
	mode = $state<ScrubberMode>('live');
	playheadTime = $state<number>(Date.now() / 1000);
	playing = $state<boolean>(false);
	availableWindow = $state<{ start: number; end: number } | null>(null);
	cameraCoverage = $state<Map<string, { start: number; end: number }[]>>(new Map());

	private tickTimer: number | null = null;
	private lastFrameTime: number | null = null;
	private modeChangeCallbacks: ModeChangeCallback[] = [];

	/** Register a callback for mode changes (live↔playback). */
	onModeChange(cb: ModeChangeCallback) {
		this.modeChangeCallbacks.push(cb);
		return () => {
			this.modeChangeCallbacks = this.modeChangeCallbacks.filter((c) => c !== cb);
		};
	}

	private notifyModeChange() {
		for (const cb of this.modeChangeCallbacks) {
			cb(this.mode, this.playheadTime);
		}
	}

	initialize() {
		this.startLiveTick();
	}

	destroy() {
		this.stopTick();
	}

	scrubTo(time: number) {
		const wasLive = this.mode === 'live';
		this.mode = 'playback';
		this.playheadTime = time;
		this.playing = true;
		this.stopTick();
		this.startPlaybackTick();
		if (wasLive) {
			this.notifyModeChange();
		}
	}

	play() {
		if (this.mode !== 'playback') return;
		this.playing = true;
		this.startPlaybackTick();
	}

	pause() {
		this.playing = false;
		this.stopTick();
	}

	goLive() {
		this.mode = 'live';
		this.playing = false;
		this.playheadTime = Date.now() / 1000;
		this.stopTick();
		this.startLiveTick();
		this.notifyModeChange();
	}

	setAvailableWindow(window: { start: number; end: number }) {
		this.availableWindow = window;
		if (this.mode === 'playback') {
			if (this.playheadTime < window.start) {
				this.playheadTime = window.start;
			} else if (this.playheadTime > window.end) {
				this.playheadTime = window.end;
			}
		}
	}

	setCameraCoverage(deviceId: string, segments: { start: number; end: number }[]) {
		// Merge segments that are within 30 seconds of each other to avoid a
		// sea of tiny bars from back-to-back HLS segments.
		const GAP_THRESHOLD_MS = 30_000;
		const sorted = [...segments].sort((a, b) => a.start - b.start);
		const merged: { start: number; end: number }[] = [];
		for (const seg of sorted) {
			const last = merged[merged.length - 1];
			if (last && seg.start - last.end <= GAP_THRESHOLD_MS) {
				last.end = Math.max(last.end, seg.end);
			} else {
				merged.push({ ...seg });
			}
		}
		this.cameraCoverage = new Map(this.cameraCoverage).set(deviceId, merged);
	}

	private static readonly TICK_INTERVAL_MS = 100; // ~10fps — sufficient for timeline UI

	private startLiveTick() {
		this.stopTick();
		const tick = () => {
			if (this.mode === 'live') {
				this.playheadTime = Date.now() / 1000;
			}
		};
		tick();
		this.tickTimer = window.setInterval(tick, ScrubberStore.TICK_INTERVAL_MS);
	}

	private startPlaybackTick() {
		this.stopTick();
		this.lastFrameTime = performance.now();
		const tick = () => {
			if (this.mode !== 'playback' || !this.playing) return;
			const now = performance.now();
			const delta = (now - (this.lastFrameTime ?? now)) / 1000;
			this.lastFrameTime = now;
			this.playheadTime += delta;
			if (this.availableWindow && this.playheadTime > this.availableWindow.end) {
				this.playheadTime = this.availableWindow.end;
				this.playing = false;
				this.stopTick();
				return;
			}
		};
		this.tickTimer = window.setInterval(tick, ScrubberStore.TICK_INTERVAL_MS);
	}

	private stopTick() {
		if (this.tickTimer != null) {
			clearInterval(this.tickTimer);
			this.tickTimer = null;
		}
		this.lastFrameTime = null;
	}
}

export const scrubberStore = new ScrubberStore();
