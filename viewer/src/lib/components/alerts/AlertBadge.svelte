<script lang="ts">
	import { alertsStore } from '$lib/stores/alerts.svelte.js';
</script>

{#if alertsStore.unreadCount > 0}
	<span
		class="inline-flex items-center justify-center min-w-[18px] h-[18px] px-1 text-[10px] font-bold leading-none text-white bg-destructive rounded-full"
	>
		{alertsStore.unreadCount > 99 ? '99+' : alertsStore.unreadCount}
	</span>
{/if}
