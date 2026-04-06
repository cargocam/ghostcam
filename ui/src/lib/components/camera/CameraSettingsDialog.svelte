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
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';

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
	let recordingMode = $state('constant');
	let saving = $state(false);
	let error = $state('');
	let success = $state(false);
	let confirmingDelete = $state(false);
	let deleting = $state(false);

	// Sync local state when dialog opens
	$effect(() => {
		if (open && camera) {
			displayName = camera.device_name || '';
			resolution = camera.resolution || '720p';
			recordingMode = camera.recording_mode || 'constant';
			error = '';
			success = false;
			confirmingDelete = false;
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
			open = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to delete camera';
		} finally {
			deleting = false;
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
				<label for="recording-mode" class="text-sm font-medium">Recording Mode</label>
				<select
					id="recording-mode"
					bind:value={recordingMode}
					class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
				>
					<option value="constant">Continuous</option>
					<option value="motion">On Motion</option>
				</select>
				<p class="mt-1 text-xs text-muted-foreground">
					{recordingMode === 'motion'
						? 'Only uploads segments with detected motion. Saves storage.'
						: 'Records and uploads all footage continuously.'}
				</p>
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

			<div class="border-t pt-4">
				{#if confirmingDelete}
					<p class="text-sm text-destructive mb-2">
						Delete this camera? Recordings will remain in storage.
					</p>
					<div class="flex gap-2">
						<Button variant="outline" class="flex-1" onclick={() => { confirmingDelete = false; }}>
							Cancel
						</Button>
						<Button variant="destructive" class="flex-1" disabled={deleting} onclick={handleDelete}>
							{deleting ? 'Deleting...' : 'Confirm Delete'}
						</Button>
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
