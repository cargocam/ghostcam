<script lang="ts">
	import { clipStore } from '$lib/stores/clip.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { prepareClip, exportTelemetry, fetchCoverage } from '$lib/signaling.js';
	import { purgeFootage, formatBytes } from '$lib/footage.js';
	import { concatSegments, type ConcatProgress } from '$lib/ffmpeg.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { billingStore } from '$lib/stores/billing.svelte.js';
	import { Download, X, FileText, Film, Trash2 } from 'lucide-svelte';

	function triggerDownload(blob: Blob, filename: string) {
		const url = URL.createObjectURL(blob);
		const a = document.createElement('a');
		a.href = url;
		a.download = filename;
		a.click();
		URL.revokeObjectURL(url);
	}

	/** Get cameras to export: selected camera, or all cameras if none selected. */
	function targetCameras(): { device_id: string; name: string }[] {
		if (cameraStore.selectedId) {
			const cam = cameraStore.getCamera(cameraStore.selectedId);
			return [{ device_id: cameraStore.selectedId, name: cam?.device_name ?? cameraStore.selectedId.slice(0, 8) }];
		}
		return cameraStore.cameras.map((c) => ({ device_id: c.device_id, name: c.device_name }));
	}

	let targetLabel = $derived(
		cameraStore.selectedId
			? cameraStore.getCamera(cameraStore.selectedId)?.device_name ?? 'Camera'
			: `All cameras (${cameraStore.cameras.length})`
	);

	async function downloadVideo() {
		const cameras = targetCameras();
		if (cameras.length === 0) return;

		try {
			clipStore.phase = 'downloading';
			clipStore.progress = 0;
			clipStore.error = null;

			const fromMs = Math.floor(clipStore.startTime * 1000);
			const toMs = Math.floor(clipStore.endTime * 1000);
			const ts = new Date(fromMs).toISOString().replace(/[:.]/g, '-').slice(0, 19);

			let totalDownloaded = 0;
			let totalToDownload = cameras.length; // rough estimate, refined after prepare

			for (let ci = 0; ci < cameras.length; ci++) {
				const cam = cameras[ci];
				const clip = await prepareClip(cam.device_id, fromMs, toMs);
				if (clip.segments.length === 0) continue;

				totalToDownload = cameras.length; // keep simple for now
				const urls = clip.segments.map((s) => s.url);

				const blob = await concatSegments(urls, (p: ConcatProgress) => {
					clipStore.phase = p.phase;
					// Scale progress across all cameras
					const camProgress = (ci + p.progress) / cameras.length;
					clipStore.progress = camProgress;
				});

				const suffix = cameras.length > 1 ? `-${cam.name || cam.device_id.slice(0, 8)}` : '';
				triggerDownload(blob, `clip${suffix}-${ts}.mp4`);
				totalDownloaded++;
			}

			if (totalDownloaded === 0) {
				clipStore.error = 'No segments in selected range';
				clipStore.phase = 'error';
			} else {
				clipStore.phase = 'idle';
			}
		} catch (e) {
			clipStore.error = e instanceof Error ? e.message : 'Download failed';
			clipStore.phase = 'error';
		}
	}

	async function downloadTelemetry(format: 'csv' | 'json') {
		const cameras = targetCameras();
		if (cameras.length === 0) return;

		try {
			const fromMs = Math.floor(clipStore.startTime * 1000);
			const toMs = Math.floor(clipStore.endTime * 1000);
			const ts = new Date(fromMs).toISOString().replace(/[:.]/g, '-').slice(0, 19);

			if (cameras.length === 1) {
				// Single camera: direct download
				const blob = await exportTelemetry(cameras[0].device_id, fromMs, toMs, format);
				triggerDownload(blob, `telemetry-${cameras[0].name || cameras[0].device_id.slice(0, 8)}-${ts}.${format}`);
			} else {
				// Multiple cameras: unified export
				if (format === 'json') {
					const allEntries: Record<string, any[]> = {};
					for (const cam of cameras) {
						const blob = await exportTelemetry(cam.device_id, fromMs, toMs, 'json');
						const data = JSON.parse(await blob.text());
						allEntries[cam.device_id] = data.entries ?? [];
					}
					const unified = new Blob([JSON.stringify({ cameras: allEntries }, null, 2)], { type: 'application/json' });
					triggerDownload(unified, `telemetry-all-${ts}.json`);
				} else {
					// CSV: merge all cameras with a device_id column
					let csv = 'device_id,ts,server_ts,cpu,mem,temp,uptime,sig,lat,lon,alt,gps_fix\n';
					for (const cam of cameras) {
						const blob = await exportTelemetry(cam.device_id, fromMs, toMs, 'csv');
						const text = await blob.text();
						const lines = text.split('\n').slice(1); // skip header
						for (const line of lines) {
							if (line.trim()) csv += `${cam.device_id},${line}\n`;
						}
					}
					const unified = new Blob([csv], { type: 'text/csv' });
					triggerDownload(unified, `telemetry-all-${ts}.csv`);
				}
			}
		} catch (e) {
			clipStore.error = e instanceof Error ? e.message : 'Export failed';
		}
	}

	let phaseLabel = $derived.by(() => {
		switch (clipStore.phase) {
			case 'downloading': return `Downloading... ${Math.round(clipStore.progress * 100)}%`;
			case 'processing': return 'Remuxing to MP4...';
			case 'done': return 'Done!';
			case 'error': return clipStore.error ?? 'Error';
			default: return '';
		}
	});

	let isWorking = $derived(clipStore.phase === 'downloading' || clipStore.phase === 'processing');

	// Delete-range state. We deliberately scope delete to a single
	// selected camera — fanning destructive deletes across "All cameras"
	// is too easy to do by accident.
	let confirmingDelete = $state(false);
	let deleting = $state(false);
	let deleteLabel = $state('');

	async function deleteRange() {
		if (!cameraStore.selectedId) return;
		const deviceId = cameraStore.selectedId;
		const fromMs = Math.floor(clipStore.startTime * 1000);
		const toMs = Math.floor(clipStore.endTime * 1000);
		if (toMs <= fromMs) return;
		deleting = true;
		deleteLabel = 'Deleting…';
		try {
			const result = await purgeFootage(deviceId, { fromMs, toMs }, (p) => {
				deleteLabel = `Deleting… ${p.deletedCount} · ${formatBytes(p.bytesFreed)}`;
			});
			deleteLabel = `Deleted ${result.deletedCount} segments · ${formatBytes(result.bytesFreed)}`;
			// Refresh usage + coverage so the storage bar and scrubber
			// reflect the purge without a full reload.
			billingStore.load().catch(() => {});
			fetchCoverage(deviceId).catch(() => {});
			confirmingDelete = false;
			clipStore.cancel();
		} catch (e) {
			clipStore.error = e instanceof Error ? e.message : 'Delete failed';
			clipStore.phase = 'error';
		} finally {
			deleting = false;
		}
	}
