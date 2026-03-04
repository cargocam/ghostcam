<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cn } from '$lib/utils.js';
	import { Pencil, Check, X } from 'lucide-svelte';

	let {
		onSelect,
	}: {
		onSelect?: () => void;
	} = $props();

	let editingId = $state<string | null>(null);
	let editingName = $state('');

	let groupedCameras = $derived(() => {
		const sorted = [...cameraStore.cameras].sort((a, b) => {
			if (a.connected !== b.connected) return a.connected ? -1 : 1;
			const nameA = cameraConfigStore.getDisplayName(a.device_id);
			const nameB = cameraConfigStore.getDisplayName(b.device_id);
			return nameA.localeCompare(nameB);
		});

		const groups = new Map<string, typeof sorted>();
		for (const cam of sorted) {
			const gid = cam.group_id || 'ungrouped';
			if (!groups.has(gid)) groups.set(gid, []);
			groups.get(gid)!.push(cam);
		}
		return [...groups.entries()].sort((a, b) => a[0].localeCompare(b[0]));
	});

	function selectCamera(id: string) {
		cameraStore.select(id);
		onSelect?.();
	}

	function startEdit(id: string, currentName: string) {
		editingId = id;
		editingName = currentName;
	}

	function confirmEdit() {
		if (editingId) {
			cameraConfigStore.rename(editingId, editingName);
			editingId = null;
		}
	}

	function cancelEdit() {
		editingId = null;
	}

	function handleEditKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter') confirmEdit();
		else if (e.key === 'Escape') cancelEdit();
	}
</script>

<div class="py-1">
	{#each groupedCameras() as [groupId, cameras] (groupId)}
		{#if groupedCameras().length > 1}
			<div class="px-3 pt-3 pb-1 first:pt-1">
				<span class="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/70">{groupId}</span>
			</div>
		{/if}

		<div class="space-y-0.5">
			{#each cameras as camera (camera.device_id)}
				{@const displayName = cameraConfigStore.getDisplayName(camera.device_id)}
				<div class="group/item relative">
					{#if editingId === camera.device_id}
						<div class="px-3 py-2 space-y-1.5">
							<div class="flex items-center gap-1">
								<span
									class={cn(
										"h-2 w-2 rounded-full flex-shrink-0",
										camera.connected ? "bg-primary" : "bg-destructive"
									)}
								></span>
								<input
									type="text"
									bind:value={editingName}
									onkeydown={handleEditKeydown}
									class="flex-1 min-w-0 text-sm bg-transparent border-b border-primary outline-none px-1"
									autofocus
								/>
								<button class="p-0.5 hover:text-primary" onclick={confirmEdit} aria-label="Confirm">
									<Check class="h-3.5 w-3.5" />
								</button>
								<button class="p-0.5 hover:text-destructive" onclick={cancelEdit} aria-label="Cancel">
									<X class="h-3.5 w-3.5" />
								</button>
							</div>
						</div>
					{:else}
						<button
							class={cn(
								"w-full flex items-center gap-3 px-3 py-2 rounded-md text-sm transition-colors text-left",
								"hover:bg-accent/50",
								cameraStore.selectedId === camera.device_id
									? "bg-accent border-l-2 border-primary"
									: "border-l-2 border-transparent"
							)}
							onclick={() => selectCamera(camera.device_id)}
							ondblclick={() => settingsStore.openCameraView(camera.device_id)}
						>
							<span
								class={cn(
									"h-2 w-2 rounded-full flex-shrink-0",
									camera.connected ? "bg-primary" : "bg-destructive"
								)}
							></span>

							<div class="flex-1 min-w-0">
								<div class="flex items-center gap-1.5">
									<span class="truncate font-medium">{displayName}</span>
								</div>
								{#if camera.telemetry}
									<div class="text-[10px] text-muted-foreground font-mono">
										CPU {camera.telemetry.cpu_percent.toFixed(0)}%
										&middot; {camera.telemetry.memory_mb.toFixed(0)}MB
									</div>
								{/if}
							</div>

							<span class={cn(
								"text-[10px] uppercase tracking-wider flex-shrink-0",
								camera.connected ? "text-primary" : "text-muted-foreground"
							)}>
								{camera.connected ? 'Live' : 'Off'}
							</span>
						</button>

						<button
							class="absolute right-8 top-1/2 -translate-y-1/2 p-1 rounded opacity-0 group-hover/item:opacity-100 hover:bg-accent transition-opacity"
							onclick={(e) => { e.stopPropagation(); startEdit(camera.device_id, displayName); }}
							aria-label="Edit camera"
						>
							<Pencil class="h-3 w-3 text-muted-foreground" />
						</button>
					{/if}
				</div>
			{/each}
		</div>
	{/each}

	{#if cameraStore.cameras.length === 0}
		<div class="px-3 py-8 text-center text-xs text-muted-foreground">
			No cameras connected
		</div>
	{/if}
</div>
