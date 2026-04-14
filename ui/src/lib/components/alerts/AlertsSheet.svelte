<script lang="ts">
	import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from '$lib/components/ui/sheet/index.js';
	import AlertPanel from './AlertPanel.svelte';
	import { alertsStore } from '$lib/stores/alerts.svelte.js';

	let {
		open = $bindable(false),
		onOpenSettings,
	}: {
		open?: boolean;
		/** Called when an alert (e.g. storage_capped) requests the settings panel.
		 *  Parent should open settings; the sheet closes itself. */
		onOpenSettings?: () => void;
	} = $props();

	// Mark all as read when sheet opens
	$effect(() => {
		if (open && alertsStore.unreadCount > 0) {
			alertsStore.markAllRead();
		}
	});
</script>

<Sheet bind:open>
	<SheetContent side="right">
		<SheetHeader>
			<SheetTitle>Alerts</SheetTitle>
			<SheetDescription>Camera events and notifications</SheetDescription>
		</SheetHeader>

		<div class="mt-4 -mx-6 h-[calc(100vh-8rem)]">
			<AlertPanel
				onNavigate={() => (open = false)}
				onOpenSettings={() => { open = false; onOpenSettings?.(); }}
			/>
		</div>
	</SheetContent>
</Sheet>
