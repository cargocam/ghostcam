/**
 * Client-side MP4 assembly via ffmpeg.wasm 0.11.x.
 * Lazy-loads the wasm binary on first use.
 * Requires Cross-Origin-Isolation (COOP/COEP headers) for SharedArrayBuffer.
 */

import { createFFmpeg, fetchFile } from '@ffmpeg/ffmpeg';

let ffmpeg: ReturnType<typeof createFFmpeg> | null = null;
let loadPromise: Promise<void> | null = null;

async function ensureLoaded(): Promise<ReturnType<typeof createFFmpeg>> {
	if (ffmpeg?.isLoaded()) return ffmpeg;

	if (!loadPromise) {
		ffmpeg = createFFmpeg({ log: false });
		loadPromise = ffmpeg.load();
	}
	await loadPromise;
	return ffmpeg!;
}

export interface ConcatProgress {
	phase: 'downloading' | 'processing';
	progress: number; // 0-1
}

/**
 * Downloads MPEG-TS segments and remuxes them into a single MP4 via ffmpeg.wasm.
 * Uses -c copy (no re-encoding) — just container remuxing.
 */
export async function concatSegments(
	urls: string[],
	onProgress?: (p: ConcatProgress) => void,
): Promise<Blob> {
	if (urls.length === 0) throw new Error('No segments to concatenate');

	const ff = await ensureLoaded();

	// Download all segments and write to virtual FS
	const files: string[] = [];
	for (let i = 0; i < urls.length; i++) {
		onProgress?.({ phase: 'downloading', progress: i / urls.length });
		const name = `seg${i}.ts`;
		ff.FS('writeFile', name, await fetchFile(urls[i]));
		files.push(name);
	}
	onProgress?.({ phase: 'downloading', progress: 1 });

	// Build concat file
	const concatList = files.map((f) => `file '${f}'`).join('\n');
	ff.FS('writeFile', 'concat.txt', new TextEncoder().encode(concatList));

	// Remux to MP4 (copy, no re-encode)
	onProgress?.({ phase: 'processing', progress: 0 });
	await ff.run('-f', 'concat', '-safe', '0', '-i', 'concat.txt', '-c', 'copy', '-movflags', '+faststart', 'output.mp4');
	onProgress?.({ phase: 'processing', progress: 1 });

	// Read output
	const data = ff.FS('readFile', 'output.mp4');

	// Cleanup
	for (const f of files) ff.FS('unlink', f);
	ff.FS('unlink', 'concat.txt');
	ff.FS('unlink', 'output.mp4');

	// @ts-ignore — Uint8Array from FS is valid BlobPart
	return new Blob([data], { type: 'video/mp4' });
}
