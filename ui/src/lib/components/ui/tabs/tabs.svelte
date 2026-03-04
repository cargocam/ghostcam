<script lang="ts">
	import type { Snippet } from "svelte";
	import type { HTMLAttributes } from "svelte/elements";
	import { cn } from "$lib/utils.js";
	import { setContext } from "svelte";

	let {
		value = $bindable(""),
		class: className,
		children,
		...restProps
	}: HTMLAttributes<HTMLDivElement> & {
		value?: string;
		children?: Snippet;
	} = $props();

	const ctx = {
		get value() { return value; },
		set value(v: string) { value = v; },
	};
	setContext("tabs", ctx);
</script>

<div class={cn("", className)} {...restProps}>
	{#if children}
		{@render children()}
	{/if}
</div>
