import { describe, it, expect } from 'vitest';

// Inline the merge logic from scrubber store for isolated testing
function mergeCoverage(
	segments: { start: number; end: number; hasMotion?: boolean }[],
	gapThreshold = 30
): { start: number; end: number; hasMotion: boolean }[] {
	const sorted = [...segments].sort((a, b) => a.start - b.start);
	const merged: { start: number; end: number; hasMotion: boolean }[] = [];
	for (const seg of sorted) {
		const last = merged[merged.length - 1];
		if (last && seg.start - last.end <= gapThreshold) {
			last.end = Math.max(last.end, seg.end);
			if (seg.hasMotion) last.hasMotion = true;
		} else {
			merged.push({ start: seg.start, end: seg.end, hasMotion: seg.hasMotion ?? false });
		}
	}
	return merged;
}

describe('coverage merge', () => {
	it('merges contiguous segments', () => {
		const result = mergeCoverage([
			{ start: 0, end: 6 },
			{ start: 6, end: 12 },
			{ start: 12, end: 18 },
		]);
		expect(result).toHaveLength(1);
		expect(result[0]).toEqual({ start: 0, end: 18, hasMotion: false });
	});

	it('merges segments within gap threshold', () => {
		const result = mergeCoverage([
			{ start: 0, end: 6 },
			{ start: 20, end: 26 }, // 14s gap < 30s threshold
		]);
		expect(result).toHaveLength(1);
		expect(result[0].end).toBe(26);
	});

	it('splits segments beyond gap threshold', () => {
		const result = mergeCoverage([
			{ start: 0, end: 6 },
			{ start: 40, end: 46 }, // 34s gap > 30s threshold
		]);
		expect(result).toHaveLength(2);
	});

	it('promotes motion state when any segment has motion', () => {
		const result = mergeCoverage([
			{ start: 0, end: 6, hasMotion: false },
			{ start: 6, end: 12, hasMotion: true },
			{ start: 12, end: 18, hasMotion: false },
		]);
		expect(result).toHaveLength(1);
		expect(result[0].hasMotion).toBe(true);
	});

	it('handles overlapping segments', () => {
		const result = mergeCoverage([
			{ start: 0, end: 10 },
			{ start: 5, end: 15 },
		]);
		expect(result).toHaveLength(1);
		expect(result[0]).toEqual({ start: 0, end: 15, hasMotion: false });
	});

	it('handles unsorted input', () => {
		const result = mergeCoverage([
			{ start: 12, end: 18 },
			{ start: 0, end: 6 },
			{ start: 6, end: 12 },
		]);
		expect(result).toHaveLength(1);
		expect(result[0]).toEqual({ start: 0, end: 18, hasMotion: false });
	});

	it('returns empty for empty input', () => {
		expect(mergeCoverage([])).toHaveLength(0);
	});
});
