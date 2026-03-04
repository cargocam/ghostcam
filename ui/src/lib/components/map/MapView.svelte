<script lang="ts">
	import { onMount } from 'svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import CameraMarker from './CameraMarker.svelte';
	import { Locate, LocateFixed } from 'lucide-svelte';
	import type * as Leaflet from 'leaflet';
	import 'leaflet/dist/leaflet.css';

	let mapContainer: HTMLDivElement;
	let map = $state<Leaflet.Map | null>(null);
	let L = $state<typeof Leaflet | null>(null);
	let tileLayer: Leaflet.TileLayer | null = null;
	let satellite = $state(false);
	let ready = $state(false);

	// Tracking state
	let tracking = $state<'all' | 'single' | 'off'>('off');
	let trackedDeviceId = $state<string | null>(null);
	let programmaticMove = false;

	// Cameras with GPS telemetry
	let gpsCameras = $derived(
		cameraStore.cameras.filter((c) => c.telemetry?.gps)
	);

	let markerMode = $derived(settingsStore.markerMode);

	// Padding accounts for marker size so fitBounds doesn't clip them
	// dot: 12x12 center-anchored, detailed: 160x56 bottom-center, pip: 160x110 bottom-center
	let fitPadding = $derived<[number, number]>(
		markerMode === 'pip' ? [130, 100] :
		markerMode === 'detailed' ? [70, 100] :
		[30, 30]
	);

	const CARTO_DARK = 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png';
	const CARTO_LIGHT = 'https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png';
	const ARCGIS_SAT = 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}';

	let isDark = $derived(
		settingsStore.theme === 'dark' ||
		(settingsStore.theme === 'system' && typeof window !== 'undefined' && window.matchMedia('(prefers-color-scheme: dark)').matches)
	);

	function getTileUrl(): string {
		if (satellite) return ARCGIS_SAT;
		return isDark ? CARTO_DARK : CARTO_LIGHT;
	}

	$effect(() => {
		// Re-apply tile layer when theme or satellite changes
		const url = getTileUrl();
		if (map && tileLayer && L) {
			tileLayer.setUrl(url);
		}
	});

	// Auto-fit / pan effect
	$effect(() => {
		if (!map || !L) return;
		const _tracking = tracking;
		const _trackedId = trackedDeviceId;
		const _cams = gpsCameras;

		if (_tracking === 'all' && _cams.length > 0) {
			const bounds = L.latLngBounds(
				_cams.map((c) => [c.telemetry!.gps!.latitude, c.telemetry!.gps!.longitude] as [number, number])
			);
			programmaticMove = true;
			map.fitBounds(bounds, { padding: fitPadding, maxZoom: 16 });
			// fitBounds is async — reset flag after moveend
			map.once('moveend', () => { programmaticMove = false; });
		} else if (_tracking === 'single' && _trackedId) {
			const cam = _cams.find((c) => c.device_id === _trackedId);
			if (cam?.telemetry?.gps) {
				const latlng: [number, number] = [cam.telemetry.gps.latitude, cam.telemetry.gps.longitude];
				programmaticMove = true;
				if (map.getZoom() < 14) {
					map.setView(latlng, 14);
				} else {
					map.panTo(latlng);
				}
				map.once('moveend', () => { programmaticMove = false; });
			}
		}
	});

	onMount(() => {
		let observer: ResizeObserver | undefined;

		(async () => {
			const leaflet = await import('leaflet');
			L = leaflet;

			map = L.map(mapContainer, {
				center: [39.8283, -98.5795],
				zoom: 4,
				zoomControl: true,
				attributionControl: false,
			});

			tileLayer = L.tileLayer(getTileUrl(), {
				maxZoom: 19,
			}).addTo(map);

			// Disengage tracking on user interaction
			map.on('dragstart', () => {
				if (!programmaticMove) disengageTracking();
			});
			map.on('zoomstart', () => {
				if (!programmaticMove) disengageTracking();
			});

			// Click on map background stops single tracking
			map.on('click', () => {
				if (tracking === 'single') {
					disengageTracking();
				}
			});

			ready = true;

			observer = new ResizeObserver(() => {
				map?.invalidateSize();
			});
			observer.observe(mapContainer);
		})();

		return () => {
			observer?.disconnect();
			map?.remove();
			map = null;
		};
	});

	function disengageTracking() {
		tracking = 'off';
		trackedDeviceId = null;
		cameraStore.selectedId = null;
	}

	function toggleFocus() {
		if (tracking === 'off') {
			tracking = 'all';
			trackedDeviceId = null;
			cameraStore.selectedId = null;
		} else {
			disengageTracking();
		}
	}

	function handleMarkerClick(deviceId: string) {
		if (tracking === 'single' && trackedDeviceId === deviceId) {
			disengageTracking();
		} else {
			tracking = 'single';
			trackedDeviceId = deviceId;
			cameraStore.selectedId = deviceId;
		}
	}

	function toggleSatellite() {
		satellite = !satellite;
	}
</script>

<div class="relative isolate h-full w-full">
	<div bind:this={mapContainer} class="h-full w-full"></div>

	{#if ready && gpsCameras.length === 0}
		<div class="absolute inset-0 flex items-center justify-center pointer-events-none">
			<div class="bg-background/90 backdrop-blur rounded-lg px-6 py-4 text-center shadow-lg pointer-events-auto">
				<p class="text-sm text-muted-foreground">No cameras with GPS data</p>
				<p class="text-xs text-muted-foreground/70 mt-1">GPS telemetry will appear here when available</p>
			</div>
		</div>
	{/if}

	{#if ready && map && L}
		{#each gpsCameras as camera (camera.device_id)}
			<CameraMarker
				{map}
				leaflet={L}
				{camera}
				selected={tracking === 'single' && trackedDeviceId === camera.device_id}
				onMarkerClick={handleMarkerClick}
			/>
		{/each}
	{/if}

	<!-- Satellite toggle -->
	<div class="absolute top-3 right-3 z-[1000]">
		<button
			onclick={toggleSatellite}
			class="rounded-md bg-background/90 backdrop-blur px-3 py-1.5 text-xs font-medium shadow-md border hover:bg-accent transition-colors"
		>
			{satellite ? 'Map' : 'Satellite'}
		</button>
	</div>

	<!-- Focus / tracking toggle -->
	{#if ready && gpsCameras.length > 0}
		<div class="absolute bottom-6 right-3 z-[1000]">
			<button
				onclick={toggleFocus}
				class="rounded-md bg-background/90 backdrop-blur p-2 shadow-md border hover:bg-accent transition-colors"
				title={tracking === 'off' ? 'Focus on cameras' : 'Stop tracking'}
			>
				{#if tracking === 'off'}
					<Locate class="h-4 w-4" />
				{:else}
					<LocateFixed class="h-4 w-4 text-green-500" />
				{/if}
			</button>
		</div>
	{/if}
</div>
