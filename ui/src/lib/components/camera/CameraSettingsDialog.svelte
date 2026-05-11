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
	import { AlertTriangle, HelpCircle, Check, X, Calendar, Battery } from 'lucide-svelte';
	import ScheduleEditorDialog from './ScheduleEditorDialog.svelte';
	import BatteryRulesEditorDialog from './BatteryRulesEditorDialog.svelte';

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
	let powerMode = $state('live');
	let uploadMode = $state('proactive');
	let saving = $state(false);
	let error = $state('');
	let success = $state(false);
	let confirmingDelete = $state(false);
	let deleting = $state(false);
	let confirmingPurge = $state(false);
	let purging = $state(false);
	let purgeProgress = $state<PurgeProgress | null>(null);
	let modeHelpOpen = $state(false);
	let powerHelpOpen = $state(false);
	let scheduleEditorOpen = $state(false);
	let batteryRulesEditorOpen = $state(false);

	function scheduleRuleCount(): number {
		if (!camera?.schedule) return 0;
		try {
			const parsed = JSON.parse(camera.schedule);
			return Array.isArray(parsed) ? parsed.length : 0;
		} catch {
			return 0;
		}
	}
	function batteryRuleCount(): number {
		if (!camera?.battery_rules) return 0;
		try {
			const parsed = JSON.parse(camera.battery_rules);
			return Array.isArray(parsed) ? parsed.length : 0;
		} catch {
			return 0;
		}
	}

	async function saveSchedule(json: string) {
		// Patch only the schedule field. Empty array clears it server-side.
		try {
			const parsed = JSON.parse(json);
			await updateCameraSettings(deviceId, { schedule: parsed });
			if (camera) camera.schedule = parsed.length === 0 ? undefined : json;
		} catch (e) {
			throw e instanceof Error ? e : new Error('save failed');
		}
	}
	async function saveBatteryRules(json: string) {
		try {
			const parsed = JSON.parse(json);
			await updateCameraSettings(deviceId, { battery_rules: parsed });
			if (camera) camera.battery_rules = parsed.length === 0 ? undefined : json;
		} catch (e) {
			throw e instanceof Error ? e : new Error('save failed');
		}
	}

	// Sync local state when dialog opens
	$effect(() => {
		if (open && camera) {
			displayName = camera.device_name || '';
			resolution = camera.resolution || '720p';
			recordingMode = camera.recording_mode || 'never';
			powerMode = camera.power_mode || 'live';
			uploadMode = camera.upload_mode || 'proactive';
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
				recordingMode !== camera.recording_mode ||
				powerMode !== camera.power_mode ||
				uploadMode !== camera.upload_mode)
	);

	// Show "currently effective" hint only when a schedule or battery rule
	// is overriding the manually-set mode (effective ≠ manual).
	let effectiveOverride = $derived.by(() => {
		if (!camera) return null;
		const eff = camera.effectivePowerMode;
		const upEff = camera.effectiveUploadMode;
		if (eff && eff !== camera.power_mode) return `power: ${eff}`;
		if (upEff && upEff !== camera.upload_mode) return `upload: ${upEff}`;
		return null;
	});

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
			if (powerMode !== camera.power_mode) update.power_mode = powerMode;
			if (uploadMode !== camera.upload_mode) update.upload_mode = uploadMode;
			await updateCameraSettings(deviceId, update);
			if (update.display_name) camera.device_name = displayName;
			camera.resolution = resolution;
			camera.recording_mode = recordingMode;
			camera.power_mode = powerMode;
			camera.upload_mode = uploadMode;
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

			<div class="border-t pt-4">
				<div class="flex items-center gap-1.5 mb-2">
					<span class="text-sm font-medium">Power &amp; data</span>
					<button
						type="button"
						class="text-muted-foreground hover:text-foreground"
						aria-label="Power-mode help"
						onclick={() => (powerHelpOpen = true)}
					>
						<HelpCircle class="h-3.5 w-3.5" />
					</button>
					{#if effectiveOverride}
						<span class="ml-auto text-xs text-amber-700 dark:text-amber-400">
							currently overridden — {effectiveOverride}
						</span>
					{/if}
				</div>

				<div class="grid grid-cols-2 gap-3">
					<div>
						<label for="power-mode" class="text-xs text-muted-foreground">Power mode</label>
						<select
							id="power-mode"
							bind:value={powerMode}
							class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
						>
							<option value="live" title="Always on. Live tile is instant, telemetry every 10s, every segment uploads ASAP. Highest battery cost.">Live</option>
							<option value="standby" title="Live WS opens on demand (~5–12s viewer wait). Telemetry every 10s, recording continues. Saves ~50% on cellular at idle.">Standby</option>
							<option value="sleep" title="Capture and GPS off. Telemetry every 5 min. Live and recording unavailable until mode changes.">Sleep</option>
						</select>
					</div>

					<div>
						<label for="upload-mode" class="text-xs text-muted-foreground">Upload</label>
						<select
							id="upload-mode"
							bind:value={uploadMode}
							class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
						>
							<option value="proactive" title="Every recorded segment uploads to S3 as soon as it's finished.">Proactive</option>
							<option value="lazy" title="Motion-flagged segments upload immediately. Non-motion stays on the camera until you scrub to that time on the timeline.">Lazy (motion-exempt)</option>
						</select>
					</div>
				</div>

				{#if powerMode === 'sleep'}
					<div class="mt-2 flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
						<AlertTriangle class="h-3.5 w-3.5 shrink-0 mt-0.5" />
						<span>
							Capture stops in sleep mode. Live viewing won't work until you
							switch back. Telemetry polls every 5 minutes so commands take
							up to that long to apply.
						</span>
					</div>
				{:else if powerMode === 'standby'}
					<p class="mt-2 text-xs text-muted-foreground">
						Live tile takes 5–12 s to start the first time after the camera
						has gone idle. After a viewer disconnects the live WS closes
						again ~30 s later.
					</p>
				{/if}

				{#if uploadMode === 'lazy' && recordingMode !== 'never'}
					<p class="mt-2 text-xs text-muted-foreground">
						Non-motion segments stay on the camera. Scrubbing to a time on the
						timeline triggers the camera to upload that range on the next
						telemetry cycle (≤ 10 s in live/standby, ≤ 5 min in sleep). Motion
						segments still upload immediately.
					</p>
				{/if}

				{#if camera?.battery_pct != null}
					<div class="mt-3 flex items-center gap-2 text-xs">
						<span class="text-muted-foreground">Battery</span>
						<div class="flex-1 h-1.5 rounded-full bg-muted overflow-hidden">
							<div
								class="h-full rounded-full transition-all duration-300"
								class:bg-destructive={camera.battery_pct < 20}
								class:bg-amber-500={camera.battery_pct >= 20 && camera.battery_pct < 50}
								class:bg-primary={camera.battery_pct >= 50}
								style="width: {camera.battery_pct}%"
							></div>
						</div>
						<span class="font-mono tabular-nums">{camera.battery_pct}%</span>
					</div>
				{/if}

				<div class="mt-3 grid grid-cols-2 gap-2">
					<Button
						variant="outline"
						class="justify-start"
						onclick={() => (scheduleEditorOpen = true)}
					>
						<Calendar class="h-3.5 w-3.5 mr-2" />
						<span class="text-xs">
							Schedule
							{#if scheduleRuleCount() > 0}
								<span class="text-muted-foreground">· {scheduleRuleCount()}</span>
							{/if}
						</span>
					</Button>
					<Button
						variant="outline"
						class="justify-start"
						onclick={() => (batteryRulesEditorOpen = true)}
					>
						<Battery class="h-3.5 w-3.5 mr-2" />
						<span class="text-xs">
							Battery rules
							{#if batteryRuleCount() > 0}
								<span class="text-muted-foreground">· {batteryRuleCount()}</span>
							{/if}
						</span>
					</Button>
				</div>
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

			{#if camera?.fw_version}
				<div class="flex items-baseline justify-between text-xs text-muted-foreground border-t pt-3">
					<span>Firmware</span>
					<span class="font-mono">{camera.fw_version}</span>
				</div>
			{/if}

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

<!-- Power-mode comparison -->
<Dialog bind:open={powerHelpOpen}>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Power &amp; data modes</DialogTitle>
			<DialogDescription>
				Trade battery and bandwidth for responsiveness. Two orthogonal
				knobs: when the camera maintains a live link, and how it pushes
				recorded footage.
			</DialogDescription>
		</DialogHeader>

		<div class="mt-4 space-y-4 text-xs">
			<div>
				<p class="font-medium mb-1">Power mode</p>
				<table class="w-full">
					<thead>
						<tr class="border-b text-left text-muted-foreground">
							<th class="py-2 pr-2 font-medium"></th>
							<th class="py-2 px-2 font-medium">Live</th>
							<th class="py-2 px-2 font-medium">Standby</th>
							<th class="py-2 pl-2 font-medium">Sleep</th>
						</tr>
					</thead>
					<tbody class="[&>tr]:border-b [&>tr:last-child]:border-0">
						<tr>
							<td class="py-2 pr-2 font-medium">Live tile</td>
							<td class="py-2 px-2">instant</td>
							<td class="py-2 px-2">5–12 s wake</td>
							<td class="py-2 pl-2">unavailable</td>
						</tr>
						<tr>
							<td class="py-2 pr-2 font-medium">Capture</td>
							<td class="py-2 px-2">always</td>
							<td class="py-2 px-2">always</td>
							<td class="py-2 pl-2">off</td>
						</tr>
						<tr>
							<td class="py-2 pr-2 font-medium">Telemetry</td>
							<td class="py-2 px-2">10 s</td>
							<td class="py-2 px-2">10 s</td>
							<td class="py-2 pl-2">5 min</td>
						</tr>
						<tr>
							<td class="py-2 pr-2 font-medium">Battery (est.)</td>
							<td class="py-2 px-2">baseline</td>
							<td class="py-2 px-2">~50% saved</td>
							<td class="py-2 pl-2">~80% saved</td>
						</tr>
					</tbody>
				</table>
			</div>

			<div>
				<p class="font-medium mb-1">Upload mode</p>
				<p class="mb-1">
					<span class="font-medium">Proactive</span> — every recorded
					segment uploads as soon as it's finished. Timeline scrubbing
					is instant.
				</p>
				<p>
					<span class="font-medium">Lazy</span> — motion-flagged
					segments always upload, so real-time motion alerts still
					work. Non-motion segments stay on the camera until you
					scrub to that time on the timeline, at which point the
					server pulls them on demand. Saves substantial upload
					bandwidth on cellular for "watched but rarely viewed"
					cameras.
				</p>
			</div>

			<p class="text-muted-foreground">
				A schedule (e.g. "standby + lazy between 22:00 and 06:00") and
				battery-driven rules ("force sleep when battery &lt; 20%") will
				eventually override the manual selection. Those editors are
				coming in a follow-up — until then, the manual choice above is
				the source of truth.
			</p>
		</div>

		<Button variant="outline" class="mt-4 w-full" onclick={() => (powerHelpOpen = false)}>
			Close
		</Button>
	</DialogContent>
</Dialog>

<ScheduleEditorDialog
	bind:open={scheduleEditorOpen}
	initial={camera?.schedule ?? ''}
	onsave={saveSchedule}
/>

<BatteryRulesEditorDialog
	bind:open={batteryRulesEditorOpen}
	initial={camera?.battery_rules ?? ''}
	batteryPctReporting={camera?.battery_pct != null}
	onsave={saveBatteryRules}
/>
