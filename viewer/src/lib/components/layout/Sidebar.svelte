<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { ScrollArea } from '$lib/components/ui/scroll-area/index.js';
	import CameraList from '$lib/components/camera/CameraList.svelte';
	import TelemetryPanel from '$lib/components/telemetry/TelemetryPanel.svelte';
	import { cn } from '$lib/utils.js';

	let {
		class: className,
	}: {
		class?: string;
	} = $props();
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
		<div class="px-4 py-3">
			<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
				Cameras
				<span class="ml-2 text-primary">{cameraStore.onlineCount}</span>
			</h2>
		</div>
		<ScrollArea class="flex-1 px-2">
			<CameraList />
		</ScrollArea>
	</div>

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
