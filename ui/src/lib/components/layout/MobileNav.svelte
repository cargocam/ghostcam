<script lang="ts">
	import { Sheet, SheetContent, SheetHeader, SheetTitle } from '$lib/components/ui/sheet/index.js';
	import { ScrollArea } from '$lib/components/ui/scroll-area/index.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import CameraList from '$lib/components/camera/CameraList.svelte';
	import TelemetryPanel from '$lib/components/telemetry/TelemetryPanel.svelte';

	let {
		open = $bindable(false),
	}: {
		open?: boolean;
	} = $props();
</script>

<Sheet bind:open>
	<SheetContent side="left" class="w-80 p-0">
		<SheetHeader class="px-4 py-3 border-b">
			<SheetTitle>
				Cameras
				<span class="ml-2 text-primary text-sm">{cameraStore.onlineCount}</span>
			</SheetTitle>
		</SheetHeader>

		<ScrollArea class="flex-1 h-[50vh] px-2 py-2">
			<CameraList onSelect={() => (open = false)} />
		</ScrollArea>

		{#if cameraStore.selected}
			<Separator />
			<div class="px-4 py-3">
				<h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground mb-2">
					Telemetry
				</h3>
				<TelemetryPanel sourceId={cameraStore.selected.device_id} />
			</div>
		{/if}
	</SheetContent>
</Sheet>
