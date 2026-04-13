<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { updateCameraSettings, deleteCamera } from '$lib/signaling.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { purgeFootage, formatBytes, type PurgeProgress } from '$lib/footage.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { billingStore } from '$lib/stores/billing.svelte.js';
	import { AlertTriangle, HelpCircle, Check, X } from 'lucide-svelte';

	let {
		open = $bindable(false),
		deviceId,
		onclose,
	}: {
		open?: boolean;
		deviceId: string;
		onclose?: () => void;
	} = $props();

	let camera = $derived(cameraStore.getCamera(deviceId));
	let displayName = $state('');
	let resolution = $state('720p');
	let recordingMode = $state('never');
	let saving = $state(false);
	let error = $state('');
	let success = $state(false);
	let confirmingDelete = $state(false);
	let deleting = $state(false);
	let confirmingPurge = $state(false);
	let purging = $state(false);
	let purgeProgress = $state<PurgeProgress | null>(null);
	let modeHelpOpen = $state(false);

	// Sync local state when dialog opens
	$effect(() => {
		if (open && camera) {
			displayName = camera.device_name || '';
			resolution = camera.resolution || '720p';
			recordingMode = camera.recording_mode || 'never';
			error = '';
			success = false;
			confirmingDelete = false;
			confirmingPurge = false;
			purgeProgress = null;
		}
	});

	// Notify parent when dialog closes
	$effect(() => {
		if (!open) {
			onclose?.();
		}
	});

	let hasChanges = $derived(
		camera != null &&
			(displayName !== camera.device_name ||
				resolution !== camera.resolution ||
				recordingMode !== camera.recording_mode)
	);

	async function save() {
		if (!camera || !hasChanges) return;
		saving = true;
		error = '';
		success = false;
		try {
			const update: Record<string, string> = {};
			if (displayName !== camera.device_name) update.display_name = displayName;
			if (resolution !== camera.resolution) update.resolution = resolution;
			if (recordingMode !== camera.recording_mode) update.recording_mode = recordingMode;
			await updateCameraSettings(deviceId, update);
			if (update.display_name) camera.device_name = displayName;
			camera.resolution = resolution;
			camera.recording_mode = recordingMode;
			success = true;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to update settings';
		} finally {
			saving = false;
		}
	}

	async function handleDelete() {
		deleting = true;
		error = '';
		try {
			await deleteCamera(deviceId);
			cameraStore.removeCamera(deviceId);
			// Refresh storage usage so the billing bar reflects the reap
			// of this camera's segments.
			billingStore.load().catch(() => {});
			open = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to delete camera';
		} finally {
			deleting = false;
		}
	}

	async function handlePurge() {
		purging = true;
		error = '';
		purgeProgress = { deletedCount: 0, bytesFreed: 0, totalCount: 0 };
		try {
			const result = await purgeFootage(deviceId, undefined, (p) => {
				purgeProgress = p;
			});
			purgeProgress = result;
			// Refresh so the storage bar + timeline scrubber reflect the
			// purge without requiring a page reload.
			billingStore.load().catch(() => {});
			transportStore.refreshCoverage(deviceId).catch(() => {});
			confirmingPurge = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to delete footage';
		} finally {
			purging = false;
		}
	}
</script>

