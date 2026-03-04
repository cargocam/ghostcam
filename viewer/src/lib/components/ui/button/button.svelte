<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLButtonAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";
	import { buttonVariants, type ButtonSize, type ButtonVariant } from "./index.js";

	let {
		class: className,
		variant = "default",
		size = "default",
		children,
		...restProps
	}: HTMLButtonAttributes & {
		variant?: ButtonVariant;
		size?: ButtonSize;
		children?: Snippet;
	} = $props();
</script>

<button class={cn(buttonVariants({ variant, size }), className)} {...restProps}>
	{#if children}
		{@render children()}
	{/if}
</button>
