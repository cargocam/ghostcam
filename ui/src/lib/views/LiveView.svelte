<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import CameraCard from '$lib/components/camera/CameraCard.svelte';
	import GetStartedCard from '$lib/components/camera/GetStartedCard.svelte';
	import { cn } from '$lib/utils.js';
	import { Camera } from 'lucide-svelte';

	let gridLayout = $derived(settingsStore.gridLayout);

	let gridClass = $derived.by(() => {
		switch (gridLayout) {
			case '1+5':
				return 'grid-cols-3';
			default:
				return 'grid-cols-[repeat(auto-fit,minmax(min(100%,28rem),1fr))]';
		}
	});

	let cameras = $derived(cameraStore.cameras);

	// The setup guide is shown by default when the user has no cameras.
	// Dismissal is sticky via localStorage so the "Click + to add" minimal
	// empty state is what returning users see after they've skipped it.
	let onboardingDismissed = $state(readDismissed());

	function readDismissed(): boolean {
		try {
			return localStorage.getItem('ghostcam-onboarding-dismissed') === '1';
		} catch {
			return false;
		}
	}

	let sortedCameras = $derived.by(() => {
		const cams = [...cameras].sort((a, b) => (b.online ? 1 : 0) - (a.online ? 1 : 0));
		if (gridLayout !== '1+5' || !cameraStore.selectedId) return cams;
		const selected = cams.find((c) => c.device_id === cameraStore.selectedId);
		if (!selected) return cams;
		return [selected, ...cams.filter((c) => c.device_id !== cameraStore.selectedId)];
	});
</script>

<div class="h-full overflow-y-auto p-2">
	{#if cameras.length === 0 && !onboardingDismissed}
		<GetStartedCard onDismiss={() => (onboardingDismissed = true)} />
	{:else if cameras.length === 0}
		<div class="flex flex-col items-center justify-center gap-4 text-muted-foreground py-32">
			<Camera class="h-12 w-12 opacity-40" />
			<div class="text-center space-y-1">
				<p class="text-lg font-semibold">No cameras yet</p>
				<p class="text-sm">Click <span class="font-bold">+</span> in the sidebar to add your first camera</p>
			</div>
		</div>
	{:else}
		<div class={cn("grid gap-3", gridClass)}>
			{#each sortedCameras as camera, i (camera.device_id)}
				<CameraCard
					deviceId={camera.device_id}
					name={cameraConfigStore.getDisplayName(camera.device_id, camera.device_name)}
					featured={gridLayout === '1+5' && i === 0}
				/>
			{/each}
		</div>
	{/if}
</div>
