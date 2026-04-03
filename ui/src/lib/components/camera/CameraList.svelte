<script lang="ts">
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { deleteCamera, updateCameraSettings } from '$lib/signaling.js';
	import { cn } from '$lib/utils.js';
	import { DropdownMenu } from 'bits-ui';
	import { MoreVertical, Pencil, Trash2, Check, X } from 'lucide-svelte';
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';

	let {
		onSelect,
	}: {
		onSelect?: () => void;
	} = $props();

	let editingId = $state<string | null>(null);
	let editingName = $state('');
	let deleteTarget = $state<{ id: string; name: string } | null>(null);
	let deleting = $state(false);

	let sortedCameras = $derived(() => {
		return [...cameraStore.cameras].sort((a, b) => {
			if (a.online !== b.online) return a.online ? -1 : 1;
			const nameA = cameraConfigStore.getDisplayName(a.device_id, a.device_name);
			const nameB = cameraConfigStore.getDisplayName(b.device_id, b.device_name);
			return nameA.localeCompare(nameB);
		});
	});

	function selectCamera(id: string) {
		cameraStore.select(id);
		onSelect?.();
	}

	function startEdit(id: string, currentName: string) {
		editingId = id;
		editingName = currentName;
	}

	async function confirmEdit() {
		if (editingId && editingName.trim()) {
			const name = editingName.trim();
			// Persist to server
			try {
				await updateCameraSettings(editingId, { display_name: name });
			} catch {
				// Fall back to local-only rename
			}
			cameraConfigStore.rename(editingId, name);
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

	function promptDelete(id: string, name: string) {
		deleteTarget = { id, name };
	}

	async function confirmDelete() {
		if (!deleteTarget) return;
		deleting = true;
		try {
			await deleteCamera(deleteTarget.id);
			cameraStore.removeCamera(deleteTarget.id);
			deleteTarget = null;
		} catch (err) {
			console.error('Delete camera failed:', err);
		} finally {
			deleting = false;
		}
	}

	function handleContextMenu(e: MouseEvent, id: string) {
		// Prevent default browser context menu — the DropdownMenu handles display
		// We don't need to do anything here since we use the trigger button approach
	}
</script>

<div class="py-1">
	<div class="space-y-0.5">
		{#each sortedCameras() as camera (camera.device_id)}
			{@const displayName = cameraConfigStore.getDisplayName(camera.device_id, camera.device_name)}
			<div class="group/item relative">
				{#if editingId === camera.device_id}
					<div class="px-3 py-2 space-y-1.5">
						<div class="flex items-center gap-1">
							<span
								class={cn(
									"h-2 w-2 rounded-full flex-shrink-0",
									camera.online ? "bg-primary" : "bg-destructive"
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
								camera.online ? "bg-primary" : "bg-destructive"
							)}
						></span>

						<div class="flex-1 min-w-0">
							<div class="flex items-center gap-1.5">
								<span class="truncate font-medium">{displayName}</span>
							</div>
							{#if camera.telemetry}
								<div class="text-[10px] text-muted-foreground font-mono">
									CPU {(camera.telemetry.cpu_percent ?? 0).toFixed(0)}%
									&middot; {(camera.telemetry.memory_mb ?? 0).toFixed(0)}MB
								</div>
							{/if}
						</div>

						<span class={cn(
							"text-[10px] uppercase tracking-wider flex-shrink-0",
							camera.online ? "text-primary" : "text-muted-foreground"
						)}>
							{camera.online ? 'Live' : 'Off'}
						</span>
					</button>

					<!-- Context menu trigger -->
					<div class="absolute right-2 top-1/2 -translate-y-1/2 opacity-0 group-hover/item:opacity-100 transition-opacity">
						<DropdownMenu.Root>
							<DropdownMenu.Trigger
								class="p-1 rounded hover:bg-accent"
								onclick={(e: MouseEvent) => e.stopPropagation()}
							>
								<MoreVertical class="h-3.5 w-3.5 text-muted-foreground" />
							</DropdownMenu.Trigger>
							<DropdownMenu.Content
								class="z-50 min-w-[140px] rounded-md border bg-popover p-1 shadow-md"
								sideOffset={4}
							>
								<DropdownMenu.Item
									class="flex cursor-pointer items-center gap-2 rounded-sm px-2 py-1.5 text-sm hover:bg-accent outline-none"
									onSelect={() => startEdit(camera.device_id, displayName)}
								>
									<Pencil class="h-3.5 w-3.5" />
									Rename
								</DropdownMenu.Item>
								<DropdownMenu.Item
									class="flex cursor-pointer items-center gap-2 rounded-sm px-2 py-1.5 text-sm text-destructive hover:bg-destructive/10 outline-none"
									onSelect={() => promptDelete(camera.device_id, displayName)}
								>
									<Trash2 class="h-3.5 w-3.5" />
									Delete
								</DropdownMenu.Item>
							</DropdownMenu.Content>
						</DropdownMenu.Root>
					</div>
				{/if}
			</div>
		{/each}
	</div>

	{#if cameraStore.cameras.length === 0}
		<div class="px-3 py-8 text-center text-xs text-muted-foreground">
			No cameras connected
		</div>
	{/if}
</div>

<!-- Delete confirmation dialog -->
{#if deleteTarget}
	<Dialog open={true}>
		<DialogContent class="sm:max-w-md">
			<DialogHeader>
				<DialogTitle>Delete Camera</DialogTitle>
				<DialogDescription>
					Are you sure you want to delete <strong>{deleteTarget.name}</strong>? This action cannot be undone. All recordings for this camera will remain in storage.
				</DialogDescription>
			</DialogHeader>
			<div class="flex justify-end gap-2 pt-4">
				<Button variant="outline" onclick={() => { deleteTarget = null; }}>
					Cancel
				</Button>
				<Button variant="destructive" disabled={deleting} onclick={confirmDelete}>
					{deleting ? 'Deleting...' : 'Delete'}
				</Button>
			</div>
		</DialogContent>
	</Dialog>
{/if}
