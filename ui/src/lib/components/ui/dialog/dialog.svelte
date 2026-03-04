<script lang="ts">
	import type { Snippet } from "svelte";

	let {
		open = $bindable(false),
		children,
	}: {
		open?: boolean;
		children?: Snippet;
	} = $props();
</script>

{#if open}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="fixed inset-0 z-50 flex items-center justify-center" onkeydown={(e) => e.key === "Escape" && (open = false)}>
		<button
			class="fixed inset-0 bg-background/80 backdrop-blur-sm"
			onclick={() => (open = false)}
			aria-label="Close dialog"
			tabindex="-1"
		></button>
		{#if children}
			{@render children()}
		{/if}
	</div>
{/if}
