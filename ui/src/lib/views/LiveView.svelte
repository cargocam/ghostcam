<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import CameraCard from '$lib/components/camera/CameraCard.svelte';
	import { cn } from '$lib/utils.js';

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

	let sortedCameras = $derived.by(() => {
		const cams = [...cameras].sort((a, b) => (b.online ? 1 : 0) - (a.online ? 1 : 0));
		if (gridLayout !== '1+5' || !cameraStore.selectedId) return cams;
		const selected = cams.find((c) => c.device_id === cameraStore.selectedId);
		if (!selected) return cams;
		return [selected, ...cams.filter((c) => c.device_id !== cameraStore.selectedId)];
	});
</script>

<div class="h-full overflow-y-auto p-2">
	<div class={cn("grid gap-3", gridClass)}>
		{#each sortedCameras as camera, i (camera.device_id)}
			<CameraCard
				deviceId={camera.device_id}
				name={cameraConfigStore.getDisplayName(camera.device_id, camera.device_name)}
				featured={gridLayout === '1+5' && i === 0}
			/>
		{/each}

		{#if cameras.length === 0}
			<div class="col-span-full flex items-center justify-center text-muted-foreground text-sm py-20">
				No cameras connected. Waiting for feeds...
			</div>
		{/if}
	</div>
</div>
