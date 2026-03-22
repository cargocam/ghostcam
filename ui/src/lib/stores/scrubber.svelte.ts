type ScrubberMode = 'live' | 'playback';
type ModeChangeCallback = (mode: ScrubberMode, playheadTime: number) => void;

class ScrubberStore {
	mode = $state<ScrubberMode>('live');
	playheadTime = $state<number>(Date.now() / 1000);
	playing = $state<boolean>(false);
	availableWindow = $state<{ start: number; end: number } | null>(null);
	cameraCoverage = $state<Map<string, { start: number; end: number }[]>>(new Map());

	private animationFrame: number | null = null;
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
		this.cameraCoverage = new Map(this.cameraCoverage).set(deviceId, segments);
	}

	private startLiveTick() {
		this.stopTick();
		const tick = () => {
			if (this.mode === 'live') {
				this.playheadTime = Date.now() / 1000;
				this.animationFrame = requestAnimationFrame(tick);
			}
		};
		this.animationFrame = requestAnimationFrame(tick);
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
			this.animationFrame = requestAnimationFrame(tick);
		};
		this.animationFrame = requestAnimationFrame(tick);
	}

	private stopTick() {
		if (this.animationFrame != null) {
			cancelAnimationFrame(this.animationFrame);
			this.animationFrame = null;
		}
		this.lastFrameTime = null;
	}
}

export const scrubberStore = new ScrubberStore();