<Dialog bind:open>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Camera Settings</DialogTitle>
			<DialogDescription>
				Resolution and recording mode changes take effect after camera restarts.
			</DialogDescription>
		</DialogHeader>

		<div class="mt-4 space-y-4">
			<div>
				<label for="display-name" class="text-sm font-medium">Name</label>
				<input
					id="display-name"
					type="text"
					bind:value={displayName}
					placeholder="Camera name"
					class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
				/>
			</div>

			<div>
				<label for="resolution" class="text-sm font-medium">Resolution</label>
				<select
					id="resolution"
					bind:value={resolution}
					class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
				>
					<option value="480p">480p (854x480, 750 Kbps)</option>
					<option value="720p">720p (1280x720, 2 Mbps)</option>
					<option value="1080p">1080p (1920x1080, 4 Mbps)</option>
				</select>
			</div>

			<div>
				<div class="flex items-center gap-1.5">
					<label for="recording-mode" class="text-sm font-medium">Recording Mode</label>
					<button
						type="button"
						class="text-muted-foreground hover:text-foreground"
						aria-label="Compare recording modes"
						onclick={() => (modeHelpOpen = true)}
					>
						<HelpCircle class="h-3.5 w-3.5" />
					</button>
				</div>
				<select
					id="recording-mode"
					bind:value={recordingMode}
					class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
				>
					<option value="never" title="Live viewing only — no footage is saved. Timeline and clips are disabled for this camera.">
						Streaming Only (no recording)
					</option>
					<option value="motion" title="Uploads only segments where motion is detected. Heuristic — quiet scenes may miss subtle movement.">
						On Motion
					</option>
					<option value="constant" title="Records and uploads every segment continuously. Highest storage and bandwidth cost.">
						Continuous
					</option>
				</select>
				{#if recordingMode === 'never'}
					<div class="mt-2 flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
						<AlertTriangle class="h-3.5 w-3.5 shrink-0 mt-0.5" />
						<span>
							Live viewing only. No footage is recorded, so the timeline scrubber
							and clip export will be empty for this camera.
						</span>
					</div>
				{:else if recordingMode === 'motion'}
					<p class="mt-2 text-xs text-muted-foreground">
						Uploads only segments where motion is detected. Motion detection is
						heuristic — quiet scenes may miss subtle movement.
					</p>
				{:else if recordingMode === 'constant'}
					<div class="mt-2 flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
						<AlertTriangle class="h-3.5 w-3.5 shrink-0 mt-0.5" />
						<span>
							Records and uploads every segment continuously. Highest storage
							and bandwidth cost — roughly 2–4 GB per camera per day at 720p.
						</span>
					</div>
				{/if}
			</div>

			<div class="flex items-center gap-2">
				<input
					id="motion-alerts"
					type="checkbox"
					checked={!settingsStore.isMotionAlertsMuted(deviceId)}
					onchange={(e) => settingsStore.setMotionAlertsMuted(deviceId, !e.currentTarget.checked)}
					class="h-4 w-4 rounded border-input"
				/>
				<label for="motion-alerts" class="text-sm font-medium">Motion alerts</label>
			</div>

			{#if error}
				<p class="text-sm text-destructive">{error}</p>
			{/if}

			{#if success}
				<p class="text-sm text-primary">Settings saved. Camera will restart shortly.</p>
			{/if}

			<Button onclick={save} disabled={saving || !hasChanges} class="w-full">
				{saving ? 'Saving...' : 'Save Settings'}
			</Button>

			<div class="border-t pt-4 space-y-3">
				{#if confirmingPurge}
					<div>
						<p class="text-sm text-destructive mb-2">
							Permanently delete every recording for this camera? This cannot be undone.
						</p>
						{#if purging || (purgeProgress && purgeProgress.deletedCount > 0)}
							{@const total = purgeProgress?.totalCount ?? 0}
							{@const deleted = purgeProgress?.deletedCount ?? 0}
							{@const pct = total > 0 ? deleted / total : 0}
							{#if purging && total > 0}
								<div class="mb-2">
									<div class="flex justify-between text-xs text-muted-foreground mb-1">
										<span>Deleting… {deleted.toLocaleString()} / {total.toLocaleString()} segments</span>
										<span>{Math.round(pct * 100)}%</span>
									</div>
									<div class="h-1.5 w-full rounded-full bg-muted overflow-hidden">
										<div
											class="h-full rounded-full bg-destructive transition-all duration-300"
											style="width: {pct * 100}%"
										></div>
									</div>
									<p class="text-xs text-muted-foreground mt-1">
										{formatBytes(purgeProgress?.bytesFreed ?? 0)} freed
									</p>
								</div>
							{:else}
								<p class="text-xs text-muted-foreground mb-2">
									{purging ? 'Deleting…' : 'Done.'}
									{deleted.toLocaleString()} segments ·
									{formatBytes(purgeProgress?.bytesFreed ?? 0)} freed
								</p>
							{/if}
						{/if}
						<div class="flex gap-2">
							<Button
								variant="outline"
								class="flex-1"
								disabled={purging}
								onclick={() => { confirmingPurge = false; purgeProgress = null; }}
							>
								Cancel
							</Button>
							<Button
								variant="destructive"
								class="flex-1"
								disabled={purging}
								onclick={handlePurge}
							>
								{purging ? 'Deleting…' : 'Delete Footage'}
							</Button>
						</div>
					</div>
				{:else}
					<Button
						variant="ghost"
						class="w-full text-destructive hover:text-destructive hover:bg-destructive/10"
						onclick={() => { confirmingPurge = true; purgeProgress = null; }}
					>
						Delete All Footage
					</Button>
				{/if}

				{#if confirmingDelete}
					<div>
						<p class="text-sm text-destructive mb-2">
							Delete this camera and all its recordings? Large archives may take a moment.
						</p>
						<div class="flex gap-2">
							<Button variant="outline" class="flex-1" disabled={deleting} onclick={() => { confirmingDelete = false; }}>
								Cancel
							</Button>
							<Button variant="destructive" class="flex-1" disabled={deleting} onclick={handleDelete}>
								{deleting ? 'Deleting…' : 'Confirm Delete'}
							</Button>
						</div>
					</div>
				{:else}
					<Button variant="ghost" class="w-full text-destructive hover:text-destructive hover:bg-destructive/10" onclick={() => { confirmingDelete = true; }}>
						Delete Camera
					</Button>
				{/if}
			</div>
		</div>
	</DialogContent>
</Dialog>

<!-- Recording mode comparison -->
<Dialog bind:open={modeHelpOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Recording modes</DialogTitle>
			<DialogDescription>
				How each mode handles live viewing, recording, and storage.
			</DialogDescription>
		</DialogHeader>

		<div class="mt-4 overflow-x-auto">
			<table class="w-full text-xs">
				<thead>
					<tr class="border-b text-left text-muted-foreground">
						<th class="py-2 pr-2 font-medium"></th>
						<th class="py-2 px-2 font-medium">Streaming Only</th>
						<th class="py-2 px-2 font-medium">On Motion</th>
						<th class="py-2 pl-2 font-medium">Continuous</th>
					</tr>
				</thead>
				<tbody class="[&>tr]:border-b [&>tr:last-child]:border-0">
					<tr>
						<td class="py-2 pr-2 font-medium">Live viewing</td>
						<td class="py-2 px-2"><Check class="h-3.5 w-3.5 text-primary" /></td>
						<td class="py-2 px-2"><Check class="h-3.5 w-3.5 text-primary" /></td>
						<td class="py-2 pl-2"><Check class="h-3.5 w-3.5 text-primary" /></td>
					</tr>
					<tr>
						<td class="py-2 pr-2 font-medium">Timeline playback</td>
						<td class="py-2 px-2"><X class="h-3.5 w-3.5 text-muted-foreground" /></td>
						<td class="py-2 px-2">motion only</td>
						<td class="py-2 pl-2"><Check class="h-3.5 w-3.5 text-primary" /></td>
					</tr>
					<tr>
						<td class="py-2 pr-2 font-medium">Clip export</td>
						<td class="py-2 px-2"><X class="h-3.5 w-3.5 text-muted-foreground" /></td>
						<td class="py-2 px-2">motion only</td>
						<td class="py-2 pl-2"><Check class="h-3.5 w-3.5 text-primary" /></td>
					</tr>
					<tr>
						<td class="py-2 pr-2 font-medium">Storage usage</td>
						<td class="py-2 px-2">none</td>
						<td class="py-2 px-2">low</td>
						<td class="py-2 pl-2">high</td>
					</tr>
					<tr>
						<td class="py-2 pr-2 font-medium">Upload bandwidth</td>
						<td class="py-2 px-2">viewing only</td>
						<td class="py-2 px-2">bursty</td>
						<td class="py-2 pl-2">sustained</td>
					</tr>
				</tbody>
			</table>
		</div>

		<p class="mt-4 text-xs text-muted-foreground">
			New cameras default to <span class="font-medium">Streaming Only</span>
			so you pay no storage before opting in. Switch to
			<span class="font-medium">On Motion</span> or
			<span class="font-medium">Continuous</span> to enable recording,
			timeline playback, and clip export.
		</p>

		<Button variant="outline" class="mt-4 w-full" onclick={() => (modeHelpOpen = false)}>
			Close
		</Button>
	</DialogContent>
</Dialog>
