<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { ScrollArea } from '$lib/components/ui/scroll-area/index.js';
	import CameraList from '$lib/components/camera/CameraList.svelte';
	import AddCameraDialog from '$lib/components/camera/AddCameraDialog.svelte';
	import TelemetryPanel from '$lib/components/telemetry/TelemetryPanel.svelte';
	import { Button } from '$lib/components/ui/button/index.js';
	import { Plus } from 'lucide-svelte';
	import { cn } from '$lib/utils.js';

	let {
		class: className,
	}: {
		class?: string;
	} = $props();

	let addCameraOpen = $state(false);
</script>

<aside
	class={cn(
		"hidden md:flex flex-col h-full bg-sidebar text-sidebar-foreground border-r border-sidebar-border",
		className
	)}
	style="width: var(--sidebar-width)"
>
	<!-- Camera list -->
	<div class="flex-1 min-h-0">
		<div class="px-4 py-3 flex items-center justify-between">
			<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
				Cameras
				<span class="ml-2 text-primary">{cameraStore.onlineCount}</span>
			</h2>
			<Button variant="ghost" size="icon" class="h-6 w-6" onclick={() => (addCameraOpen = true)}>
				<Plus class="h-3.5 w-3.5" />
			</Button>
		</div>
		<ScrollArea class="flex-1 px-2">
			<CameraList />
		</ScrollArea>
	</div>

	<AddCameraDialog bind:open={addCameraOpen} />

	<Separator />

	<!-- Telemetry for selected camera -->
	<div class="flex-shrink-0 overflow-y-auto max-h-[40%]">
		{#if cameraStore.selected}
			<div class="px-4 py-3">
				<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
					Telemetry
				</h2>
			</div>
			<TelemetryPanel sourceId={cameraStore.selected.device_id} />
		{:else}
			<div class="px-4 py-6 text-xs text-muted-foreground text-center">
				Select a camera to view telemetry
			</div>
		{/if}
	</div>
</aside>
