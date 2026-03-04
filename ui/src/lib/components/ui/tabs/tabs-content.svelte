<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";
	import { getContext } from "svelte";

	let {
		value,
		class: className,
		children,
		...restProps
	}: HTMLAttributes<HTMLDivElement> & {
		value: string;
		children?: Snippet;
	} = $props();

	const ctx = getContext<{ value: string }>("tabs");
	let active = $derived(ctx.value === value);
</script>

{#if active}
	<div
		class={cn("mt-2 ring-offset-background focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2", className)}
		role="tabpanel"
		{...restProps}
	>
		{#if children}
			{@render children()}
		{/if}
	</div>
{/if}
