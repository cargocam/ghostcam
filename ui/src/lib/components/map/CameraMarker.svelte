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
		offsetAngle = 315,
		onMarkerClick,
	}: {
		map: L.Map;
		leaflet: typeof L;
		camera: CameraState;
		gpsOverride?: { latitude: number; longitude: number } | undefined;
		selected?: boolean;
		offsetAngle?: number;
		onMarkerClick?: (deviceId: string) => void;
	} = $props();

	let marker: L.Marker | null = null;
	let pipHls: Hls | null = null;
	let pipVideo: HTMLVideoElement | null = null;
	let lastIconKey = '';

	let gps = $derived(gpsOverride ?? camera.telemetry?.gps);
	let displayName = $derived(cameraConfigStore.getDisplayName(camera.device_id, camera.device_name));
	let markerMode = $derived(settingsStore.markerMode);

	// Panel offset from dot center (px), along offsetAngle
	const PANEL_DISTANCE = 20;
	const PIP_W = 160, PIP_H = 110;
	const INFO_W = 160, INFO_H = 56;
	const DOT_SIZE = 12;

	function iconKey(): string {
		return `${markerMode}|${selected}|${camera.online}|${displayName}|${offsetAngle}`;
	}

	function hlsSrc(): string {
		const base = `/hls/${encodeURIComponent(camera.device_id)}/playlist.m3u8`;
		const target = scrubberStore.seekTarget;
		if (target === null) return base;
		const center = Math.floor(target * 1000);
		return `${base}?from=${center}&to=${center + 2 * 60 * 1000}`;
	}

	function createIcon(): L.DivIcon {
		const online = camera.online;
		const t = camera.telemetry;
		const dotColor = online ? '#22c55e' : '#ef4444';
		const dotRing = selected ? 'box-shadow:0 0 0 3px #22c55e,0 1px 3px rgba(0,0,0,0.3)' : 'box-shadow:0 1px 3px rgba(0,0,0,0.3)';

		const dotHtml = `<div style="position:absolute;width:${DOT_SIZE}px;height:${DOT_SIZE}px;border-radius:50%;background:${dotColor};border:2px solid white;${dotRing};z-index:2"></div>`;

		if (markerMode === 'dot') {
			return leaflet.divIcon({
				className: '',
				iconSize: [DOT_SIZE, DOT_SIZE],
				iconAnchor: [DOT_SIZE / 2, DOT_SIZE / 2],
				html: dotHtml,
			});
		}

		// Compute panel offset position from dot center
		const rad = (offsetAngle * Math.PI) / 180;
		const panelW = markerMode === 'pip' ? PIP_W : INFO_W;
		const panelH = markerMode === 'pip' ? PIP_H : INFO_H;

		// Panel center offset from dot center
		const cx = Math.cos(rad) * (PANEL_DISTANCE + panelW / 2);
		const cy = -Math.sin(rad) * (PANEL_DISTANCE + panelH / 2); // negative because CSS y is down

		// Total icon size must contain both dot and panel
		const pad = 10;
		const totalW = Math.max(panelW, DOT_SIZE) + PANEL_DISTANCE * 2 + panelW + pad * 2;
		const totalH = Math.max(panelH, DOT_SIZE) + PANEL_DISTANCE * 2 + panelH + pad * 2;

		// Dot is at the center of the total icon
		const dotLeft = totalW / 2 - DOT_SIZE / 2;
		const dotTop = totalH / 2 - DOT_SIZE / 2;

		// Panel positioned relative to dot center
		const panelLeft = totalW / 2 + cx - panelW / 2;
		const panelTop = totalH / 2 + cy - panelH / 2;

		// Line from dot edge to panel edge
		const lineX1 = totalW / 2;
		const lineY1 = totalH / 2;
		const lineX2 = panelLeft + panelW / 2;
		const lineY2 = panelTop + panelH / 2;

		const lineHtml = `<svg style="position:absolute;left:0;top:0;width:${totalW}px;height:${totalH}px;pointer-events:none;z-index:0"><line x1="${lineX1}" y1="${lineY1}" x2="${lineX2}" y2="${lineY2}" stroke="rgba(255,255,255,0.3)" stroke-width="1"/></svg>`;

		let panelHtml = '';
		if (markerMode === 'pip') {
			const pipBorder = selected
				? 'border:2px solid #22c55e;box-shadow:0 0 0 2px #22c55e,0 2px 8px rgba(0,0,0,0.4)'
				: 'border:1px solid rgba(255,255,255,0.15);box-shadow:0 2px 8px rgba(0,0,0,0.4)';
			const statusDot = `<span style="width:6px;height:6px;border-radius:50%;background:${dotColor};flex-shrink:0"></span>`;
			panelHtml = `
				<div style="position:absolute;left:${panelLeft}px;top:${panelTop}px;width:${PIP_W}px;z-index:1;border-radius:8px;overflow:hidden;${pipBorder};background:#000">
					<div class="pip-video-slot" style="width:${PIP_W}px;height:90px;background:#1a1a2e"></div>
					<div style="display:flex;align-items:center;gap:5px;padding:3px 8px;background:rgba(0,0,0,0.9);color:white;font-size:10px;font-family:monospace">
						${statusDot}
						<span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${displayName}</span>
					</div>
				</div>`;
		} else {
			// Info mode
			const cpu = t?.cpu_percent?.toFixed(0) ?? '--';
			const mem = t?.memory_mb?.toFixed(0) ?? '--';
			const temp = t?.temp_celsius ? `${t.temp_celsius.toFixed(0)}°` : '';
			const infoBorder = selected
				? 'border:2px solid #22c55e;box-shadow:0 0 0 2px #22c55e,0 2px 8px rgba(0,0,0,0.3)'
				: 'border:1px solid rgba(255,255,255,0.1);box-shadow:0 2px 8px rgba(0,0,0,0.3)';
			panelHtml = `
				<div style="position:absolute;left:${panelLeft}px;top:${panelTop}px;z-index:1;background:rgba(0,0,0,0.85);border-radius:8px;padding:6px 10px;color:white;font-size:11px;font-family:monospace;white-space:nowrap;${infoBorder}">
					<div style="display:flex;align-items:center;gap:6px;margin-bottom:3px">
						<span style="width:6px;height:6px;border-radius:50%;background:${dotColor};flex-shrink:0"></span>
						<span style="font-weight:600;overflow:hidden;text-overflow:ellipsis">${displayName}</span>
					</div>
					<div style="display:flex;gap:8px;color:rgba(255,255,255,0.7);font-size:10px">
						<span>CPU ${cpu}%</span>
						<span>${mem} MB</span>
						${temp ? `<span>${temp}C</span>` : ''}
					</div>
				</div>`;
		}

		const dotAbsHtml = `<div style="position:absolute;left:${dotLeft}px;top:${dotTop}px;width:${DOT_SIZE}px;height:${DOT_SIZE}px;border-radius:50%;background:${dotColor};border:2px solid white;${dotRing};z-index:2"></div>`;

		return leaflet.divIcon({
			className: '',
			iconSize: [totalW, totalH],
			iconAnchor: [totalW / 2, totalH / 2],
			html: `<div style="position:relative;width:${totalW}px;height:${totalH}px;pointer-events:none">${lineHtml}${dotAbsHtml}${panelHtml}</div>`,
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
		if (pipVideo && slot.contains(pipVideo)) return;

		destroyPipPlayer();

		const video = document.createElement('video');
		video.autoplay = true;
		video.muted = true;
		video.playsInline = true;
		video.style.cssText = `width:${PIP_W}px;height:90px;object-fit:cover;display:block`;
		slot.innerHTML = '';
		slot.appendChild(video);
		pipVideo = video;

		const src = hlsSrc();
		if (Hls.isSupported()) {
			const instance = new Hls({
				enableWorker: false,
				liveSyncDurationCount: 2,
				liveMaxLatencyDurationCount: 4,
			});
			pipHls = instance;
			instance.loadSource(src);
			instance.attachMedia(video);
			instance.on(Hls.Events.MANIFEST_PARSED, () => { video.play().catch(() => {}); });
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

	$effect(() => {
		if (!gps) return;
		void selected;
		void offsetAngle;

		const key = iconKey();

		if (!marker) {
			marker = leaflet.marker([gps.latitude, gps.longitude], { icon: createIcon() }).addTo(map);
			marker.on('click', () => onMarkerClick?.(camera.device_id));
			lastIconKey = key;
			if (markerMode === 'pip') requestAnimationFrame(() => injectVideo());
		} else {
			marker.setLatLng([gps.latitude, gps.longitude]);

			if (key !== lastIconKey) {
				const wasPip = lastIconKey.startsWith('pip|');
				marker.setIcon(createIcon());
				lastIconKey = key;
				if (wasPip || markerMode !== 'pip') destroyPipPlayer();
				if (markerMode === 'pip') requestAnimationFrame(() => injectVideo());
			} else if (markerMode === 'pip') {
				requestAnimationFrame(() => injectVideo());
			}
		}
	});

	onMount(() => {
		return () => {
			destroyPipPlayer();
			marker?.remove();
		};
	});
</script>
