<script lang="ts">
	import { onMount } from 'svelte';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import { fetchTelemetryRangeCached, nearestTelemetryEntryWithin } from '$lib/telemetry-history.js';
	import CameraMarker from './CameraMarker.svelte';
	import PlaybackTrailOverlay from './PlaybackTrailOverlay.svelte';
	import { Locate, LocateFixed } from 'lucide-svelte';
	import type * as Leaflet from 'leaflet';
	import 'leaflet/dist/leaflet.css';

	let mapContainer: HTMLDivElement;
	let map = $state<Leaflet.Map | null>(null);
	let L = $state<typeof Leaflet | null>(null);
	let tileLayer: Leaflet.TileLayer | null = null;
	let satellite = $state(false);
	let ready = $state(false);

	// Tracking state — on by default, disengaged only by manual map interaction
	let tracking = $state<'all' | 'single' | 'off'>('all');
	let trackedDeviceId = $state<string | null>(null);
	let programmaticMove = false;
	let playbackTrailsByDevice = $state<Record<string, [number, number][]>>({});
	let playbackPointByDevice = $state<Record<string, [number, number] | null>>({});
	let playbackFetchTimer: ReturnType<typeof setTimeout> | null = null;
	const MAX_GPS_STALENESS_MS = 21600000;

	// Cameras with live GPS telemetry
	let gpsCameras = $derived(
		cameraStore.cameras.filter((c) => c.telemetry?.gps)
	);

	// Marker source list: in playback, include cameras with historical playback points too.
	let markerCameras = $derived.by(() => {
		if (scrubberStore.isLive) return gpsCameras;
		return cameraStore.cameras.filter((c) => !!playbackPointByDevice[c.device_id] || !!c.telemetry?.gps);
	});

	let markerMode = $derived(settingsStore.markerMode);
	let SHOW_PLAYBACK_DEBUG = $derived(settingsStore.debugMode);

	// Track viewport width reactively so fitPadding and marker size adapt to
	// small screens where detailed/pip panels would otherwise overflow.
	let viewportWidth = $state(typeof window !== 'undefined' ? window.innerWidth : 1024);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const onResize = () => { viewportWidth = window.innerWidth; };
		window.addEventListener('resize', onResize);
		return () => window.removeEventListener('resize', onResize);
	});
	let isNarrow = $derived(viewportWidth < 640);
	let mapSourceDebug = $derived.by(() => {
		return cameraStore.cameras.map((camera) => {
			const playbackPoint = playbackPointByDevice[camera.device_id];
			const hasLive = !!camera.telemetry?.gps;
			const source = !scrubberStore.isLive
				? (playbackPoint ? 'playback' : (hasLive ? 'live-fallback' : 'none'))
				: (hasLive ? 'live' : 'none');
			return {
				deviceId: camera.device_id,
				source,
				hasPlaybackPoint: !!playbackPoint,
				hasLive
			};
		});
	});

	// Padding (half of the panel size + margin) keeps markers inside the
	// visible area after fitBounds. The detailed/pip panels hang off the dot
	// corner so we need roughly the panel extent on the leading axes.
	let fitPadding: [number, number] = $derived.by(() => {
		if (markerMode === 'dot') return [30, 30];
		if (isNarrow) {
			// Smaller panels on narrow screens — see CameraMarker.
			// Panel extent from the dot: 128 + 14 = 142px horizontally.
			return markerMode === 'pip' ? [145, 120] : [145, 75];
		}
		// Full-size panels: 160 + 14 = 174px horizontally.
		return markerMode === 'pip' ? [180, 135] : [180, 80];
	});

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

	$effect(() => {
		const isLive = scrubberStore.isLive;
		const playheadTime = Math.floor(scrubberStore.playheadTime);
		const cameraIds = cameraStore.cameras.map((c) => c.device_id);

		if (isLive || cameraIds.length === 0) {
			playbackTrailsByDevice = {};
			playbackPointByDevice = {};
			if (playbackFetchTimer) {
				clearTimeout(playbackFetchTimer);
				playbackFetchTimer = null;
			}
			return;
		}

		if (playbackFetchTimer) return;
		playbackFetchTimer = setTimeout(async () => {
			const targetMs = Math.floor(scrubberStore.playheadTime * 1000);
			const nearRadiusMs = 20 * 60 * 1000;
			const fallbackRadiusMs = 6 * 60 * 60 * 1000;
			const fromMs = Math.max(0, targetMs - nearRadiusMs);
			const toMs = targetMs + nearRadiusMs;
			const nextTrails: Record<string, [number, number][]> = {};
			const nextPoints: Record<string, [number, number] | null> = {};

			await Promise.all(
				cameraIds.map(async (deviceId) => {
					try {
						const nearEntries = await fetchTelemetryRangeCached(deviceId, fromMs, toMs, 1200);
						let gpsEntries = nearEntries.filter(
							(e) => typeof e.lat === 'number' && typeof e.lon === 'number',
						);
						let nearest = nearestTelemetryEntryWithin(
							gpsEntries,
							targetMs,
							MAX_GPS_STALENESS_MS,
						);
						if (!nearest) {
							const wideFrom = Math.max(0, targetMs - fallbackRadiusMs);
							const wideTo = targetMs + fallbackRadiusMs;
							const wideEntries = await fetchTelemetryRangeCached(deviceId, wideFrom, wideTo, 1200);
							gpsEntries = wideEntries.filter(
								(e) => typeof e.lat === 'number' && typeof e.lon === 'number',
							);
							nearest = nearestTelemetryEntryWithin(
								gpsEntries,
								targetMs,
								MAX_GPS_STALENESS_MS,
							);
						}
						if (!nearest) {
							nextTrails[deviceId] = [];
							nextPoints[deviceId] = null;
							return;
						}
						nextTrails[deviceId] = gpsEntries.map((e) => [e.lat!, e.lon!] as [number, number]);
						nextPoints[deviceId] = [nearest.lat!, nearest.lon!];
					} catch {
						nextTrails[deviceId] = [];
						nextPoints[deviceId] = null;
					}
				}),
			);

			playbackTrailsByDevice = nextTrails;
			playbackPointByDevice = nextPoints;
			playbackFetchTimer = null;
		}, 260);

	});

	// Compute offset angles for overlapping markers.
	// Only markers whose dots are very close (<50px) get spread. Others stay
	// at the default top-right (315°). Uses union-find to build clusters so
	// only mutual neighbors are grouped — an isolated camera far from a pair
	// never gets pulled into their cluster.
	const OVERLAP_PX = 50;
	let markerOffsets = $derived.by((): Record<string, number> => {
		if (!map || !L) return {};
		const positions: { id: string; px: { x: number; y: number } }[] = [];
		for (const cam of markerCameras) {
			const gpsOv = !scrubberStore.isLive ? playbackPointByDevice[cam.device_id] : null;
			const gps = gpsOv ? { latitude: gpsOv[0], longitude: gpsOv[1] } : cam.telemetry?.gps;
			if (!gps) continue;
			const pt = map.latLngToContainerPoint([gps.latitude, gps.longitude]);
			positions.push({ id: cam.device_id, px: pt });
		}

		// Union-find to build clusters of overlapping markers
		const parent = positions.map((_, i) => i);
		function find(x: number): number { return parent[x] === x ? x : (parent[x] = find(parent[x])); }
		function union(a: number, b: number) { parent[find(a)] = find(b); }

		for (let i = 0; i < positions.length; i++) {
			for (let j = i + 1; j < positions.length; j++) {
				const dx = positions[i].px.x - positions[j].px.x;
				const dy = positions[i].px.y - positions[j].px.y;
				if (Math.sqrt(dx * dx + dy * dy) < OVERLAP_PX) {
					union(i, j);
				}
			}
		}

		// Group by cluster root
		const clusters = new Map<number, number[]>();
		for (let i = 0; i < positions.length; i++) {
			const root = find(i);
			if (!clusters.has(root)) clusters.set(root, []);
			clusters.get(root)!.push(i);
		}

		const offsets: Record<string, number> = {};
		for (const members of clusters.values()) {
			if (members.length === 1) {
				offsets[positions[members[0]].id] = 315; // default: top-right
			} else {
				// Spread cluster members evenly
				const angleStep = 360 / members.length;
				for (let rank = 0; rank < members.length; rank++) {
					offsets[positions[members[rank]].id] = (315 + rank * angleStep) % 360;
				}
			}
		}
		return offsets;
	});

	// Re-engage tracking when scrubbing or returning to live
	$effect(() => {
		// Track these reactive values to trigger on change
		const _seekTarget = scrubberStore.seekTarget;
		const _isLive = scrubberStore.isLive;
		if (tracking === 'off') {
			tracking = 'all';
		}
	});

	// Resolve current GPS positions: playback points when scrubbing, live telemetry otherwise
	let trackablePoints = $derived.by(() => {
		const points: { deviceId: string; lat: number; lon: number }[] = [];
		for (const cam of cameraStore.cameras) {
			if (!scrubberStore.isLive) {
				const pb = playbackPointByDevice[cam.device_id];
				if (pb) {
					points.push({ deviceId: cam.device_id, lat: pb[0], lon: pb[1] });
					continue;
				}
			}
			if (cam.telemetry?.gps) {
				points.push({ deviceId: cam.device_id, lat: cam.telemetry.gps.latitude, lon: cam.telemetry.gps.longitude });
			}
		}
		return points;
	});

	// Auto-fit / pan effect
	$effect(() => {
		if (!map || !L) return;
		const _tracking = tracking;
		const _trackedId = trackedDeviceId;
		const _points = trackablePoints;

		if (_tracking === 'all' && _points.length > 0) {
			const bounds = L.latLngBounds(
				_points.map((p) => [p.lat, p.lon] as [number, number])
			);
			programmaticMove = true;
			map.fitBounds(bounds, { padding: fitPadding, maxZoom: 16 });
			map.once('moveend', () => { programmaticMove = false; });
		} else if (_tracking === 'single' && _trackedId) {
			const pt = _points.find((p) => p.deviceId === _trackedId);
			if (pt) {
				const latlng: [number, number] = [pt.lat, pt.lon];
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
		tracking = 'all';
		trackedDeviceId = null;
		cameraStore.selectedId = null;
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

	{#if ready && markerCameras.length === 0}
		<div class="absolute inset-0 flex items-center justify-center pointer-events-none">
			<div class="bg-background/90 backdrop-blur rounded-lg px-6 py-4 text-center shadow-lg pointer-events-auto">
				<p class="text-sm text-muted-foreground">No cameras with GPS data</p>
				<p class="text-xs text-muted-foreground/70 mt-1">GPS telemetry will appear here when available</p>
			</div>
		</div>
	{/if}

	{#if ready && map && L}
		{#each markerCameras as camera (camera.device_id)}
			{@const playbackPoint = playbackPointByDevice[camera.device_id]}
			<CameraMarker
				{map}
				leaflet={L}
				{camera}
				gpsOverride={!scrubberStore.isLive && playbackPoint
					? { latitude: playbackPoint[0], longitude: playbackPoint[1] }
					: undefined}
				selected={tracking === 'single' && trackedDeviceId === camera.device_id}
				offsetAngle={markerOffsets[camera.device_id] ?? 315}
				compact={isNarrow}
				onMarkerClick={handleMarkerClick}
			/>
		{/each}
	{/if}

	{#if ready && map && L && !scrubberStore.isLive}
		{#each markerCameras as camera (camera.device_id)}
			{@const points = playbackTrailsByDevice[camera.device_id] ?? []}
			{@const historicPoint = playbackPointByDevice[camera.device_id] ?? null}
			{#if points.length > 0 || historicPoint}
				<PlaybackTrailOverlay
					{map}
					leaflet={L}
					{points}
					{historicPoint}
				/>
			{/if}
		{/each}
	{/if}

	<!-- Satellite toggle -->
	<div class="absolute top-3 right-3 z-[1000]">
		<button
			onclick={toggleSatellite}
			class="rounded-md bg-background/90 backdrop-blur px-3 py-1.5 text-xs font-medium shadow-md border hover:bg-accent transition-colors"
		>
			{satellite ? 'Street' : 'Satellite'}
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
					<LocateFixed class="h-4 w-4 text-primary" />
				{/if}
			</button>
		</div>
	{/if}

	{#if SHOW_PLAYBACK_DEBUG}
		<div class="absolute left-3 bottom-3 z-[1000] rounded-md bg-black/70 px-2 py-1 text-[10px] font-mono text-white/85 pointer-events-none">
			<div>map-mode={scrubberStore.isLive ? 'live' : 'seeking'}</div>
			{#each mapSourceDebug as item (item.deviceId)}
				<div>
					{item.deviceId.slice(0, 8)} src={item.source} live={item.hasLive ? '1' : '0'} pb={item.hasPlaybackPoint ? '1' : '0'}
				</div>
			{/each}
		</div>
	{/if}
</div>
