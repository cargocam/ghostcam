<script lang="ts">
	import { onMount } from 'svelte';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cameraConfigStore } from '$lib/stores/cameraConfig.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import type { CameraState } from '$lib/stores/cameras.svelte.js';
	import Hls from 'hls.js';
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
	let pipHls: Hls | null = null;
	let pipVideo: HTMLVideoElement | null = null;
	let lastPipKey = '';

	let gps = $derived(gpsOverride ?? camera.telemetry?.gps);
	let displayName = $derived(cameraConfigStore.getDisplayName(camera.device_id, camera.device_name));
	let markerMode = $derived(settingsStore.markerMode);

	function pipKey(): string {
		return `${selected}|${camera.online}|${displayName}`;
	}

	function hlsSrc(): string {
		const base = `/hls/${encodeURIComponent(camera.device_id)}/playlist.m3u8`;
		const target = scrubberStore.seekTarget;
		if (target === null) return base;
		const center = Math.floor(target * 1000);
		return `${base}?from=${center}&to=${center + 2 * 60 * 1000}`;
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

	function destroyPipPlayer() {
		if (pipHls) { pipHls.destroy(); pipHls = null; }
		pipVideo = null;
	}

	function injectVideo() {
		if (!marker) return;
		const el = marker.getElement();
		const slot = el?.querySelector('.pip-video-slot') as HTMLElement | null;
		if (!slot) return;

		// Reuse existing video if already injected
		if (pipVideo && slot.contains(pipVideo)) return;

		destroyPipPlayer();

		const video = document.createElement('video');
		video.autoplay = true;
		video.muted = true;
		video.playsInline = true;
		video.style.cssText = 'width:160px;height:90px;object-fit:cover;display:block';
		slot.innerHTML = '';
		slot.appendChild(video);
		pipVideo = video;

		const src = hlsSrc();
		if (Hls.isSupported()) {
			const instance = new Hls({
				enableWorker: false, // lightweight for pip
				liveSyncDurationCount: 2,
				liveMaxLatencyDurationCount: 4,
			});
			pipHls = instance;
			instance.loadSource(src);
			instance.attachMedia(video);
			instance.on(Hls.Events.MANIFEST_PARSED, () => {
				video.play().catch(() => {});
			});
			instance.on(Hls.Events.ERROR, (_event, data) => {
				if (data.fatal) {
					if (data.type === Hls.ErrorTypes.NETWORK_ERROR) instance.startLoad();
					else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) instance.recoverMediaError();
					else { instance.destroy(); if (pipHls === instance) pipHls = null; }
				}
			});
		} else if (video.canPlayType('application/vnd.apple.mpegurl')) {
			video.src = src;
			video.addEventListener('loadedmetadata', () => video.play().catch(() => {}));
		}
	}

	// Create/update marker position and icon
	$effect(() => {
		if (!gps) return;

		void selected;

		if (!marker) {
			marker = leaflet.marker([gps.latitude, gps.longitude], {
				icon: createIcon(),
			}).addTo(map);
			marker.on('click', () => onMarkerClick?.(camera.device_id));
			if (markerMode === 'pip') {
				lastPipKey = pipKey();
				// Defer video injection until Leaflet renders the DOM
				requestAnimationFrame(() => injectVideo());
			}
		} else {
			marker.setLatLng([gps.latitude, gps.longitude]);

			if (markerMode === 'pip') {
				const key = pipKey();
				if (key !== lastPipKey) {
					marker.setIcon(createIcon());
					lastPipKey = key;
					destroyPipPlayer();
					requestAnimationFrame(() => injectVideo());
				} else {
					// Ensure video is still injected (Leaflet may have recreated DOM)
					requestAnimationFrame(() => injectVideo());
				}
			} else {
				marker.setIcon(createIcon());
				destroyPipPlayer();
			}
		}
	});

	// Rebuild icon when marker mode changes
	$effect(() => {
		if (!marker) return;
		void markerMode;
		marker.setIcon(createIcon());
		destroyPipPlayer();
		if (markerMode === 'pip') {
			lastPipKey = pipKey();
			requestAnimationFrame(() => injectVideo());
		}
	});

	// Cleanup on unmount
	onMount(() => {
		return () => {
			destroyPipPlayer();
			marker?.remove();
		};
	});
</script>
