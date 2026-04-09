import { describe, it, expect } from 'vitest';

function formatTimeAgo(epochMs: number): string {
	const sec = Math.floor((Date.now() - epochMs) / 1000);
	if (sec < 60) return `${sec}s ago`;
	const min = Math.floor(sec / 60);
	if (min < 60) return `${min}m ago`;
	const hr = Math.floor(min / 60);
	if (hr < 24) return `${hr}h ago`;
	return `${Math.floor(hr / 24)}d ago`;
}

describe('formatTimeAgo', () => {
	it('formats seconds', () => {
		expect(formatTimeAgo(Date.now() - 5000)).toBe('5s ago');
	});

	it('formats minutes', () => {
		expect(formatTimeAgo(Date.now() - 3 * 60 * 1000)).toBe('3m ago');
	});

	it('formats hours', () => {
		expect(formatTimeAgo(Date.now() - 2 * 60 * 60 * 1000)).toBe('2h ago');
	});

	it('formats days', () => {
		expect(formatTimeAgo(Date.now() - 3 * 24 * 60 * 60 * 1000)).toBe('3d ago');
	});

	it('handles 0 seconds ago', () => {
		expect(formatTimeAgo(Date.now())).toBe('0s ago');
	});

	it('handles boundary at 60s', () => {
		expect(formatTimeAgo(Date.now() - 59 * 1000)).toBe('59s ago');
		expect(formatTimeAgo(Date.now() - 60 * 1000)).toBe('1m ago');
	});
});
