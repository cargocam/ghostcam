<script lang="ts">
	import { onMount } from 'svelte';
	import type L from 'leaflet';

	let {
		map,
		leaflet,
		points,
		historicPoint,
	}: {
		map: L.Map;
		leaflet: typeof L;
		points: [number, number][];
		historicPoint: [number, number] | null;
	} = $props();

	let polyline: L.Polyline | null = null;
	let marker: L.CircleMarker | null = null;

	$effect(() => {
		if (!map || !leaflet) return;

		if (points.length >= 2) {
			if (!polyline) {
				polyline = leaflet.polyline(points, {
					color: '#38bdf8',
					weight: 2,
					opacity: 0.9,
					dashArray: '6 6',
				}).addTo(map);
			} else {
				polyline.setLatLngs(points);
			}
		} else if (polyline) {
			polyline.remove();
			polyline = null;
		}

		if (historicPoint) {
			if (!marker) {
				marker = leaflet.circleMarker(historicPoint, {
					radius: 5,
					color: '#38bdf8',
					weight: 2,
					fillColor: '#0ea5e9',
					fillOpacity: 0.95,
				}).addTo(map);
			} else {
				marker.setLatLng(historicPoint);
			}
		} else if (marker) {
			marker.remove();
			marker = null;
		}
	});

	onMount(() => {
		return () => {
			polyline?.remove();
			marker?.remove();
		};
	});
</script>
