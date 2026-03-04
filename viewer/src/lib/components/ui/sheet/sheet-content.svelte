<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";

	let {
		class: className,
		side = "right",
		children,
		...restProps
	}: HTMLAttributes<HTMLDivElement> & {
		side?: "left" | "right" | "top" | "bottom";
		children?: Snippet;
	} = $props();

	const sideClasses: Record<string, string> = {
		left: "inset-y-0 left-0 h-full w-3/4 max-w-sm border-r slide-in-from-left",
		right: "inset-y-0 right-0 h-full w-3/4 max-w-sm border-l slide-in-from-right",
		top: "inset-x-0 top-0 border-b",
		bottom: "inset-x-0 bottom-0 border-t",
	};
</script>

<div
	class={cn(
		"fixed z-50 bg-background p-6 shadow-lg transition-transform duration-300",
		sideClasses[side],
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

<style>
	.slide-in-from-left {
		animation: slide-in-left 0.3s ease-out;
	}
	.slide-in-from-right {
		animation: slide-in-right 0.3s ease-out;
	}
	@keyframes slide-in-left {
		from { transform: translateX(-100%); }
		to { transform: translateX(0); }
	}
	@keyframes slide-in-right {
		from { transform: translateX(100%); }
		to { transform: translateX(0); }
	}
</style>
