<script lang="ts">
	import type { Snippet } from "svelte";

	let {
		open = $bindable(false),
		children,
		trigger,
	}: {
		open?: boolean;
		children?: Snippet;
		trigger?: Snippet;
	} = $props();
</script>

<div class="relative inline-block text-left">
	{#if trigger}
		<button onclick={() => (open = !open)} class="inline-flex" aria-expanded={open} aria-haspopup="true">
			{@render trigger()}
		</button>
	{/if}

	{#if open}
		<button class="fixed inset-0 z-40" onclick={() => (open = false)} aria-label="Close menu" tabindex="-1"></button>
		<div
			class="absolute right-0 z-50 mt-2 min-w-[8rem] overflow-hidden rounded-md border bg-popover p-1 text-popover-foreground shadow-md"
			role="menu"
		>
			{#if children}
				{@render children()}
			{/if}
		</div>
	{/if}
</div>