</script>

{#if clipStore.enabled}
	<div class="flex items-center gap-3 px-4 py-1.5 bg-background border-t border-border text-sm">
		<span class="text-muted-foreground font-mono text-xs" title="Max 5 minutes">
			{clipStore.durationLabel}
		</span>
		<span class="text-xs text-muted-foreground/70 truncate max-w-32">
			{targetLabel}
		</span>

		{#if isWorking}
			<div class="flex items-center gap-2 flex-1">
				<div class="h-1.5 flex-1 rounded-full bg-muted overflow-hidden">
					<div
						class="h-full rounded-full bg-primary transition-all duration-200"
						style="width: {clipStore.progress * 100}%"
					></div>
				</div>
				<span class="text-xs text-muted-foreground">{phaseLabel}</span>
			</div>
		{:else if clipStore.phase === 'error'}
			<span class="text-xs text-destructive flex-1">{clipStore.error}</span>
		{:else if deleting}
			<span class="text-xs text-destructive flex-1">{deleteLabel}</span>
		{:else if confirmingDelete}
			<div class="flex items-center gap-1.5 flex-1">
				<span class="text-xs text-destructive">Delete this {clipStore.durationLabel} range?</span>
				<Button variant="outline" size="sm" class="h-7 text-xs" onclick={() => { confirmingDelete = false; }}>
					Cancel
				</Button>
				<Button variant="destructive" size="sm" class="h-7 text-xs" onclick={deleteRange}>
					Confirm
				</Button>
			</div>
		{:else}
			<div class="flex items-center gap-1.5 flex-1">
				<Button variant="outline" size="sm" class="h-7 text-xs gap-1" onclick={downloadVideo} disabled={clipStore.durationSecs <= 0}>
					<Film class="h-3 w-3" />
					Video
				</Button>
				<Button variant="outline" size="sm" class="h-7 text-xs gap-1" onclick={() => downloadTelemetry('csv')} disabled={clipStore.durationSecs <= 0}>
					<FileText class="h-3 w-3" />
					CSV
				</Button>
				<Button variant="outline" size="sm" class="h-7 text-xs gap-1" onclick={() => downloadTelemetry('json')} disabled={clipStore.durationSecs <= 0}>
					<Download class="h-3 w-3" />
					JSON
				</Button>
				<Button
					variant="outline"
					size="sm"
					class="h-7 text-xs gap-1 text-destructive hover:text-destructive hover:bg-destructive/10"
					onclick={() => { confirmingDelete = true; }}
					disabled={clipStore.durationSecs <= 0 || !cameraStore.selectedId}
					title={cameraStore.selectedId ? 'Delete footage in this range' : 'Select a single camera to delete footage'}
				>
					<Trash2 class="h-3 w-3" />
					Delete
				</Button>
			</div>
		{/if}

		<button class="p-1 rounded hover:bg-accent" onclick={() => clipStore.cancel()} aria-label="Cancel clip">
			<X class="h-3.5 w-3.5 text-muted-foreground" />
		</button>
	</div>
{/if}
