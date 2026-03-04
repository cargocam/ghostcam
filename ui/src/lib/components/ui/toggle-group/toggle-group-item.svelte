<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLButtonAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";
	import { getContext } from "svelte";

	let {
		value,
		class: className,
		children,
		...restProps
	}: HTMLButtonAttributes & {
		value: string;
		children?: Snippet;
	} = $props();

	const ctx = getContext<{ value: string }>("toggle-group");
	let active = $derived(ctx.value === value);
</script>

<button
	class={cn(
		"inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1.5 text-sm font-medium transition-all focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50",
		active ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
		className
	)}
	aria-pressed={active}
	data-state={active ? "on" : "off"}
	onclick={() => (ctx.value = value)}
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</button>
