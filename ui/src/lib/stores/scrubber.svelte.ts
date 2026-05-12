class ScrubberStore {
	playheadTime = $state<number>(Date.now() / 1000);
	isLive = $state<boolean>(true);
	/** Committed seek time (epoch seconds). Null = live. */
	seekTarget = $state<number | null>(null);
	cameraCoverage = $state<
		Map<
			string,
			{ start: number; end: number; hasMotion?: boolean; uploaded?: boolean; pending?: boolean }[]
		>
	>(new Map());
	/**
	 * Segments the camera pre-announced via the presign call's `pending`
	 * field. Server publishes a `segment_pending` SSE when the row is
	 * inserted; the UI renders them as a pulsing blue stripe in the
	 * timeline. Entries get evicted automatically when (a) the matching
	 * `coverage` event fires (transition to landed), or (b) ~60 s elapses
	 * with no transition (camera died mid-upload — the server-side
	 * sweeper would also drop the DB row at 5 min, but the UI fade is
	 * faster so the operator's view recovers quickly).
	 *
	 * Stored separately from `cameraCoverage` so we don't have to
	 * re-merge on every transition — the timeline renderer overlays
	 * pending on top of the existing coverage stripe.
	 */
	pendingSegments = $state<
		Map<string, { id: string; start: number; end: number; hasMotion: boolean; receivedAt: number }[]>
	>(new Map());
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

	setCameraCoverage(
		deviceId: string,
		segments: { start: number; end: number; hasMotion?: boolean; uploaded?: boolean }[],
	) {
		const GAP_THRESHOLD_SEC = 30;
		const sorted = [...segments].sort((a, b) => a.start - b.start);
		// We track lazy-mode runs separately from uploaded runs so the
		// timeline can render them with a different visual treatment.
		// A run that mixes uploaded + local-only is rendered as
		// uploaded=false so the user knows "scrub here will need a fetch
		// round-trip."
		const merged: {
			start: number;
			end: number;
			hasMotion: boolean;
			uploaded: boolean;
		}[] = [];
		for (const seg of sorted) {
			const last = merged[merged.length - 1];
			const segUploaded = seg.uploaded ?? true;
			if (
				last &&
				seg.start - last.end <= GAP_THRESHOLD_SEC &&
				last.uploaded === segUploaded
			) {
				last.end = Math.max(last.end, seg.end);
				if (seg.hasMotion) last.hasMotion = true;
			} else {
				merged.push({
					start: seg.start,
					end: seg.end,
					hasMotion: seg.hasMotion ?? false,
					uploaded: segUploaded,
				});
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

		// Any pending entries that just landed on `cameraCoverage` should
		// stop showing the blue indicator. Drop pending entries whose
		// time range overlaps a new uploaded segment. The matching is
		// loose — pending and uploaded ranges are nominally identical
		// because the server stores both with the same start_ts/end_ts.
		const pending = this.pendingSegments.get(deviceId);
		if (pending && pending.length > 0) {
			const stillPending = pending.filter((p) => {
				return !sorted.some(
					(c) => Math.abs(c.start - p.start) < 0.5 && Math.abs(c.end - p.end) < 0.5,
				);
			});
			if (stillPending.length !== pending.length) {
				this.pendingSegments = new Map(this.pendingSegments).set(
					deviceId, stillPending,
				);
			}
		}
	}

	/**
	 * Add segments the camera pre-announced via `segment_pending` SSE.
	 * Caller supplies wall-time (`now`) so the auto-fade is testable.
	 */
	addPendingSegments(
		deviceId: string,
		segments: { id: string; start: number; end: number; hasMotion: boolean }[],
		now: number = Date.now() / 1000,
	) {
		if (segments.length === 0) return;
		const existing = this.pendingSegments.get(deviceId) ?? [];
		const existingIds = new Set(existing.map((p) => p.id));
		const merged = [...existing];
		for (const s of segments) {
			if (!existingIds.has(s.id)) {
				merged.push({ ...s, receivedAt: now });
			}
		}
		this.pendingSegments = new Map(this.pendingSegments).set(deviceId, merged);
	}

	/**
	 * Drop pending entries older than `maxAgeSec` (default 60s). The
	 * timeline ticker calls this opportunistically — bounded by O(N)
	 * over a list that's normally <10 entries, so the cost is trivial.
	 */
	expirePendingOlderThan(maxAgeSec: number = 60, now: number = Date.now() / 1000) {
		let changed = false;
		const next = new Map(this.pendingSegments);
		for (const [deviceId, entries] of next) {
			const filtered = entries.filter((p) => now - p.receivedAt < maxAgeSec);
			if (filtered.length !== entries.length) {
				next.set(deviceId, filtered);
				changed = true;
			}
		}
		if (changed) this.pendingSegments = next;
	}

	private startTick() {
		this.stopTick();
		// Coalesce per-second housekeeping (pending-segment expiry)
		// so we don't run it 60×/s alongside playhead updates.
		let lastHousekeepingMs = 0;
		const tick = () => {
			if (!this.dragging) {
				if (this.isLive) {
					this.playheadTime = Date.now() / 1000;
				} else if (this.seekTarget !== null) {
					const elapsed = Date.now() / 1000 - this.playbackStartWall;
					this.playheadTime = this.playbackStartTime + elapsed;
				}
			}
			const nowMs = Date.now();
			if (nowMs - lastHousekeepingMs >= 1000) {
				this.expirePendingOlderThan();
				lastHousekeepingMs = nowMs;
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
