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

<div class={cn("relative overflow-hidden", className)} {...restProps}>
	<div class="h-full w-full overflow-auto scrollbar-thin scrollbar-thumb-border scrollbar-track-transparent">
		{#if children}
			{@render children()}
		{/if}
	</div>
</div>
