<script lang="ts">
	import type { Snippet } from "svelte";

	let {
		text,
		children,
	}: {
		text: string;
		children?: Snippet;
	} = $props();
</script>

<div class="group relative inline-flex">
	{#if children}
		{@render children()}
	{/if}
	<div
		class="pointer-events-none absolute bottom-full left-1/2 z-50 mb-2 -translate-x-1/2 rounded-md bg-popover px-3 py-1.5 text-xs text-popover-foreground shadow-md border opacity-0 transition-opacity group-hover:opacity-100"
		role="tooltip"
	>
		{text}
	</div>
</div>
