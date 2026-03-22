# Plan 11: Viewer — UI Components & Views

## Overview

This plan covers all UI components and views for the Ghostcam viewer SPA: the application shell (header, sidebar, mobile drawer), the three main views (Live, Map, Dashboard), the global timeline scrubber, camera tiles with all states, playback controls, audio focus, alerts sheet, settings sheet, telemetry panel with sparklines, debug overlay, and thumbnail capture. It builds on the stores and WebRTC/signaling layer from Plan 10.

**Depends on**: Plan 10 (stores, WebRTC, signaling, SSE, HLS), Plan 6 (HTTP API)

## 1. Application Shell

### 1.1 Layout

```
┌─────────────────────────────────────────────────────┐
│ Header                                              │
├──────────┬──────────────────────────────────────────┤
│ Sidebar  │ Main content area                        │
│          │ (active view)                            │
│          │                                          │
│          │                                          │
│          │                                          │
├──────────┴──────────────────────────────────────────┤
│ Timeline Scrubber (global, always visible)          │
└─────────────────────────────────────────────────────┘
```

On mobile: sidebar hidden, accessible via hamburger → slide-in drawer.

### 1.2 Header Component

```svelte
<!-- components/Header.svelte -->
<script lang="ts">
  import { settingsStore } from '$stores/settings.svelte';
  import { alertStore } from '$stores/alerts.svelte';
  import { transportStore } from '$stores/transport.svelte';

  let {
    activeView,
    onViewChange,
    liveLayout,       // 'grid' | 'focus'
    onLiveLayoutChange,
    mapMarkerMode,    // 'dot' | 'detailed' | 'pip'
    onMapMarkerModeChange,
  }: {
    activeView: 'live' | 'map' | 'dashboard';
    onViewChange: (view: 'live' | 'map' | 'dashboard') => void;
    liveLayout: 'grid' | 'focus';
    onLiveLayoutChange: (layout: 'grid' | 'focus') => void;
    mapMarkerMode: 'dot' | 'detailed' | 'pip';
    onMapMarkerModeChange: (mode: 'dot' | 'detailed' | 'pip') => void;
  } = $props();
</script>

<header>
  <!-- Branding -->
  <span class="font-bold">Ghostcam</span>

  <!-- View mode switcher -->
  <nav>
    <button onclick={() => onViewChange('live')} class:active={activeView === 'live'}>Live</button>
    <button onclick={() => onViewChange('map')} class:active={activeView === 'map'}>Map</button>
    <button onclick={() => onViewChange('dashboard')} class:active={activeView === 'dashboard'}>Dashboard</button>
  </nav>

  <!-- Context controls (conditional on active view) -->
  {#if activeView === 'live'}
    <div class="context-controls">
      <button onclick={() => onLiveLayoutChange('grid')} class:active={liveLayout === 'grid'}>Grid</button>
      <button onclick={() => onLiveLayoutChange('focus')} class:active={liveLayout === 'focus'}>Focus</button>
    </div>
  {/if}

  {#if activeView === 'map'}
    <div class="context-controls">
      <button onclick={() => onMapMarkerModeChange('dot')} class:active={mapMarkerMode === 'dot'}>Dot</button>
      <button onclick={() => onMapMarkerModeChange('detailed')} class:active={mapMarkerMode === 'detailed'}>Details</button>
      <button onclick={() => onMapMarkerModeChange('pip')} class:active={mapMarkerMode === 'pip'}>PiP</button>
    </div>
  {/if}

  <!-- Connection status -->
  <ConnectionStatus connected={transportStore.sseConnected} />

  <!-- Alerts button -->
  <button onclick={openAlertsSheet}>
    🔔 {#if alertStore.active.length > 0}<span class="badge">{alertStore.active.length}</span>{/if}
  </button>

  <!-- Settings button -->
  <button onclick={openSettingsSheet}>⚙</button>

  <!-- Mobile hamburger -->
  <button class="md:hidden" onclick={openMobileDrawer}>☰</button>
</header>
```

### 1.3 Sidebar Component

Visible on desktop (`md:` breakpoint and above). Contains camera list and telemetry panel.

```svelte
<!-- components/Sidebar.svelte -->
<script lang="ts">
  import { cameraStore } from '$stores/cameras.svelte';
  import TelemetryPanel from './TelemetryPanel.svelte';

  let { selectedCameraId, onSelectCamera }: {
    selectedCameraId: string | null;
    onSelectCamera: (id: string) => void;
  } = $props();
</script>

<aside class="hidden md:flex flex-col w-64 border-r">
  <!-- Camera list -->
  <div class="flex-1 overflow-y-auto">
    {#each cameraStore.list as camera}
      <button
        class="w-full text-left px-3 py-2"
        class:bg-accent={selectedCameraId === camera.device_id}
        onclick={() => onSelectCamera(camera.device_id)}
      >
        <span class="inline-block w-2 h-2 rounded-full mr-2"
          class:bg-green-500={camera.online}
          class:bg-gray-400={!camera.online}
        ></span>
        {camera.device_name}
      </button>
    {/each}
  </div>

  <!-- Telemetry panel for selected camera -->
  <TelemetryPanel cameraId={selectedCameraId} />
</aside>
```

### 1.4 Mobile Drawer

Same content as sidebar, rendered in a bits-ui `Sheet` component that slides in from the left.

## 2. Camera Tile

### 2.1 Tile States

The tile renders one of seven states based on camera and playback status:

| State | Condition | Display |
|-------|-----------|---------|
| Live | Online, client_mode=live, frames arriving | Live WebRTC `<video>` |
| Playback | client_mode=playback, HLS loaded | HLS `<video>` with controls |
| Loading | Playback requested, segment uploading | Spinner overlay |
| No footage | Playback requested, 404 returned | "No footage" + nearest window |
| No signal | Online, client_mode=live, no frames | "No signal" message |
| Storage full | camera sent storage_full alert | Warning overlay, live unaffected |
| Offline | SSE camera_offline received | "Offline" message |

### 2.2 Tile Component

