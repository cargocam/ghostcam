import { deleteFootage } from '$lib/signaling.js';

export interface PurgeProgress {
	deletedCount: number;
	bytesFreed: number;
	/** Total = deletedCount + remainingCount. 0 means unknown (indeterminate). */
	totalCount: number;
}

/**
 * Purge footage for a camera by calling {@link deleteFootage} in a loop
 * until the server reports no more work. The backend bounds each call
 * to a small batch; larger deletions therefore complete via many
 * round-trips, with `onProgress` invoked after each one so the UI can
 * render an indeterminate-ish progress indicator.
 *
 * Omitting `range` deletes every segment for the device. A partial
 * range passes epoch-millisecond bounds straight through to the
 * server.
 *
 * A hard safety cap prevents a buggy server that always returned
 * `has_more=true` from spinning the UI forever.
 */
export async function purgeFootage(
	deviceId: string,
	range: { fromMs?: number; toMs?: number } | undefined,
	onProgress?: (p: PurgeProgress) => void,
): Promise<PurgeProgress> {
	const MAX_ITERATIONS = 1000;
	let deletedCount = 0;
	let bytesFreed = 0;
	for (let i = 0; i < MAX_ITERATIONS; i++) {
		const res = await deleteFootage(deviceId, range);
		deletedCount += res.deleted_count;
		bytesFreed += res.bytes_freed;
		const totalCount = deletedCount + res.remaining_count;
		onProgress?.({ deletedCount, bytesFreed, totalCount });
		if (!res.has_more) return { deletedCount, bytesFreed, totalCount: deletedCount };
		// A well-behaved server returns deleted_count > 0 when has_more
		// is true. Defensive break: if it claims more remains but
		// nothing was deleted this batch, we'd loop forever.
		if (res.deleted_count === 0) return { deletedCount, bytesFreed, totalCount };
	}
	return { deletedCount, bytesFreed, totalCount: deletedCount };
}

/** Human-readable bytes label: "1.23 GB", "456 MB", "12 KB". */
export function formatBytes(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`;
	const kb = bytes / 1024;
	if (kb < 1024) return `${kb.toFixed(0)} KB`;
	const mb = kb / 1024;
	if (mb < 1024) return `${mb.toFixed(1)} MB`;
	const gb = mb / 1024;
	return `${gb.toFixed(2)} GB`;
}
