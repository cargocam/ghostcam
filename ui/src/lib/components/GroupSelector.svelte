<script lang="ts">
	import { groupStore } from '$lib/stores/groups.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { cn } from '$lib/utils.js';

	let groups = $derived(groupStore.groups);
	let activeId = $derived(groupStore.activeGroupId);
	let totalCameras = $derived(groups.reduce((sum, g) => sum + g.camera_count, 0));

	function switchGroup(groupId: string) {
		if (groupId === activeId) return;
		transportStore.switchGroup(groupId);
	}
</script>

{#if groups.length > 0}
	<div class="flex flex-wrap gap-1 px-3 py-2">
		{#if groups.length > 1}
			<button
				class={cn(
					"text-[10px] px-2 py-0.5 rounded-full border transition-colors",
					activeId === '__all__'
						? "bg-primary text-primary-foreground border-primary"
						: "border-border text-muted-foreground hover:border-foreground/30"
				)}
				onclick={() => switchGroup('__all__')}
			>
				All
				<span class="ml-1 opacity-60">{totalCameras}</span>
			</button>
		{/if}
		{#each groups as group (group.group_id)}
			<button
				class={cn(
					"text-[10px] px-2 py-0.5 rounded-full border transition-colors",
					activeId === group.group_id
						? "bg-primary text-primary-foreground border-primary"
						: "border-border text-muted-foreground hover:border-foreground/30"
				)}
				onclick={() => switchGroup(group.group_id)}
			>
				{group.group_id}
				<span class="ml-1 opacity-60">{group.camera_count}</span>
			</button>
		{/each}
	</div>
{/if}
