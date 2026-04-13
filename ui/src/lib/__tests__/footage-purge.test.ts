import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { purgeFootage, formatBytes } from '../footage.js';

// Mock the signaling layer. We re-import deleteFootage here so the vi
// mock binding is in scope for the tests.
vi.mock('../signaling.js', () => ({
	deleteFootage: vi.fn(),
}));

import { deleteFootage } from '../signaling.js';

describe('purgeFootage', () => {
	beforeEach(() => {
		(deleteFootage as ReturnType<typeof vi.fn>).mockReset();
	});

	afterEach(() => {
		vi.restoreAllMocks();
	});

	it('returns totals from a single-batch response', async () => {
		(deleteFootage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
			deleted_count: 42,
			bytes_freed: 1024,
			has_more: false,
			remaining_count: 0,
		});
		const progress: number[] = [];
		const result = await purgeFootage('cam-1', undefined, (p) => progress.push(p.deletedCount));
		expect(result).toEqual({ deletedCount: 42, bytesFreed: 1024, totalCount: 42 });
		expect(deleteFootage).toHaveBeenCalledTimes(1);
		expect(progress).toEqual([42]);
	});

	it('loops until has_more is false, accumulating totals', async () => {
		(deleteFootage as ReturnType<typeof vi.fn>)
			.mockResolvedValueOnce({ deleted_count: 100, bytes_freed: 1000, has_more: true, remaining_count: 130 })
			.mockResolvedValueOnce({ deleted_count: 100, bytes_freed: 1500, has_more: true, remaining_count: 30 })
			.mockResolvedValueOnce({ deleted_count: 30, bytes_freed: 300, has_more: false, remaining_count: 0 });
		const result = await purgeFootage('cam-1', { fromMs: 1, toMs: 2 });
		expect(result).toEqual({ deletedCount: 230, bytesFreed: 2800, totalCount: 230 });
		expect(deleteFootage).toHaveBeenCalledTimes(3);
	});

	it('breaks defensively when server returns has_more=true with zero deletes', async () => {
		// A buggy server that always says has_more without making
		// progress would otherwise spin forever — the helper must bail.
		(deleteFootage as ReturnType<typeof vi.fn>).mockResolvedValue({
			deleted_count: 0,
			bytes_freed: 0,
			has_more: true,
			remaining_count: 5,
		});
		const result = await purgeFootage('cam-1', undefined);
		expect(result).toEqual({ deletedCount: 0, bytesFreed: 0, totalCount: 5 });
		expect(deleteFootage).toHaveBeenCalledTimes(1);
	});

	it('propagates the range to signaling', async () => {
		(deleteFootage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
			deleted_count: 0,
			bytes_freed: 0,
			has_more: false,
			remaining_count: 0,
		});
		await purgeFootage('cam-1', { fromMs: 1000, toMs: 2000 });
		expect(deleteFootage).toHaveBeenCalledWith('cam-1', { fromMs: 1000, toMs: 2000 });
	});
});

describe('formatBytes', () => {
	it('formats byte sizes with unit suffixes', () => {
		expect(formatBytes(0)).toBe('0 B');
		expect(formatBytes(500)).toBe('500 B');
		expect(formatBytes(2048)).toBe('2 KB');
		expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB');
		expect(formatBytes(2.5 * 1024 * 1024 * 1024)).toBe('2.50 GB');
	});
});