```svelte
<!-- components/CameraTile.svelte -->
<script lang="ts">
  import type { CameraState } from '$stores/cameras.svelte';
  import { settingsStore } from '$stores/settings.svelte';
  import HlsPlayer from './HlsPlayer.svelte';
  import DebugOverlay from './DebugOverlay.svelte';

  let {
    camera,
    tileState,
    hlsManifestUrl,
    isFocused,
    isAudioFocused,
    onAudioFocusToggle,
    onFocus,
    class: className = '',
  }: {
    camera: CameraState;
    tileState: 'live' | 'playback' | 'loading' | 'no-footage' | 'no-signal' | 'storage-full' | 'offline';
    hlsManifestUrl: string | null;
    isFocused: boolean;
    isAudioFocused: boolean;
    onAudioFocusToggle: () => void;
    onFocus: () => void;
    class?: string;
  } = $props();

  let videoEl: HTMLVideoElement | undefined = $state();
  let editingName = $state(false);
  let nameInput = $state(camera.device_name);
</script>

<div
  class={cn("relative rounded-lg overflow-hidden bg-black", className)}
  class:ring-2={isFocused}
  class:ring-primary={isFocused}
  onclick={onFocus}
>
  <!-- Video layer -->
  {#if tileState === 'live' && camera.videoStream}
    <video
      bind:this={videoEl}
      srcObject={camera.videoStream}
      autoplay
      playsinline
      muted={!isAudioFocused}
      class="w-full h-full object-cover"
    />
    {#if camera.audioStream && isAudioFocused}
      <audio srcObject={camera.audioStream} autoplay />
    {/if}
  {:else if tileState === 'playback' && hlsManifestUrl}
    <HlsPlayer src={hlsManifestUrl} class="w-full h-full object-cover" />
  {:else if tileState === 'loading'}
    <div class="flex items-center justify-center w-full h-full">
      <Spinner />
    </div>
  {:else if tileState === 'no-footage'}
    <div class="flex items-center justify-center w-full h-full text-muted-foreground">
      No footage available
    </div>
  {:else if tileState === 'no-signal'}
    <div class="flex items-center justify-center w-full h-full text-muted-foreground">
      No signal
    </div>
  {:else if tileState === 'storage-full'}
    <video bind:this={videoEl} srcObject={camera.videoStream} autoplay playsinline muted class="w-full h-full object-cover" />
    <div class="absolute inset-x-0 top-0 bg-warning/80 text-warning-foreground text-center py-1 text-sm">
      Recording paused — storage full
    </div>
  {:else if tileState === 'offline'}
    <div class="flex items-center justify-center w-full h-full text-muted-foreground">
      Offline
    </div>
  {/if}

  <!-- Overlay -->
  <div class="absolute bottom-0 inset-x-0 p-2 bg-gradient-to-t from-black/60 to-transparent">
    <div class="flex items-center justify-between">
      <!-- Camera name (editable) -->
      {#if editingName}
        <input
          class="bg-transparent text-white text-sm border-b"
          bind:value={nameInput}
          onblur={() => { commitRename(); editingName = false; }}
          onkeydown={(e) => e.key === 'Enter' && (commitRename(), editingName = false)}
        />
      {:else}
        <span
          class="text-white text-sm cursor-pointer"
          ondblclick={() => { editingName = true; }}
        >
          {camera.device_name}
        </span>
      {/if}

      <div class="flex gap-1">
        <!-- Audio focus indicator -->
        {#if isAudioFocused}
          <button onclick|stopPropagation={onAudioFocusToggle} class="text-white">🔊</button>
        {/if}

        <!-- PiP action -->
        <button onclick|stopPropagation={() => videoEl?.requestPictureInPicture()} class="text-white/60 hover:text-white">PiP</button>

        <!-- Snapshot action -->
        <button onclick|stopPropagation={() => captureSnapshot(videoEl, camera.device_name)} class="text-white/60 hover:text-white">📷</button>
      </div>
    </div>
  </div>

  <!-- Debug overlay -->
  {#if settingsStore.debugOverlay}
    <DebugOverlay cameraId={camera.device_id} />
  {/if}
</div>
```

### 2.3 Camera Renaming

Inline edit via double-click on name. Optimistic update with rollback:

```typescript
async function commitRename() {
  const oldName = camera.device_name;
  cameraStore.setName(camera.device_id, nameInput);

  try {
    await fetch(`/api/v1/cameras/${camera.device_id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ display_name: nameInput }),
      credentials: 'include',
    });
  } catch {
    // Rollback
    cameraStore.setName(camera.device_id, oldName);
    alertStore.add('error', `Rename failed for ${oldName}`);
  }
}
```

### 2.4 Snapshot Capture

```typescript
function captureSnapshot(video: HTMLVideoElement | undefined, name: string) {
  if (!video) return;
  const canvas = document.createElement('canvas');
  canvas.width = video.videoWidth;
  canvas.height = video.videoHeight;
  canvas.getContext('2d')!.drawImage(video, 0, 0);
  const url = canvas.toDataURL('image/png');
  const a = document.createElement('a');
  a.href = url;
  a.download = `${name}-${Date.now()}.png`;
  a.click();
}
```

## 3. Live View

### 3.1 Auto-fit Grid Layout

All cameras displayed in a uniform responsive grid. Grid columns computed from camera count and viewport:

```svelte
<!-- views/LiveView.svelte -->
<script lang="ts">
  import { cameraStore } from '$stores/cameras.svelte';
  import CameraTile from '$components/CameraTile.svelte';

  let { layout, focusedCameraId, onFocusCamera }: {
    layout: 'grid' | 'focus';
    focusedCameraId: string | null;
    onFocusCamera: (id: string) => void;
  } = $props();

  let containerEl: HTMLDivElement | undefined = $state();

  // Auto-fit: compute columns from camera count
  let columns = $derived(() => {
    const count = cameraStore.onlineList.length;
    if (count <= 1) return 1;
    if (count <= 4) return 2;
    if (count <= 9) return 3;
    return 4;
  });
</script>

