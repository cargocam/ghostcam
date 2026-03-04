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

	const ctx = getContext<{ value: string }>("tabs");
	let active = $derived(ctx.value === value);
</script>

<button
	class={cn(
		"inline-flex items-center justify-center whitespace-nowrap rounded-md px-3 py-1 text-sm font-medium ring-offset-background transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50",
		active && "bg-background text-foreground shadow",
		className
	)}
	role="tab"
	aria-selected={active}
	onclick={() => (ctx.value = value)}
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</button>
