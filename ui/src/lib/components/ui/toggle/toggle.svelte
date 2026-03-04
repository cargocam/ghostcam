<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLButtonAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";
	import { toggleVariants, type ToggleVariant, type ToggleSize } from "./index.js";

	let {
		pressed = $bindable(false),
		variant = "default",
		size = "default",
		class: className,
		children,
		...restProps
	}: HTMLButtonAttributes & {
		pressed?: boolean;
		variant?: ToggleVariant;
		size?: ToggleSize;
		children?: Snippet;
	} = $props();
</script>

<button
	class={cn(toggleVariants({ variant, size }), className)}
	aria-pressed={pressed}
	data-state={pressed ? "on" : "off"}
	onclick={() => (pressed = !pressed)}
	{...restProps}
>
	{#if children}
		{@render children()}
	{/if}
</button>
