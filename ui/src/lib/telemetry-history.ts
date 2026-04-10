import { fetchTelemetryRange } from '$lib/signaling.js';
import type { TelemetryEntry } from '$lib/api-types';

type CacheEntry = {
	expiresAt: number;
	entries: TelemetryEntry[];
};

const CACHE_TTL_MS = 3000;
const BUCKET_MS = 15000;
const cache = new Map<string, CacheEntry>();

function bucket(tsMs: number): number {
	return Math.floor(tsMs / BUCKET_MS) * BUCKET_MS;
}

function cacheKey(deviceId: string, fromMs: number, toMs: number, limit: number): string {
	return `${deviceId}:${bucket(fromMs)}:${bucket(toMs)}:${limit}`;
}

export async function fetchTelemetryRangeCached(
	deviceId: string,
	fromMs: number,
	toMs: number,
	limit: number,
): Promise<TelemetryEntry[]> {
	const key = cacheKey(deviceId, fromMs, toMs, limit);
	const now = Date.now();
	const cached = cache.get(key);
	if (cached && cached.expiresAt > now) {
		return cached.entries;
	}

	const page = await fetchTelemetryRange(deviceId, fromMs, toMs, limit);
	cache.set(key, {
		expiresAt: now + CACHE_TTL_MS,
		entries: page.entries,
	});
	return page.entries;
}

export function nearestTelemetryEntry(
	entries: TelemetryEntry[],
	targetMs: number,
): TelemetryEntry | null {
	if (entries.length === 0) return null;
	let nearest = entries[0];
	let minDelta = Math.abs(entries[0].ts - targetMs);
	for (const entry of entries) {
		const delta = Math.abs(entry.ts - targetMs);
		// Deterministic tie-breaker: prefer earlier sample.
		if (delta < minDelta || (delta === minDelta && entry.ts <= nearest.ts)) {
			minDelta = delta;
			nearest = entry;
		}
	}
	return nearest;
}

export function nearestTelemetryEntryWithin(
	entries: TelemetryEntry[],
	targetMs: number,
	maxDeltaMs: number,
): TelemetryEntry | null {
	const nearest = nearestTelemetryEntry(entries, targetMs);
	if (!nearest) return null;
	if (Math.abs(nearest.ts - targetMs) > maxDeltaMs) return null;
	return nearest;
}