{#if layout === 'grid'}
  <div
    bind:this={containerEl}
    class="grid gap-2 p-2 h-full"
    style="grid-template-columns: repeat({columns()}, 1fr);"
  >
    {#each cameraStore.onlineList as camera (camera.device_id)}
      <CameraTile
        {camera}
        tileState={getTileState(camera)}
        hlsManifestUrl={getManifestUrl(camera.device_id)}
        isFocused={false}
        isAudioFocused={settingsStore.unmutedCameraId === camera.device_id}
        onAudioFocusToggle={() => settingsStore.toggleMute(camera.device_id)}
        onFocus={() => settingsStore.toggleMute(camera.device_id)}
      />
    {/each}
  </div>
{:else}
  <!-- Focus layout -->
  <FocusLayout
    cameras={cameraStore.onlineList}
    focusedCameraId={focusedCameraId}
    {onFocusCamera}
  />
{/if}
```

### 3.2 Focus Layout

One camera enlarged, others smaller:

```svelte
<!-- components/FocusLayout.svelte -->
<script lang="ts">
  let { cameras, focusedCameraId, onFocusCamera } = $props();

  let focused = $derived(cameras.find(c => c.device_id === focusedCameraId) ?? cameras[0]);
  let others = $derived(cameras.filter(c => c.device_id !== focused?.device_id));
</script>

<div class="flex h-full gap-2 p-2">
  <!-- Focused camera: takes 2/3 width -->
  {#if focused}
    <div class="flex-[2]">
      <CameraTile
        camera={focused}
        tileState={getTileState(focused)}
        hlsManifestUrl={getManifestUrl(focused.device_id)}
        isFocused={true}
        isAudioFocused={settingsStore.unmutedCameraId === focused.device_id}
        onAudioFocusToggle={() => settingsStore.toggleMute(focused.device_id)}
        onFocus={() => {}}
        class="h-full"
      />
    </div>
  {/if}

  <!-- Other cameras: 1/3 width, stacked -->
  <div class="flex-1 flex flex-col gap-2 overflow-y-auto">
    {#each others as camera (camera.device_id)}
      <CameraTile
        {camera}
        tileState={getTileState(camera)}
        hlsManifestUrl={null}
        isFocused={false}
        isAudioFocused={settingsStore.unmutedCameraId === camera.device_id}
        onAudioFocusToggle={() => settingsStore.toggleMute(camera.device_id)}
        onFocus={() => onFocusCamera(camera.device_id)}
        class="aspect-video"
      />
    {/each}
  </div>
</div>
```

Clicking a non-focused tile:
1. Previously focused camera returns to live mode if it was in playback
2. Newly focused camera becomes the scrubber target
3. All non-focused cameras remain live

## 4. Map View

### 4.1 Leaflet Integration

```svelte
<!-- views/MapView.svelte -->
<script lang="ts">
  import L from 'leaflet';
  import { cameraStore } from '$stores/cameras.svelte';
  import { settingsStore } from '$stores/settings.svelte';
  import { thumbnailStore } from '$stores/thumbnails.svelte';
  import { scrubberStore } from '$stores/scrubber.svelte';

  let { markerMode }: { markerMode: 'dot' | 'detailed' | 'pip' } = $props();

  let mapEl: HTMLDivElement | undefined = $state();
  let map: L.Map | undefined = $state();
  let markers = new Map<string, L.Marker | L.CircleMarker>();
  let historicalMarkers = new Map<string, L.CircleMarker>();

  // Tracking state
  let tracking: 'all' | 'single' | 'manual' = $state('all');
  let trackedCameraId: string | null = $state(null);

  // Tile layers
  const tileLayers = {
    'dark': L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
      attribution: '© CARTO',
    }),
    'light': L.tileLayer('https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png', {
      attribution: '© CARTO',
    }),
    'satellite': L.tileLayer('https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}', {
      attribution: '© Esri',
    }),
  };

  let activeThemeLayer = $derived(() => {
    if (settingsStore.mapLayer === 'satellite') return 'satellite';
    const theme = settingsStore.resolvedTheme; // 'dark' or 'light'
    return theme;
  });

  // Initialize map
  $effect(() => {
    if (!mapEl || map) return;

    map = L.map(mapEl, {
      center: [0, 0],
      zoom: 2,
    });

    tileLayers[activeThemeLayer()].addTo(map);

    // Disengage tracking on user interaction
    map.on('dragstart', () => { tracking = 'manual'; });
    map.on('zoomstart', () => {
      // Only disengage if user-initiated (not programmatic)
      if (!programmaticMove) tracking = 'manual';
    });

    return () => { map?.remove(); map = undefined; };
  });

  // Switch tile layer on theme change
  $effect(() => {
    if (!map) return;
    const layer = activeThemeLayer();
    for (const [key, tl] of Object.entries(tileLayers)) {
      if (key === layer) tl.addTo(map);
      else map.removeLayer(tl);
    }
  });

  // Update markers when camera telemetry changes
  $effect(() => {
    if (!map) return;

    const isPlayback = scrubberStore.mode === 'playback';

    for (const camera of cameraStore.list) {
      const gps = camera.telemetry?.gps;
      if (!gps) {
        // Remove marker for cameras without GPS
        removeMarker(camera.device_id);
        continue;
      }

      updateOrCreateMarker(camera, gps, markerMode, isPlayback);
    }

    // Update historical markers if in playback mode
    if (isPlayback) {
      updateHistoricalMarkers();
    } else {
      clearHistoricalMarkers();
    }

    // Auto-fit
    if (tracking === 'all') fitAllMarkers();
    else if (tracking === 'single' && trackedCameraId) trackSingle(trackedCameraId);
  });
</script>

<div class="relative h-full">
  <div bind:this={mapEl} class="h-full w-full" />

  <!-- Track all button -->
  {#if tracking !== 'all'}
    <button
      class="absolute top-4 right-4 z-[1000] bg-background border rounded-lg px-3 py-1.5 shadow"
      onclick={() => { tracking = 'all'; fitAllMarkers(); }}
    >
      Track all
    </button>
  {/if}

  <!-- Map layer toggle -->
  <div class="absolute bottom-4 right-4 z-[1000] flex gap-1 bg-background border rounded-lg p-1 shadow">
    <button class:active={settingsStore.mapLayer !== 'satellite'} onclick={() => settingsStore.mapLayer = 'auto'}>Map</button>
    <button class:active={settingsStore.mapLayer === 'satellite'} onclick={() => settingsStore.mapLayer = 'satellite'}>Satellite</button>
  </div>
</div>
```

### 4.2 Marker Modes

**Dot mode**: `L.circleMarker` with 6px radius, green (online) or gray (offline).

**Detailed mode**: `L.marker` with a custom `L.divIcon` containing camera name, status, and key telemetry values (CPU, temp):

```typescript
function createDetailedIcon(camera: CameraState): L.DivIcon {
  return L.divIcon({
    className: 'detailed-marker',
    html: `
      <div class="bg-background border rounded-lg px-2 py-1 shadow text-xs whitespace-nowrap">
        <div class="font-medium">${camera.device_name}</div>
        <div class="text-muted-foreground">
          ${camera.online ? '● Online' : '○ Offline'}
          ${camera.telemetry?.cpu_percent != null ? ` · CPU ${camera.telemetry.cpu_percent.toFixed(0)}%` : ''}
          ${camera.telemetry?.temp_celsius != null ? ` · ${camera.telemetry.temp_celsius.toFixed(0)}°C` : ''}
        </div>
      </div>
    `,
  });
}
```

**PiP mode**: `L.marker` with a custom `L.divIcon` containing a live video thumbnail:

```typescript
function createPipIcon(camera: CameraState): L.DivIcon {
  const thumbnail = thumbnailStore.get(camera.device_id);
  return L.divIcon({
    className: 'pip-marker',
    html: `
      <div class="rounded-lg overflow-hidden shadow border-2 ${camera.online ? 'border-green-500' : 'border-gray-400'}">
        ${thumbnail
          ? `<img src="${thumbnail}" class="w-32 h-24 object-cover" />`
          : `<div class="w-32 h-24 bg-muted flex items-center justify-center text-xs">No preview</div>`
        }
        <div class="bg-background px-1.5 py-0.5 text-xs text-center truncate">${camera.device_name}</div>
      </div>
    `,
    iconSize: [128, 112],
    iconAnchor: [64, 112],
  });
}
```

### 4.3 Marker Click

Clicking a marker enters single-camera tracking and selects that camera in the sidebar:

```typescript
marker.on('click', () => {
  tracking = 'single';
  trackedCameraId = camera.device_id;
  onSelectCamera(camera.device_id);
});
```

### 4.4 Tracking

```typescript
let programmaticMove = false;

function fitAllMarkers() {
  if (!map || markers.size === 0) return;
  const bounds = L.latLngBounds([...markers.values()].map(m => m.getLatLng()));
  programmaticMove = true;
  map.fitBounds(bounds, { padding: [50, 50] });
  setTimeout(() => { programmaticMove = false; }, 300);
}

function trackSingle(deviceId: string) {
  const marker = markers.get(deviceId);
  if (!map || !marker) return;
  programmaticMove = true;
  map.panTo(marker.getLatLng());
  setTimeout(() => { programmaticMove = false; }, 300);
}
```

### 4.5 Scrubber Integration — Historical Position Markers

When the scrubber is in playback mode, the map shows two sets of markers:

1. **Current position markers** — the camera's current live GPS position, rendered as a **faded dot** (opacity 0.3, always dot style regardless of active marker mode). These show where the camera is right now.
2. **Historical position markers** — the camera's GPS position at the scrubber's playhead time, rendered at **full opacity** using the active marker mode (dot/detailed/PiP). These show where the camera was at that point in time.

Historical positions are sourced from the telemetry time series (Plan 5 Redis telemetry range query). When the scrubber moves, the viewer fetches GPS coordinates for the playhead timestamp:

```typescript
async function fetchHistoricalPositions(timestamp: number): Promise<Map<string, GpsData>> {
  const positions = new Map<string, GpsData>();

  for (const camera of cameraStore.onlineList) {
    try {
      const res = await fetch(
        `/api/v1/cameras/${camera.device_id}/telemetry?at=${timestamp}`,
        { credentials: 'include' },
      );
      if (res.ok) {
        const data = await res.json();
        if (data.gps) {
          positions.set(camera.device_id, data.gps);
        }
      }
    } catch {
      // Skip cameras with no telemetry at this time
    }
  }

  return positions;
}
```

Rendering logic:

```typescript
function updateHistoricalMarkers() {
  // Replace current markers with faded dots (regardless of active marker mode)
  for (const [id, marker] of markers) {
    const latlng = marker.getLatLng();
    map!.removeLayer(marker);

    const fadedDot = L.circleMarker(latlng, {
      radius: 6,
      color: '#9ca3af',    // gray-400
      fillColor: '#9ca3af',
      fillOpacity: 0.15,
      opacity: 0.3,
    }).addTo(map!);

    markers.set(id, fadedDot);
  }

  // Create/update historical markers at full opacity
  for (const [deviceId, gps] of historicalPositions) {
    const existing = historicalMarkers.get(deviceId);
    const latlng = L.latLng(gps.lat, gps.lon);

    if (existing) {
      existing.setLatLng(latlng);
    } else {
      const marker = L.circleMarker(latlng, {
        radius: 8,
        color: '#3b82f6', // Blue to distinguish from current
        fillColor: '#3b82f6',
        fillOpacity: 0.8,
        opacity: 1.0,
      }).addTo(map!);

      // Tooltip with camera name + timestamp
      const camera = cameraStore.getCamera(deviceId);
      if (camera) {
        marker.bindTooltip(camera.device_name, { permanent: markerMode === 'detailed' });
      }

      historicalMarkers.set(deviceId, marker);
    }
  }

  // Remove historical markers for cameras no longer in the set
  for (const [id, marker] of historicalMarkers) {
    if (!historicalPositions.has(id)) {
      map!.removeLayer(marker);
      historicalMarkers.delete(id);
    }
  }
}

function clearHistoricalMarkers() {
  for (const [, marker] of historicalMarkers) {
    map!.removeLayer(marker);
  }
  historicalMarkers.clear();

  // Restore current markers to full opacity
  for (const [, marker] of markers) {
    if (marker instanceof L.CircleMarker) {
      marker.setStyle({ opacity: 1.0, fillOpacity: 0.6 });
    } else {
      marker.setOpacity(1.0);
    }
  }
}
```

When the scrubber returns to live (Go Live), historical markers are cleared and current markers return to full opacity. The historical position fetch is debounced (200ms) during scrubbing to avoid excessive API calls.

### 4.6 Tile Layers

Three tile layer options:

| Layer | Source | When |
|-------|--------|------|
| Light | CARTO light_all | Theme is light, map layer is "auto" |
| Dark | CARTO dark_all | Theme is dark, map layer is "auto" |
| Satellite | Esri World Imagery | Map layer toggle set to "satellite" |

A toggle in the bottom-right corner of the map switches between "Map" (auto light/dark) and "Satellite". The active setting is persisted to localStorage via `settingsStore.mapLayer`.

## 5. Dashboard View

### 5.1 Stat Cards

Four summary cards at the top:

```svelte
<!-- views/DashboardView.svelte -->
<script lang="ts">
  import { cameraStore } from '$stores/cameras.svelte';
  import { transportStore } from '$stores/transport.svelte';
  import { videoStatsStore } from '$stores/videoStats.svelte';
  import Sparkline from '$components/Sparkline.svelte';
</script>

<div class="p-4 space-y-4">
  <!-- Stat cards -->
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-4">
    <StatCard title="Cameras Online">
      <span class="text-2xl font-bold">{cameraStore.onlineList.length}</span>
      <span class="text-muted-foreground">/ {cameraStore.list.length}</span>
    </StatCard>

    <StatCard title="Total Bandwidth">
      <span class="text-2xl font-bold">{formatBitrate(videoStatsStore.totalBitrate)}</span>
      <Sparkline data={videoStatsStore.bitrateSamples} class="h-8 w-24" />
    </StatCard>

    <StatCard title="Frames Decoded">
      <span class="text-2xl font-bold">{videoStatsStore.totalFramesDecoded}</span>
      <span class="text-muted-foreground">dropped: {videoStatsStore.totalFramesDropped}</span>
    </StatCard>

    <StatCard title="Connection Uptime">
      <span class="text-2xl font-bold">{formatUptime(transportStore.sessionUptimeMs)}</span>
      <span class="text-muted-foreground">{transportStore.sseConnected ? 'Connected' : 'Disconnected'}</span>
    </StatCard>
  </div>

  <!-- Status cards -->
  <div class="grid grid-cols-3 gap-4">
    <StatusCard title="Connection" status={transportStore.sseConnected ? 'ok' : 'error'}>
      {transportStore.sseConnected ? 'Connected' : transportStore.lastError ?? 'Disconnected'}
    </StatusCard>

    <StatusCard title="Stream Health" status={getHealthStatus(videoStatsStore.dropRate)}>
      {videoStatsStore.dropRate.toFixed(1)}% drop rate —
      {getHealthLabel(videoStatsStore.dropRate)}
    </StatusCard>

    <StatusCard title="Alerts" status={alertStore.active.length > 0 ? 'warning' : 'ok'}>
      {alertStore.active.length} unread / {alertStore.alerts.length} total
    </StatusCard>
  </div>

  <!-- Per-camera table -->
  <CameraTable />
</div>
```

### 5.2 Health Status Thresholds

```typescript
function getHealthStatus(dropRate: number): 'ok' | 'warning' | 'error' {
  if (dropRate < 1) return 'ok';
  if (dropRate < 5) return 'warning';
  return 'error';
}

function getHealthLabel(dropRate: number): string {
  if (dropRate < 1) return 'Excellent';
  if (dropRate < 5) return 'Good';
  return 'Degraded';
}
```

### 5.3 Per-Camera Table

```svelte
<!-- components/CameraTable.svelte -->
<table class="w-full text-sm">
  <thead>
    <tr class="border-b text-left text-muted-foreground">
      <th class="py-2">Camera</th>
      <th>Status</th>
      <th>Resolution</th>
      <th>Codec</th>
      <th>Bitrate</th>
      <th>Dropped</th>
      <th>CPU</th>
      <th>Memory</th>
      <th>Temp</th>
    </tr>
  </thead>
  <tbody>
    {#each cameraStore.list as camera (camera.device_id)}
      {@const stats = videoStatsStore.getStats(camera.device_id)}
      {@const tel = camera.telemetry}
      <tr class="border-b">
        <td class="py-2 font-medium">{camera.device_name}</td>
        <td>
          <span class="inline-block w-2 h-2 rounded-full mr-1"
            class:bg-green-500={camera.online}
            class:bg-gray-400={!camera.online}
          ></span>
          {camera.online ? 'Online' : 'Offline'}
        </td>
        <td>{stats?.resolution ?? '—'}</td>
        <td>{stats?.codec ?? '—'}</td>
        <td>{stats ? formatBitrate(stats.bitrate) : '—'}</td>
        <td>{stats?.framesDropped ?? '—'}</td>
        <td>{tel?.cpu_percent != null ? `${tel.cpu_percent.toFixed(0)}%` : '—'}</td>
        <td>{tel?.memory_mb != null ? `${tel.memory_mb.toFixed(0)} MB` : '—'}</td>
        <td>{tel?.temp_celsius != null ? `${tel.temp_celsius.toFixed(0)}°C` : '—'}</td>
      </tr>
    {/each}
  </tbody>
</table>
```

## 6. Timeline Scrubber

### 6.1 Structure

The scrubber is a global component rendered at the bottom of every view. It displays:

- **Available window** — the time range for which footage exists
- **Playhead** — current position, advances in real time when live
- **Go Live button** — appears in playback mode
- **Timestamp label** — current playhead time
- **Per-camera coverage indicators** — in grid mode, thin colored bars showing which cameras have footage at the playhead position

### 6.2 Scrubber Store

```typescript
// stores/scrubber.svelte.ts

type ScrubberMode = 'live' | 'playback';

function createScrubberStore() {
  let mode = $state<ScrubberMode>('live');
  let playheadTime = $state<number>(Date.now() / 1000); // Unix seconds
  let availableWindow = $state<{ start: number; end: number } | null>(null);
  let cameraCoverage = $state<Map<string, { start: number; end: number }[]>>(new Map());

  let animationFrame: number | null = null;

  function startLiveTick() {
    const tick = () => {
      if (mode === 'live') {
        playheadTime = Date.now() / 1000;
        animationFrame = requestAnimationFrame(tick);
      }
    };
    animationFrame = requestAnimationFrame(tick);
  }

  function stopLiveTick() {
    if (animationFrame) {
      cancelAnimationFrame(animationFrame);
      animationFrame = null;
    }
  }

  return {
    get mode() { return mode; },
    get playheadTime() { return playheadTime; },
    get availableWindow() { return availableWindow; },
    get cameraCoverage() { return cameraCoverage; },

    setAvailableWindow(window: { start: number; end: number }) {
      availableWindow = window;
    },

    setCameraCoverage(deviceId: string, segments: { start: number; end: number }[]) {
      cameraCoverage.set(deviceId, segments);
    },

    scrubTo(time: number) {
      mode = 'playback';
      playheadTime = time;
      stopLiveTick();
    },

    goLive() {
      mode = 'live';
      playheadTime = Date.now() / 1000;
      startLiveTick();
    },

    initialize() {
      startLiveTick();
    },

    destroy() {
      stopLiveTick();
    },
  };
}

export const scrubberStore = createScrubberStore();
```

### 6.3 Scrubber Component

```svelte
<!-- components/TimelineScrubber.svelte -->
<script lang="ts">
  import { scrubberStore } from '$stores/scrubber.svelte';
  import { cameraStore } from '$stores/cameras.svelte';

  let { layout }: { layout: 'grid' | 'focus' } = $props();

  let scrubberEl: HTMLDivElement | undefined = $state();
  let isDragging = $state(false);

  function handlePointerDown(e: PointerEvent) {
    isDragging = true;
    scrubberEl?.setPointerCapture(e.pointerId);
    scrubToPointer(e);
  }

  function handlePointerMove(e: PointerEvent) {
    if (!isDragging) return;
    scrubToPointer(e);
  }

  function handlePointerUp() {
    isDragging = false;
  }

  function scrubToPointer(e: PointerEvent) {
    if (!scrubberEl || !scrubberStore.availableWindow) return;
    const rect = scrubberEl.getBoundingClientRect();
    const pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    const { start, end } = scrubberStore.availableWindow;
    const time = start + pct * (end - start);
    scrubberStore.scrubTo(time);
  }

  // Playhead position as percentage
  let playheadPct = $derived(() => {
    if (!scrubberStore.availableWindow) return 100;
    const { start, end } = scrubberStore.availableWindow;
    const range = end - start;
    if (range <= 0) return 100;
    return ((scrubberStore.playheadTime - start) / range) * 100;
  });
</script>

<div class="border-t bg-background px-4 py-2">
  <div class="flex items-center gap-3">
    <!-- Timestamp -->
    <span class="text-xs text-muted-foreground w-20 tabular-nums">
      {formatTimestamp(scrubberStore.playheadTime)}
    </span>

    <!-- Scrubber track -->
    <div
      bind:this={scrubberEl}
      class="relative flex-1 h-8 cursor-pointer"
      onpointerdown={handlePointerDown}
      onpointermove={handlePointerMove}
      onpointerup={handlePointerUp}
    >
      <!-- Available window background -->
      <div class="absolute inset-0 rounded bg-muted" />

      <!-- Per-camera coverage indicators (grid mode only) -->
      {#if layout === 'grid'}
        {@const cameras = [...scrubberStore.cameraCoverage.entries()]}
        {#each cameras as [deviceId, segments], i}
          {#each segments as segment}
            <div
              class="absolute h-1 rounded-full"
              style="
                left: {pctForTime(segment.start)}%;
                width: {pctForTime(segment.end) - pctForTime(segment.start)}%;
                top: {4 + i * 3}px;
                background: {colorForCamera(deviceId)};
              "
            />
          {/each}
        {/each}
      {/if}

      <!-- Playhead -->
      <div
        class="absolute top-0 bottom-0 w-0.5 bg-primary"
        style="left: {playheadPct()}%;"
      >
        <div class="absolute -top-1 -left-1.5 w-3 h-3 rounded-full bg-primary" />
      </div>
    </div>

    <!-- Go Live button -->
    {#if scrubberStore.mode === 'playback'}
      <button
        class="text-xs font-medium text-primary hover:underline"
        onclick={() => scrubberStore.goLive()}
      >
        Go Live
      </button>
    {/if}
  </div>
</div>
```

### 6.4 Scrubber Behaviour by View

| View | Layout | Scrubber controls | Behaviour |
|------|--------|-------------------|-----------|
| Live | Grid | All cameras | All cameras enter playback; coverage indicators shown |
| Live | Focus | Focused camera only | Only focused camera enters playback; others stay live |
| Map | — | All cameras | Historical GPS markers shown at playhead time; current markers faded |
| Dashboard | — | All cameras | No visual change in dashboard; scrubbing updates the timestamp context |

### 6.5 Playback Transition

When the scrubber moves from live to playback:

```typescript
// In connection manager or transport store
function enterPlayback(cameraIds: string[], timestamp: number) {
  for (const id of cameraIds) {
    // Send client_mode: playback on the commands data channel
    const conn = connections.get(id);
    conn?.sendCommand({ type: 'client_mode', mode: 'playback' });

    // Fetch HLS manifest and seek
    fetchManifestAndSeek(id, timestamp);
  }
}

function returnToLive(cameraIds: string[]) {
  for (const id of cameraIds) {
    const conn = connections.get(id);
    conn?.sendCommand({ type: 'client_mode', mode: 'live' });

    // Destroy HLS player, show live WebRTC stream
    destroyHlsPlayer(id);
  }
}
```

## 7. Audio Focus

### 7.1 Model

One camera at a time has active audio. All muted by default on session start. Clicking a camera tile toggles its audio focus:

- Click unfocused tile → that tile gets audio, previously focused tile muted
- Click already-focused tile → muted, no tile has audio

Audio focus and layout focus (in focus layout) are independent.

### 7.2 Visual Indicator

The audio-focused tile displays a speaker icon in the overlay and a subtle highlighted border or glow to make the state unambiguous.

## 8. Supporting Components

### 8.1 Sparkline

Reusable SVG filled area chart:

```svelte
<!-- components/Sparkline.svelte -->
<script lang="ts">
  let { data, class: className = '' }: { data: number[]; class?: string } = $props();

  let path = $derived(() => {
    if (data.length < 2) return '';
    const max = Math.max(...data, 1);
    const w = 100;
    const h = 100;
    const step = w / (data.length - 1);

    const points = data.map((v, i) => `${i * step},${h - (v / max) * h}`);
    const line = `M${points.join(' L')}`;
    const fill = `${line} L${w},${h} L0,${h} Z`;
    return fill;
  });
</script>

<svg viewBox="0 0 100 100" preserveAspectRatio="none" class={cn("fill-primary/20 stroke-primary stroke-[1.5]", className)}>
  <path d={path()} />
</svg>
```

Used in:
- Sidebar telemetry panel (CPU, memory sparklines — 60 samples)
- Dashboard bandwidth stat card

### 8.2 Telemetry Panel

```svelte
<!-- components/TelemetryPanel.svelte -->
<script lang="ts">
  import { cameraStore } from '$stores/cameras.svelte';
  import Sparkline from './Sparkline.svelte';

  let { cameraId }: { cameraId: string | null } = $props();

  let camera = $derived(cameraId ? cameraStore.getCamera(cameraId) : null);

  // Ring buffers for sparkline data (60 samples at 2s interval = 2 min window)
  let cpuSamples: number[] = $state([]);
  let memorySamples: number[] = $state([]);

  $effect(() => {
    if (!camera?.telemetry) return;

    if (camera.telemetry.cpu_percent != null) {
      cpuSamples = [...cpuSamples.slice(-59), camera.telemetry.cpu_percent];
    }
    if (camera.telemetry.memory_mb != null) {
      memorySamples = [...memorySamples.slice(-59), camera.telemetry.memory_mb];
    }
  });
</script>

<div class="p-3 border-t space-y-3">
  {#if camera}
    <div>
      <div class="text-xs text-muted-foreground">CPU</div>
      <div class="flex items-center gap-2">
        <span class="text-sm font-medium">
          {camera.telemetry?.cpu_percent?.toFixed(0) ?? '—'}%
        </span>
        <Sparkline data={cpuSamples} class="h-6 flex-1" />
      </div>
    </div>

    <div>
      <div class="text-xs text-muted-foreground">Memory</div>
      <div class="flex items-center gap-2">
        <span class="text-sm font-medium">
          {camera.telemetry?.memory_mb?.toFixed(0) ?? '—'} MB
        </span>
        <Sparkline data={memorySamples} class="h-6 flex-1" />
      </div>
    </div>

    <div>
      <div class="text-xs text-muted-foreground">Temperature</div>
      <span class="text-sm font-medium"
        class:text-warning={camera.telemetry?.temp_celsius != null && camera.telemetry.temp_celsius > 70}
        class:text-destructive={camera.telemetry?.temp_celsius != null && camera.telemetry.temp_celsius > 80}
      >
        {camera.telemetry?.temp_celsius?.toFixed(0) ?? '—'}°C
      </span>
    </div>

    <div>
      <div class="text-xs text-muted-foreground">Uptime</div>
      <span class="text-sm font-medium">
        {camera.telemetry?.uptime_secs != null ? formatUptime(camera.telemetry.uptime_secs * 1000) : '—'}
      </span>
    </div>
  {:else}
    <div class="text-sm text-muted-foreground">
      Select a camera to view telemetry
    </div>
  {/if}
</div>
```

### 8.3 Debug Overlay

```svelte
<!-- components/DebugOverlay.svelte -->
<script lang="ts">
  import { videoStatsStore } from '$stores/videoStats.svelte';

  let { cameraId }: { cameraId: string } = $props();
  let stats = $derived(videoStatsStore.getStats(cameraId));
</script>

{#if stats}
  <div class="absolute top-2 left-2 bg-black/70 text-white text-xs font-mono px-2 py-1 rounded space-y-0.5">
    <div>{formatBitrate(stats.bitrate)}</div>
    <div>{stats.frameRate} fps</div>
    <div>{stats.resolution}</div>
    <div>{stats.codec}</div>
    <div>loss: {stats.packetLoss.toFixed(2)}%</div>
  </div>
{/if}
```

### 8.4 Alerts Sheet

```svelte
<!-- components/AlertsSheet.svelte -->
<script lang="ts">
  import { alertStore } from '$stores/alerts.svelte';
  import { Sheet } from 'bits-ui';
</script>

<Sheet.Root>
  <Sheet.Content side="right" class="w-80">
    <Sheet.Header>
      <Sheet.Title>Alerts</Sheet.Title>
    </Sheet.Header>

    <div class="flex-1 overflow-y-auto">
      {#each alertStore.alerts.toReversed() as alert (alert.id)}
        <div class="px-4 py-3 border-b" class:opacity-50={alert.dismissed}>
          <div class="flex items-center gap-2">
            <span class="text-xs px-1.5 py-0.5 rounded"
              class:bg-destructive/10={alert.type === 'error'}
              class:bg-warning/10={alert.type === 'warning'}
              class:bg-muted={alert.type === 'info'}
            >
              {alert.type}
            </span>
            <span class="text-xs text-muted-foreground">
              {formatTime(alert.timestamp)}
            </span>
          </div>
          <p class="text-sm mt-1">{alert.message}</p>
          {#if !alert.dismissed}
            <button class="text-xs text-muted-foreground mt-1" onclick={() => alertStore.dismiss(alert.id)}>
              Dismiss
            </button>
          {/if}
        </div>
      {/each}

      {#if alertStore.alerts.length === 0}
        <div class="p-4 text-sm text-muted-foreground">No alerts</div>
      {/if}
    </div>
  </Sheet.Content>
</Sheet.Root>
```

### 8.5 Settings Sheet

```svelte
<!-- components/SettingsSheet.svelte -->
<script lang="ts">
  import { settingsStore } from '$stores/settings.svelte';
  import { transportStore } from '$stores/transport.svelte';
  import { Sheet } from 'bits-ui';
</script>

<Sheet.Root>
  <Sheet.Content side="right" class="w-80">
    <Sheet.Header>
      <Sheet.Title>Settings</Sheet.Title>
    </Sheet.Header>

    <div class="p-4 space-y-6">
      <!-- Theme -->
      <div>
        <label class="text-sm font-medium">Theme</label>
        <div class="flex gap-2 mt-1">
          {#each ['light', 'dark', 'system'] as t}
            <button
              class="px-3 py-1.5 rounded text-sm border"
              class:bg-primary={settingsStore.theme === t}
              class:text-primary-foreground={settingsStore.theme === t}
              onclick={() => settingsStore.theme = t}
            >
              {t}
            </button>
          {/each}
        </div>
      </div>

      <!-- Debug overlay -->
      <div class="flex items-center justify-between">
        <label class="text-sm font-medium">Debug overlay</label>
        <Switch checked={settingsStore.debugOverlay} onCheckedChange={(v) => settingsStore.debugOverlay = v} />
      </div>

      <!-- Connection status (read-only) -->
      <div>
        <label class="text-sm font-medium">Connection</label>
        <p class="text-sm text-muted-foreground mt-1">
          {transportStore.sseConnected ? 'Connected' : 'Disconnected'}
          {#if transportStore.lastError}
            — {transportStore.lastError}
          {/if}
        </p>
      </div>
    </div>
  </Sheet.Content>
</Sheet.Root>
```

### 8.6 Thumbnail Store

```typescript
// stores/thumbnails.svelte.ts

function createThumbnailStore() {
  let thumbnails = $state<Map<string, string>>(new Map());

  return {
    get(deviceId: string): string | null {
      return thumbnails.get(deviceId) ?? null;
    },

    capture(deviceId: string, videoEl: HTMLVideoElement) {
      if (videoEl.readyState < 2) return; // Not enough data
      const canvas = document.createElement('canvas');
      canvas.width = 160; // Small thumbnail
      canvas.height = 120;
      canvas.getContext('2d')!.drawImage(videoEl, 0, 0, 160, 120);
      thumbnails.set(deviceId, canvas.toDataURL('image/jpeg', 0.6));
    },

    clear() {
      thumbnails = new Map();
    },
  };
}

export const thumbnailStore = createThumbnailStore();
```

Thumbnails are captured periodically (every 2s) from live `<video>` elements via a `$effect` in the CameraTile component. Used by map PiP markers.

### 8.7 Video Stats Store

```typescript
// stores/videoStats.svelte.ts

interface CameraStats {
  bitrate: number;       // kbps
  frameRate: number;
  resolution: string;    // "640x480"
  codec: string;
  packetLoss: number;    // percentage
  framesDecoded: number;
  framesDropped: number;
}

function createVideoStatsStore() {
  let stats = $state<Map<string, CameraStats>>(new Map());
  let bitrateSamples = $state<number[]>([]);

  return {
    getStats(deviceId: string): CameraStats | undefined {
      return stats.get(deviceId);
    },

    get totalBitrate(): number {
      let sum = 0;
      for (const s of stats.values()) sum += s.bitrate;
      return sum;
    },

    get bitrateSamples() { return bitrateSamples; },

    get totalFramesDecoded(): number {
      let sum = 0;
      for (const s of stats.values()) sum += s.framesDecoded;
      return sum;
    },

    get totalFramesDropped(): number {
      let sum = 0;
      for (const s of stats.values()) sum += s.framesDropped;
      return sum;
    },

    get dropRate(): number {
      const decoded = this.totalFramesDecoded;
      if (decoded === 0) return 0;
      return (this.totalFramesDropped / decoded) * 100;
    },

    updateStats(deviceId: string, newStats: CameraStats) {
      stats.set(deviceId, newStats);
      bitrateSamples = [...bitrateSamples.slice(-59), this.totalBitrate];
    },

    remove(deviceId: string) {
      stats.delete(deviceId);
    },

    clear() {
      stats = new Map();
      bitrateSamples = [];
    },
  };
}

export const videoStatsStore = createVideoStatsStore();
```

Stats are polled from `RTCPeerConnection.getStats()` every 1s per camera connection.

## 9. New Dependencies

```json
{
  "dependencies": {
    "leaflet": "^1.9",
    "hls.js": "^1.5"
  },
  "devDependencies": {
    "@types/leaflet": "^1.9"
  }
}
```

- `leaflet`: ~40KB gzipped, well-maintained map library
- `hls.js`: ~60KB gzipped, fMP4 HLS playback for non-Safari browsers
- No Svelte-specific wrappers — direct API usage via `$effect` lifecycle

## 10. File Structure

```
ui/src/
├── lib/
│   ├── auth.ts
│   ├── signaling.ts
│   ├── sse.ts
│   ├── webrtc.ts
│   ├── connection-manager.ts
│   ├── playback.ts
│   ├── utils.ts
│   └── components/
│       ├── ui/                    bits-ui primitives
│       ├── Header.svelte
│       ├── Sidebar.svelte
│       ├── MobileDrawer.svelte
│       ├── CameraTile.svelte
│       ├── FocusLayout.svelte
│       ├── TimelineScrubber.svelte
│       ├── Sparkline.svelte
│       ├── TelemetryPanel.svelte
│       ├── DebugOverlay.svelte
│       ├── AlertsSheet.svelte
│       ├── SettingsSheet.svelte
│       ├── HlsPlayer.svelte
│       ├── CameraTable.svelte
│       ├── StatCard.svelte
│       ├── StatusCard.svelte
│       └── ConnectionStatus.svelte
├── views/
│   ├── LiveView.svelte
│   ├── MapView.svelte
│   └── DashboardView.svelte
├── stores/
│   ├── transport.svelte.ts
│   ├── cameras.svelte.ts
│   ├── settings.svelte.ts
│   ├── alerts.svelte.ts
│   ├── scrubber.svelte.ts
│   ├── thumbnails.svelte.ts
│   └── videoStats.svelte.ts
├── App.svelte
└── Login.svelte
```

## 11. Settings Store Additions

Add to `settings.svelte.ts` (Plan 10):

```typescript
// Additional persisted settings
let debugOverlay = $state<boolean>(loadSetting('debug-overlay', false));
let mapLayer = $state<'auto' | 'satellite'>(loadSetting('map-layer', 'auto'));
let liveLayout = $state<'grid' | 'focus'>(loadSetting('live-layout', 'grid'));

$effect(() => { saveSetting('debug-overlay', debugOverlay); });
$effect(() => { saveSetting('map-layer', mapLayer); });
$effect(() => { saveSetting('live-layout', liveLayout); });

// Resolved theme (for map tile layer selection)
get resolvedTheme(): 'dark' | 'light' {
  if (theme === 'system') {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  return theme;
}
```

## 12. Test Plan

### 12.1 Unit Tests — Sparkline

| # | Test | Validates |
|---|------|-----------|
| 1 | Empty data renders no path | No crash on empty array |
| 2 | Single data point renders no path | Minimum 2 points for a line |
| 3 | Uniform data renders flat line | All same value → horizontal |
| 4 | Varying data renders correct SVG path | Points map to percentages |
| 5 | Negative values handled | Clamped or normalized |

### 12.2 Unit Tests — Scrubber Store

| # | Test | Validates |
|---|------|-----------|
| 1 | Initial mode is 'live' | Correct default |
| 2 | `scrubTo()` sets mode to 'playback' | Mode transition |
| 3 | `goLive()` sets mode to 'live' | Return to live |
| 4 | Playhead advances in live mode | requestAnimationFrame ticks |
| 5 | Playhead stays fixed in playback mode | No tick |
| 6 | `setAvailableWindow()` stores range | Window accessible |
| 7 | `setCameraCoverage()` stores per-camera segments | Coverage data accessible |

### 12.3 Unit Tests — Thumbnail Store

| # | Test | Validates |
|---|------|-----------|
| 1 | `capture()` stores data URL | Thumbnail in map |
| 2 | `get()` returns null for missing camera | No crash |
| 3 | `clear()` empties map | Logout cleanup |
| 4 | Overwrite existing thumbnail | Latest capture stored |

### 12.4 Unit Tests — Video Stats Store

| # | Test | Validates |
|---|------|-----------|
| 1 | `updateStats()` stores per-camera stats | Stats retrievable |
| 2 | `totalBitrate` sums all cameras | Aggregate correct |
| 3 | `dropRate` computed correctly | frames_dropped / frames_decoded |
| 4 | `bitrateSamples` ring buffer at 60 | Oldest sample dropped |
| 5 | `remove()` cleans up camera | No stale data |

### 12.5 Component Tests — CameraTile

| # | Test | Validates |
|---|------|-----------|
| 1 | Live state renders `<video>` with srcObject | Video element present |
| 2 | Offline state renders "Offline" message | No video element |
| 3 | Playback state renders HlsPlayer | HLS component mounted |
| 4 | Loading state renders spinner | Spinner visible |
| 5 | Audio focus shows speaker icon | Indicator visible |
| 6 | Double-click name enables editing | Input element appears |
| 7 | PiP button calls requestPictureInPicture | API called |
| 8 | Snapshot button downloads PNG | Download triggered |

### 12.6 Component Tests — TimelineScrubber

| # | Test | Validates |
|---|------|-----------|
| 1 | Pointer drag updates playhead position | scrubTo called with correct time |
| 2 | Go Live button visible in playback mode | Button rendered |
| 3 | Go Live button hidden in live mode | Button not rendered |
| 4 | Coverage indicators render in grid mode | Colored bars visible |
| 5 | Coverage indicators hidden in focus mode | No bars |
| 6 | Timestamp label shows formatted time | Readable format |

### 12.7 Component Tests — MapView

| # | Test | Validates |
|---|------|-----------|
| 1 | Leaflet map initializes | Map container rendered |
| 2 | Cameras with GPS show markers | Markers on map |
| 3 | Cameras without GPS hidden | No marker |
| 4 | Dot marker mode renders circle markers | L.circleMarker instances |
| 5 | Detailed marker mode renders div icons with name | Camera name visible |
| 6 | PiP marker mode renders thumbnail images | img elements in markers |
| 7 | Marker click selects camera | onSelectCamera called |
| 8 | Track-all fits all markers | fitBounds called |
| 9 | User drag disengages tracking | tracking = 'manual' |
| 10 | Track-all button re-engages | tracking = 'all' |
| 11 | Theme change switches tile layer | Correct CARTO layer active |
| 12 | Satellite toggle switches to Esri layer | Satellite tiles loaded |
| 13 | Playback mode fades current markers | opacity 0.3 |
| 14 | Playback mode shows historical markers at full opacity | Blue markers at past positions |
| 15 | Go Live clears historical markers and restores current | Markers back to full opacity |

### 12.8 Component Tests — DashboardView

| # | Test | Validates |
|---|------|-----------|
| 1 | Stat cards show correct values | Online count, bitrate, frames, uptime |
| 2 | Status cards show correct health | Excellent/Good/Degraded thresholds |
| 3 | Per-camera table lists all cameras | Correct columns |
| 4 | Table shows telemetry data | CPU, memory, temp populated |
| 5 | Bandwidth sparkline updates | Sparkline data grows |

### 12.9 Integration Tests — View Switching

| # | Test | Validates |
|---|------|-----------|
| 1 | Switch Live → Map → Dashboard → Live | All views render, no crash |
| 2 | Camera streams persist across view switches | Video not interrupted |
| 3 | Scrubber visible in all views | Always rendered |
| 4 | Context controls change with view | Grid/Focus for Live, Dot/Detail/PiP for Map, none for Dashboard |
| 5 | Layout persisted to localStorage | Restored on reload |

### 12.10 Integration Tests — Scrubber + Playback

| # | Test | Validates |
|---|------|-----------|
| 1 | Scrub to past → tiles switch to HLS playback | Playback state rendered |
| 2 | Go Live → tiles switch back to WebRTC | Live state restored |
| 3 | Focus layout: scrub only affects focused camera | Others remain live |
| 4 | Grid layout: scrub affects all cameras | All enter playback |
| 5 | Scrub outside available window → no-footage state | "No footage" shown |
| 6 | Map view: scrub shows historical GPS markers | Blue markers at past positions |
| 7 | Map view: current markers faded during playback | Opacity reduced |
| 8 | Map view: Go Live restores current markers | Full opacity |

### 12.11 Manual End-to-End

| # | Test | Validates |
|---|------|-----------|
| 1 | Full login → live grid → video playing | Basic flow |
| 2 | Camera joins → tile appears in grid | SSE + WebRTC lifecycle |
| 3 | Camera leaves → tile shows offline | SSE + cleanup |
| 4 | Switch to focus layout → one enlarged | Layout switch |
| 5 | Click different tile → becomes focused | Focus change |
| 6 | Click tile for audio → speaker icon shows | Audio focus |
| 7 | Click same tile → audio muted | Toggle off |
| 8 | Switch to map → markers at GPS positions | Map rendering |
| 9 | Change marker mode dot → detailed → PiP | All three render |
| 10 | Switch to satellite tiles | Esri imagery loads |
| 11 | Click marker → sidebar selects camera | Selection sync |
| 12 | Switch to dashboard → stats + table shown | Dashboard rendering |
| 13 | Open settings sheet → change theme | Theme applies |
| 14 | Enable debug overlay → stats on tiles | Overlay visible |
| 15 | Open alerts sheet → see connect/disconnect history | Alerts listed |
| 16 | Rename camera → persists across reload | API call + optimistic update |
| 17 | Scrub timeline → playback transition | Live → HLS seamless |
| 18 | Go Live → return to live | HLS → WebRTC seamless |
| 19 | Scrub on map → historical markers shown, current faded | GPS time travel |
| 20 | Mobile: hamburger → drawer with camera list | Responsive layout |

### 12.12 Validation Checklist

```
[ ] Application shell renders (header, sidebar, main, scrubber)
[ ] View mode switcher works (Live, Map, Dashboard)
[ ] Live grid layout auto-fits cameras
[ ] Live focus layout enlarges one camera
[ ] Focus change works by clicking non-focused tile
[ ] Camera tiles render all 7 states correctly
[ ] Camera rename inline edit persists via API
[ ] Snapshot downloads PNG
[ ] PiP opens browser native PiP window
[ ] Map renders Leaflet with CARTO tiles
[ ] Map tile layer switches with theme
[ ] Satellite tile toggle works
[ ] All three marker modes render correctly
[ ] Track-all auto-fits, track-single follows, manual disengages on drag
[ ] Scrubber advances playhead in live mode
[ ] Scrubbing enters playback mode, Go Live returns
[ ] Focus layout scrubber only affects focused camera
[ ] Grid layout scrubber affects all cameras
[ ] Per-camera coverage indicators shown in grid mode
[ ] Map: playback mode fades current markers and shows historical markers
[ ] Map: Go Live restores current markers
[ ] Audio focus one-at-a-time model works
[ ] Dashboard stat cards show correct aggregates
[ ] Dashboard status cards with health thresholds
[ ] Per-camera table with all columns
[ ] Sparklines render for CPU, memory, bandwidth
[ ] Telemetry panel shows selected camera data
[ ] Debug overlay toggleable from settings
[ ] Alerts sheet lists session events
[ ] Settings sheet: theme, debug, connection status
[ ] Thumbnail store captures frames for map PiP
[ ] Mobile drawer accessible via hamburger
[ ] All stores clean up on logout
[ ] No memory leaks on view switching
```
