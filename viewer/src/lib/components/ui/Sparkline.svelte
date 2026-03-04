<script lang="ts">
	import { cn } from '$lib/utils.js';

	let {
		data = [],
		width = 120,
		height = 32,
		color = 'currentColor',
		class: className,
	}: {
		data?: number[];
		width?: number;
		height?: number;
		color?: string;
		class?: string;
	} = $props();

	let points = $derived.by(() => {
		if (data.length < 2) return '';
		const max = Math.max(...data, 1);
		const step = width / (data.length - 1);
		return data.map((v, i) => `${i * step},${height - (v / max) * (height - 2) - 1}`).join(' ');
	});

	let fillPoints = $derived.by(() => {
		if (data.length < 2) return '';
		const max = Math.max(...data, 1);
		const step = width / (data.length - 1);
		const line = data.map((v, i) => `${i * step},${height - (v / max) * (height - 2) - 1}`);
		return `0,${height} ${line.join(' ')} ${width},${height}`;
	});
</script>

<svg
	{width}
	{height}
	viewBox="0 0 {width} {height}"
	class={cn("inline-block", className)}
	aria-hidden="true"
>
	{#if data.length >= 2}
		<polygon points={fillPoints} fill={color} opacity="0.1" />
		<polyline points={points} fill="none" stroke={color} stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
	{/if}
</svg>
