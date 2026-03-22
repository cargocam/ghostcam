<script lang="ts">
	import { onMount } from 'svelte';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import type { CameraState } from '$lib/stores/cameras.svelte.js';
	import type L from 'leaflet';

	let {
		map,
		leaflet,
		camera,
		gpsOverride = undefined,
		selected = false,
		onMarkerClick,
	}: {
		map: L.Map;
		leaflet: typeof L;
		camera: CameraState;
		gpsOverride?: { latitude: number; longitude: number } | undefined;
		selected?: boolean;
		onMarkerClick?: (deviceId: string) => void;
	} = $props();

	let marker: L.Marker | null = null;
	let videoEl: HTMLVideoElement | null = null;
	let lastPipKey = '';

	let gps = $derived(gpsOverride ?? camera.telemetry?.gps);
	let displayName = $derived(cameraConfigStore.getDisplayName(camera.device_id, camera.device_name));
	let markerMode = $derived(settingsStore.markerMode);

	function pipKey(): string {
		return `${selected}|${camera.online}|${displayName}`;
	}

	function createIcon(): L.DivIcon {
		const t = camera.telemetry;
		const online = camera.online;
		const ring = selected ? 'box-shadow:0 0 0 3px #22c55e,0 1px 3px rgba(0,0,0,0.3)' : 'box-shadow:0 1px 3px rgba(0,0,0,0.3)';

		if (markerMode === 'dot') {
			return leaflet.divIcon({
				className: '',
				iconSize: [12, 12],
				iconAnchor: [6, 6],
				html: `<div style="width:12px;height:12px;border-radius:50%;background:${online ? '#22c55e' : '#ef4444'};border:2px solid white;${ring}"></div>`,
			});
		}

		if (markerMode === 'pip') {
			const pipBorder = selected
				? 'border:2px solid #22c55e;box-shadow:0 0 0 2px #22c55e,0 2px 8px rgba(0,0,0,0.4)'
				: 'border:1px solid rgba(255,255,255,0.15);box-shadow:0 2px 8px rgba(0,0,0,0.4)';
			const statusDot = `<span style="width:6px;height:6px;border-radius:50%;background:${online ? '#22c55e' : '#ef4444'};flex-shrink:0"></span>`;

			return leaflet.divIcon({
				className: '',
				iconSize: [160, 110],
				iconAnchor: [80, 110],
				html: `
					<div style="border-radius:8px;overflow:hidden;${pipBorder};background:#000">
						<div class="pip-video-slot" style="width:160px;height:90px;background:#1a1a2e"></div>
						<div style="display:flex;align-items:center;gap:5px;padding:3px 8px;background:rgba(0,0,0,0.9);color:white;font-size:10px;font-family:monospace">
							${statusDot}
							<span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${displayName}</span>
						</div>
					</div>
				`,
			});
		}

		// Detailed mode
		const cpu = t?.cpu_percent?.toFixed(0) ?? '--';
		const mem = t?.memory_mb?.toFixed(0) ?? '--';
		const temp = t?.temp_celsius ? `${t.temp_celsius.toFixed(0)}°` : '';
		const statusColor = online ? '#22c55e' : '#ef4444';
		const detailedBorder = selected
			? 'border:2px solid #22c55e;box-shadow:0 0 0 2px #22c55e,0 2px 8px rgba(0,0,0,0.3)'
			: 'border:1px solid rgba(255,255,255,0.1);box-shadow:0 2px 8px rgba(0,0,0,0.3)';

		return leaflet.divIcon({
			className: '',
			iconSize: [160, 56],
			iconAnchor: [80, 56],
			html: `
				<div style="background:rgba(0,0,0,0.85);border-radius:8px;padding:6px 10px;color:white;font-size:11px;font-family:monospace;white-space:nowrap;${detailedBorder}">
					<div style="display:flex;align-items:center;gap:6px;margin-bottom:3px">
						<span style="width:6px;height:6px;border-radius:50%;background:${statusColor};flex-shrink:0"></span>
						<span style="font-weight:600;overflow:hidden;text-overflow:ellipsis">${displayName}</span>
					</div>
					<div style="display:flex;gap:8px;color:rgba(255,255,255,0.7);font-size:10px">
						<span>CPU ${cpu}%</span>
						<span>${mem} MB</span>
						${temp ? `<span>${temp}C</span>` : ''}
					</div>
				</div>
			`,
		});
	}

	function injectVideo() {
		if (!marker || markerMode !== 'pip') return;
		const stream = camera.videoStream;
		const container = marker.getElement()?.querySelector('.pip-video-slot') as HTMLElement | null;
		if (!container) return;

		if (!videoEl || !container.contains(videoEl)) {
			videoEl = document.createElement('video');
			videoEl.autoplay = true;
			videoEl.playsInline = true;
			videoEl.muted = true;
			videoEl.style.cssText = 'width:100%;height:100%;object-fit:cover;display:block';
			container.innerHTML = '';
			container.appendChild(videoEl);
		}

		if (stream && videoEl.srcObject !== stream) {
			videoEl.srcObject = stream;
		}
	}

	// Create/update marker position and icon
	$effect(() => {
		if (!gps) return;

		// Track dependencies for non-pip icon rebuilds
		void selected;
		const stream = camera.videoStream;

		if (!marker) {
			marker = leaflet.marker([gps.latitude, gps.longitude], {
				icon: createIcon(),
			}).addTo(map);
			marker.on('click', () => onMarkerClick?.(camera.device_id));
			if (markerMode === 'pip') {
				lastPipKey = pipKey();
				injectVideo();
			}
		} else {
			marker.setLatLng([gps.latitude, gps.longitude]);

			if (markerMode === 'pip') {
				// Only rebuild pip icon when visual properties change, to preserve live video
				const key = pipKey();
				if (key !== lastPipKey) {
					marker.setIcon(createIcon());
					lastPipKey = key;
					videoEl = null;
					injectVideo();
				}
			} else {
				marker.setIcon(createIcon());
				videoEl = null;
			}
		}

		// Inject video when stream arrives or changes
		if (markerMode === 'pip' && stream) {
			injectVideo();
		}
	});

	// Rebuild icon when marker mode changes
	$effect(() => {
		if (!marker) return;
		void markerMode;
		marker.setIcon(createIcon());
		videoEl = null;
		if (markerMode === 'pip') {
			lastPipKey = pipKey();
			injectVideo();
		}
	});

	// Cleanup on unmount
	onMount(() => {
		return () => {
			videoEl = null;
			marker?.remove();
		};
	});
</script>
