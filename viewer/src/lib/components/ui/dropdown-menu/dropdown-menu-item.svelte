<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLButtonAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";

	let {
		class: className,
		children,
		...restProps
	}: HTMLButtonAttributes & { children?: Snippet } = $props();
</script>

<button
	class={cn(
		"relative flex w-full cursor-default select-none items-center rounded-sm px-2 py-1.5 text-sm outline-none transition-colors hover:bg-accent hover:text-accent-foreground focus:bg-accent focus:text-accent-foreground",
		className
	)}
	role="menuitem"
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</button>
