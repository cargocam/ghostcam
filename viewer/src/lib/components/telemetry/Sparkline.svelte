<script lang="ts">
	import { cn } from '$lib/utils.js';

	let {
		data = [],
		width = 80,
		height = 24,
		color = 'var(--color-primary)',
		class: className,
	}: {
		data?: number[];
		width?: number;
		height?: number;
		color?: string;
		class?: string;
	} = $props();

	let pathD = $derived(() => {
		if (data.length < 2) return '';
		const max = Math.max(...data);
		const min = Math.min(...data);
		const range = max - min || 1;
		const step = width / (data.length - 1);

		const points = data.map((v, i) => {
			const x = i * step;
			const y = height - ((v - min) / range) * (height - 2) - 1;
			return `${x},${y}`;
		});

		return `M${points.join('L')}`;
	});

	let fillD = $derived(() => {
		if (data.length < 2) return '';
		return `${pathD()}L${width},${height}L0,${height}Z`;
	});
</script>

<svg
	class={cn("inline-block", className)}
	viewBox="0 0 {width} {height}"
	height={height}
	preserveAspectRatio="none"
>
	{#if data.length >= 2}
		<path d={fillD()} fill={color} fill-opacity="0.1" />
		<path d={pathD()} fill="none" stroke={color} stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
	{/if}
</svg>
