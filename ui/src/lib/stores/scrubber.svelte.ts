import { cameraStore } from '$lib/stores/cameras.svelte.js';

type ScrubberMode = 'live' | 'playback';
type ModeChangeCallback = (mode: ScrubberMode, playheadTime: number) => void;

export type SegmentCacheState = 'cached' | 'uploading' | 'available';

class ScrubberStore {
	mode = $state<ScrubberMode>('live');
	playheadTime = $state<number>(Date.now() / 1000);
	playing = $state<boolean>(false);
	availableWindow = $state<{ start: number; end: number } | null>(null);
	cameraCoverage = $state<Map<string, { start: number; end: number }[]>>(new Map());
	/** Per-camera segment cache states for timeline visualization. */
	cameraSegmentStates = $state<
		Map<string, { start: number; end: number; state: SegmentCacheState }[]>
	>(new Map());

	private animationFrame: number | null = null;
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
		// Don't start playback tick — HLS player drives the playhead via reportPlaybackTime()
		if (wasLive) {
			this.notifyModeChange();
		}
		this.debouncePrefetch(time);
	}

	/** No-op: segments are served directly from S3, no prefetch needed. */
	private debouncePrefetch(_timeSec: number) {}

	/** Called by the HLS player to report actual playback position.
	 *  This drives the timeline in playback mode instead of wall-clock time. */
	reportPlaybackTime(epochTime: number) {
		if (this.mode !== 'playback') return;
		this.playheadTime = epochTime;
	}

	play() {
		if (this.mode !== 'playback') return;
		this.playing = true;
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

	/** Update per-segment cache states. All S3 segments are "available". */
	setCameraSegmentStates(deviceId: string, segments: { start_ms: number; end_ms: number }[]) {
		const mapped = segments.map((s) => ({
			start: s.start_ms / 1000,
			end: s.end_ms / 1000,
			state: 'available' as SegmentCacheState,
		}));
		this.cameraSegmentStates = new Map(this.cameraSegmentStates).set(deviceId, mapped);
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

	private stopTick() {
		if (this.animationFrame != null) {
			cancelAnimationFrame(this.animationFrame);
			this.animationFrame = null;
		}
	}
}

export const scrubberStore = new ScrubberStore();
