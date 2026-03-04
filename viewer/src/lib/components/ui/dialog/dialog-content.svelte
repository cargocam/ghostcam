<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";

	let {
		class: className,
		children,
		...restProps
	}: HTMLAttributes<HTMLDivElement> & { children?: Snippet } = $props();
</script>

<div
	class={cn(
		"relative z-50 w-full max-w-lg rounded-lg border bg-background p-6 shadow-lg",
		className
	)}
	role="dialog"
	aria-modal="true"
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</div>
