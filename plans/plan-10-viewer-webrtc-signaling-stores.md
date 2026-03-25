# Plan 10: Viewer — WebRTC, Signaling & Stores

## Overview

This plan covers the viewer's core runtime: per-camera WebRTC connections, SSE event handling, REST signaling, data channels for telemetry/commands, HLS playback for recordings, and the Svelte 5 store layer. It replaces the old per-group PeerConnection model with per-camera PeerConnections and replaces data-channel-based camera discovery with SSE push events.

**Depends on**: Plan 6 (HTTP API, SSE, WebRTC egress), Plan 1 (shared types)

## 1. Auth Flow

### Login

Cookie-based sessions. The viewer starts at a login page if no valid session cookie exists.

```typescript
// lib/auth.ts

export async function login(username: string, password: string): Promise<boolean> {
  const res = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
    credentials: 'include',
  });

  return res.ok;
}

export async function logout(): Promise<void> {
  await fetch('/api/v1/auth/logout', {
    method: 'POST',
    credentials: 'include',
  });
}

export async function checkSession(): Promise<boolean> {
  const res = await fetch('/api/v1/auth/me', {
    credentials: 'include',
  });
  return res.ok;
}
```

### Session Guard

The app component checks for a valid session on mount. If invalid, show login view. All API calls include `credentials: 'include'` to send the session cookie.

## 2. SSE Event Bus

### Connection

The viewer opens a single SSE connection to receive push events from the server:

```typescript
// lib/sse.ts

export type SseEvent =
  | { type: 'camera_online'; device_id: string; device_name: string }
  | { type: 'camera_offline'; device_id: string }
  | { type: 'camera_updated'; device_id: string; device_name: string };

export function connectSse(
  onEvent: (event: SseEvent) => void,
  onError: () => void,
): EventSource {
  const source = new EventSource('/api/v1/events', { withCredentials: true });

  source.addEventListener('camera_online', (e) => {
    onEvent({ type: 'camera_online', ...JSON.parse(e.data) });
  });

  source.addEventListener('camera_offline', (e) => {
    onEvent({ type: 'camera_offline', ...JSON.parse(e.data) });
  });

  source.addEventListener('camera_updated', (e) => {
    onEvent({ type: 'camera_updated', ...JSON.parse(e.data) });
  });

  source.onerror = () => {
    onError();
  };

  return source;
}
```

### Reconnection

`EventSource` auto-reconnects on error. The viewer tracks connection state for UI indicators:

```typescript
// In transport store
let sseConnected = $state(true);

source.onopen = () => { sseConnected = true; };
source.onerror = () => { sseConnected = false; };
```

## 3. REST Signaling

### Watch (Create PeerConnection)

```typescript
// lib/signaling.ts

export interface WatchResponse {
  session_id: string;
  sdp_answer: string;
}

export async function watchCamera(
  deviceId: string,
  sdpOffer: string,
): Promise<WatchResponse> {
  const res = await fetch(`/api/v1/cameras/${deviceId}/watch`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sdp_offer: sdpOffer }),
    credentials: 'include',
  });

  if (!res.ok) {
    throw new Error(`Watch failed: ${res.status} ${await res.text()}`);
  }

  return res.json();
}
```

### Unwatch (Tear Down)

```typescript
export async function unwatchCamera(sessionId: string): Promise<void> {
  await fetch(`/api/v1/sessions/${sessionId}`, {
    method: 'DELETE',
    credentials: 'include',
  });
}
```

### ICE Trickle

```typescript
export async function sendIceCandidate(
  sessionId: string,
  candidate: RTCIceCandidateInit,
): Promise<void> {
  await fetch(`/api/v1/sessions/${sessionId}/ice`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(candidate),
    credentials: 'include',
  });
}
```

### Camera List

```typescript
export interface CameraInfo {
  device_id: string;
  device_name: string;
  online: boolean;
}

export async function listCameras(): Promise<CameraInfo[]> {
  const res = await fetch('/api/v1/cameras', {
    credentials: 'include',
  });
  return res.json();
}
```

## 4. Per-Camera WebRTC Manager

### CameraConnection Class

Each camera gets its own `RTCPeerConnection`:

```typescript
// lib/webrtc.ts

export interface CameraCallbacks {
  onVideoTrack: (stream: MediaStream) => void;
  onAudioTrack: (stream: MediaStream) => void;
  onTelemetry: (data: TelemetryData) => void;
  onDisconnect: () => void;
}

export class CameraConnection {
  readonly deviceId: string;
  readonly pc: RTCPeerConnection;
  sessionId: string | null = null;

  private telemetryChannel: RTCDataChannel | null = null;
  private callbacks: CameraCallbacks;

  constructor(deviceId: string, callbacks: CameraCallbacks) {
    this.deviceId = deviceId;
    this.callbacks = callbacks;

    this.pc = new RTCPeerConnection({
      iceServers: [], // ICE-lite, no STUN/TURN needed
    });

    this.setupTrackHandler();
    this.setupDataChannels();
    this.setupConnectionStateHandler();
  }

  private setupTrackHandler() {
    this.pc.ontrack = (event) => {
      const stream = event.streams[0] ?? new MediaStream([event.track]);

      if (event.track.kind === 'video') {
        this.callbacks.onVideoTrack(stream);
      } else if (event.track.kind === 'audio') {
        this.callbacks.onAudioTrack(stream);
      }
    };
  }

  private setupDataChannels() {
    this.pc.ondatachannel = (event) => {
      const channel = event.channel;

      if (channel.label === 'telemetry') {
        this.telemetryChannel = channel;
        channel.onmessage = (msg) => {
          try {
            const data = JSON.parse(msg.data) as TelemetryData;
            this.callbacks.onTelemetry(data);
          } catch {
            // Ignore malformed telemetry
          }
        };
      }
    };
  }

  private setupConnectionStateHandler() {
    this.pc.onconnectionstatechange = () => {
      if (
        this.pc.connectionState === 'failed' ||
        this.pc.connectionState === 'closed'
      ) {
        this.callbacks.onDisconnect();
      }
    };
  }

  async connect(): Promise<void> {
    // Add transceivers for receiving
    this.pc.addTransceiver('video', { direction: 'recvonly' });
    this.pc.addTransceiver('audio', { direction: 'recvonly' });

    const offer = await this.pc.createOffer();
    await this.pc.setLocalDescription(offer);

    const response = await watchCamera(this.deviceId, offer.sdp!);
    this.sessionId = response.session_id;

    await this.pc.setRemoteDescription({
      type: 'answer',
      sdp: response.sdp_answer,
    });
  }

  async disconnect(): Promise<void> {
    if (this.sessionId) {
      await unwatchCamera(this.sessionId).catch(() => {});
      this.sessionId = null;
    }
    this.pc.close();
  }
}
```

### Connection Lifecycle

Connections are created/destroyed in response to SSE events and user actions:

```
camera_online SSE event → create CameraConnection → connect()
camera_offline SSE event → disconnect() + remove
User navigates away from camera → disconnect() + remove
PeerConnection fails → disconnect() + attempt reconnect
```

## 5. Connection Manager

Coordinates all camera connections:

```typescript
// lib/connection-manager.ts

export class ConnectionManager {
  private connections = new Map<string, CameraConnection>();
  private cameraStore: CameraStore;
  private reconnectTimers = new Map<string, ReturnType<typeof setTimeout>>();

  constructor(cameraStore: CameraStore) {
    this.cameraStore = cameraStore;
  }

  async connectCamera(deviceId: string): Promise<void> {
    // Don't double-connect
    if (this.connections.has(deviceId)) return;

    const conn = new CameraConnection(deviceId, {
      onVideoTrack: (stream) => {
        this.cameraStore.setVideoStream(deviceId, stream);
      },
      onAudioTrack: (stream) => {
        this.cameraStore.setAudioStream(deviceId, stream);
      },
      onTelemetry: (data) => {
        this.cameraStore.setTelemetry(deviceId, data);
      },
      onDisconnect: () => {
        this.handleDisconnect(deviceId);
      },
    });

    this.connections.set(deviceId, conn);

    try {
      await conn.connect();
    } catch (e) {
      console.error(`Failed to connect to ${deviceId}:`, e);
      this.connections.delete(deviceId);
      this.scheduleReconnect(deviceId);
    }
  }

  async disconnectCamera(deviceId: string): Promise<void> {
    this.clearReconnectTimer(deviceId);
    const conn = this.connections.get(deviceId);
    if (conn) {
      await conn.disconnect();
      this.connections.delete(deviceId);
    }
    this.cameraStore.clearStreams(deviceId);
  }

  private handleDisconnect(deviceId: string) {
    this.connections.delete(deviceId);
    this.cameraStore.clearStreams(deviceId);
    this.scheduleReconnect(deviceId);
  }

  private scheduleReconnect(deviceId: string) {
    // Only reconnect if camera is still online
    if (!this.cameraStore.isOnline(deviceId)) return;

    this.clearReconnectTimer(deviceId);
    const timer = setTimeout(() => {
      this.connectCamera(deviceId);
    }, 3000);
    this.reconnectTimers.set(deviceId, timer);
  }

  private clearReconnectTimer(deviceId: string) {
    const timer = this.reconnectTimers.get(deviceId);
    if (timer) {
      clearTimeout(timer);
      this.reconnectTimers.delete(deviceId);
    }
  }

  async disconnectAll(): Promise<void> {
    for (const [id] of this.connections) {
      await this.disconnectCamera(id);
    }
  }
}
```

## 6. Svelte 5 Stores

### Transport Store

Orchestrates SSE + ConnectionManager:

```typescript
// stores/transport.svelte.ts

interface TransportState {
  authenticated: boolean;
  sseConnected: boolean;
  connectionManager: ConnectionManager | null;
}

function createTransportStore() {
  let authenticated = $state(false);
  let sseConnected = $state(false);
  let sse: EventSource | null = null;
  let connManager: ConnectionManager | null = null;

  return {
    get authenticated() { return authenticated; },
    get sseConnected() { return sseConnected; },

    async initialize() {
      authenticated = await checkSession();
      if (!authenticated) return;

      // Fetch initial camera list
      const cameras = await listCameras();
      cameraStore.setInitialList(cameras);

      // Start SSE
      sse = connectSse(
        (event) => this.handleSseEvent(event),
        () => { sseConnected = false; },
      );
      sseConnected = true;

      // Create connection manager
      connManager = new ConnectionManager(cameraStore);

      // Connect to all online cameras
      for (const cam of cameras) {
        if (cam.online) {
          cameraStore.setOnline(cam.device_id, true);
          connManager.connectCamera(cam.device_id);
        }
      }
    },

    handleSseEvent(event: SseEvent) {
      switch (event.type) {
        case 'camera_online':
          cameraStore.setOnline(event.device_id, true);
          cameraStore.setName(event.device_id, event.device_name);
          connManager?.connectCamera(event.device_id);
          break;

        case 'camera_offline':
          cameraStore.setOnline(event.device_id, false);
          connManager?.disconnectCamera(event.device_id);
          break;

        case 'camera_updated':
          cameraStore.setName(event.device_id, event.device_name);
          break;
      }
    },

    async login(username: string, password: string): Promise<boolean> {
      const ok = await login(username, password);
      if (ok) {
        authenticated = true;
        await this.initialize();
      }
      return ok;
    },

    async logout() {
      await logout();
      authenticated = false;
      sse?.close();
      sse = null;
      sseConnected = false;
      await connManager?.disconnectAll();
      connManager = null;
      cameraStore.clear();
    },

    destroy() {
      sse?.close();
      connManager?.disconnectAll();
    },
  };
}

export const transportStore = createTransportStore();
```

### Camera Store

```typescript
// stores/cameras.svelte.ts

export interface TelemetryData {
  cpu_percent?: number;
  temp_celsius?: number;
  memory_mb?: number;
  uptime_secs?: number;
  gps?: { lat: number; lon: number; alt?: number };
}

interface CameraState {
  device_id: string;
  device_name: string;
  online: boolean;
  videoStream: MediaStream | null;
  audioStream: MediaStream | null;
  telemetry: TelemetryData | null;
  lastTelemetryAt: number | null;
}

function createCameraStore() {
  let cameras = $state<Map<string, CameraState>>(new Map());

  return {
    get cameras() { return cameras; },

    get list(): CameraState[] {
      return [...cameras.values()];
    },

    get onlineList(): CameraState[] {
      return [...cameras.values()].filter((c) => c.online);
    },

    getCamera(deviceId: string): CameraState | undefined {
      return cameras.get(deviceId);
    },

    isOnline(deviceId: string): boolean {
      return cameras.get(deviceId)?.online ?? false;
    },

    setInitialList(list: CameraInfo[]) {
      cameras = new Map(
        list.map((c) => [
          c.device_id,
          {
            device_id: c.device_id,
            device_name: c.device_name,
            online: c.online,
            videoStream: null,
            audioStream: null,
            telemetry: null,
            lastTelemetryAt: null,
          },
        ]),
      );
    },

    setOnline(deviceId: string, online: boolean) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.online = online;
      } else {
        cameras.set(deviceId, {
          device_id: deviceId,
          device_name: deviceId,
          online,
          videoStream: null,
          audioStream: null,
          telemetry: null,
          lastTelemetryAt: null,
        });
      }
    },

    setName(deviceId: string, name: string) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.device_name = name;
      }
    },

    setVideoStream(deviceId: string, stream: MediaStream) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.videoStream = stream;
      }
    },

    setAudioStream(deviceId: string, stream: MediaStream) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.audioStream = stream;
      }
    },

    setTelemetry(deviceId: string, data: TelemetryData) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.telemetry = data;
        cam.lastTelemetryAt = Date.now();
      }
    },

    clearStreams(deviceId: string) {
      const cam = cameras.get(deviceId);
      if (cam) {
        cam.videoStream = null;
        cam.audioStream = null;
      }
    },

    clear() {
      cameras = new Map();
    },
  };
}

export const cameraStore = createCameraStore();
```

### Settings Store

```typescript
// stores/settings.svelte.ts

type Theme = 'dark' | 'light' | 'system';
type GridLayout = '1x1' | '2x2' | '3x3' | '4x4' | 'auto';

function loadSetting<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(`ghostcam-${key}`);
    return raw ? JSON.parse(raw) : fallback;
  } catch {
    return fallback;
  }
}

function saveSetting(key: string, value: unknown) {
  localStorage.setItem(`ghostcam-${key}`, JSON.stringify(value));
}

function createSettingsStore() {
  let theme = $state<Theme>(loadSetting('theme', 'dark'));
  let gridLayout = $state<GridLayout>(loadSetting('grid-layout', 'auto'));
  let globalMuted = $state<boolean>(loadSetting('global-muted', true));
  let unmutedCameraId = $state<string | null>(loadSetting('unmuted-camera', null));

  // Persist on change
  $effect(() => { saveSetting('theme', theme); });
  $effect(() => { saveSetting('grid-layout', gridLayout); });
  $effect(() => { saveSetting('global-muted', globalMuted); });
  $effect(() => { saveSetting('unmuted-camera', unmutedCameraId); });

  return {
    get theme() { return theme; },
    set theme(v: Theme) { theme = v; },

    get gridLayout() { return gridLayout; },
    set gridLayout(v: GridLayout) { gridLayout = v; },

    get globalMuted() { return globalMuted; },
    set globalMuted(v: boolean) { globalMuted = v; },

    get unmutedCameraId() { return unmutedCameraId; },

    toggleMute(deviceId: string) {
      if (unmutedCameraId === deviceId) {
        unmutedCameraId = null;
      } else {
        unmutedCameraId = deviceId;
        globalMuted = false;
      }
    },

    isMuted(deviceId: string): boolean {
      if (globalMuted) return true;
      return unmutedCameraId !== deviceId;
    },
  };
}

export const settingsStore = createSettingsStore();
```

### Alerts Store

```typescript
// stores/alerts.svelte.ts

interface Alert {
  id: string;
  type: 'info' | 'warning' | 'error';
  message: string;
  cameraId?: string;
  timestamp: number;
  dismissed: boolean;
}

function createAlertStore() {
  let alerts = $state<Alert[]>([]);
  let nextId = 0;

  return {
    get alerts() { return alerts; },

    get active(): Alert[] {
      return alerts.filter((a) => !a.dismissed);
    },

    add(type: Alert['type'], message: string, cameraId?: string) {
      const id = `alert-${nextId++}`;
      alerts = [
        ...alerts,
        { id, type, message, cameraId, timestamp: Date.now(), dismissed: false },
      ];

      // Auto-dismiss info alerts after 5s
      if (type === 'info') {
        setTimeout(() => this.dismiss(id), 5000);
      }
    },

    dismiss(id: string) {
      const alert = alerts.find((a) => a.id === id);
      if (alert) {
        alert.dismissed = true;
      }
    },

    clearAll() {
      alerts = [];
    },
  };
}

export const alertStore = createAlertStore();
```

## 7. HLS Playback

### Manifest Fetching

```typescript
// lib/playback.ts

export async function getHlsManifest(
  deviceId: string,
  startTime: number, // Unix seconds
  endTime: number,
): Promise<string> {
  const res = await fetch(
    `/api/v1/cameras/${deviceId}/playback?start=${startTime}&end=${endTime}`,
    { credentials: 'include' },
  );
  if (!res.ok) {
    throw new Error(`Playback manifest failed: ${res.status}`);
  }
  return res.text();
}

export async function getSegmentUrl(
  deviceId: string,
  segmentId: string,
): Promise<string> {
  return `/api/v1/cameras/${deviceId}/segments/${segmentId}`;
}
```

### HLS Player Component

```svelte
<!-- lib/components/HlsPlayer.svelte -->
<script lang="ts">
  import Hls from 'hls.js';

  let { src, class: className = '' }: { src: string; class?: string } = $props();

  let videoEl: HTMLVideoElement;

  $effect(() => {
    if (!src || !videoEl) return;

    if (videoEl.canPlayType('application/vnd.apple.mpegurl')) {
      // Safari native HLS
      videoEl.src = src;
    } else if (Hls.isSupported()) {
      const hls = new Hls();
      hls.loadSource(src);
      hls.attachMedia(videoEl);
      return () => hls.destroy();
    }
  });
</script>

<video bind:this={videoEl} class={className} controls playsinline />
```

### Dependency

```json
{
  "dependencies": {
    "hls.js": "^1.5"
  }
}
```

`hls.js` is ~60KB gzipped, well-maintained, handles fMP4 segments. Safari uses native HLS so hls.js isn't loaded there.

## 8. Vite Proxy Configuration

```typescript
// vite.config.ts

export default defineConfig({
  plugins: [svelte()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
    },
  },
});
```

## 9. File Structure

```
ui/src/
├── lib/
│   ├── auth.ts                  Login/logout/session check
│   ├── signaling.ts             Watch/unwatch/ICE REST calls
│   ├── sse.ts                   SSE connection + event types
│   ├── webrtc.ts                CameraConnection class
│   ├── connection-manager.ts    Manages all CameraConnections
│   ├── playback.ts              HLS manifest fetching
│   ├── utils.ts                 cn() helper, formatters
│   └── components/
│       ├── ui/                  bits-ui primitives
│       └── HlsPlayer.svelte    HLS video player
├── stores/
│   ├── transport.svelte.ts      SSE + connection orchestration
│   ├── cameras.svelte.ts        Camera state map
│   ├── settings.svelte.ts       Persisted preferences
│   └── alerts.svelte.ts         Notification queue
├── routes/                      View components (Plan 11)
└── App.svelte                   Root: auth guard + layout
```

## 10. Test Plan

### 10.1 Unit Tests — Auth

| # | Test | Validates |
|---|------|-----------|
| 1 | `login()` sends correct request shape | POST with JSON body, credentials: include |
| 2 | `login()` returns true on 200 | Session established |
| 3 | `login()` returns false on 401 | Bad credentials handled |
| 4 | `checkSession()` returns true on 200 | Valid session cookie |
| 5 | `checkSession()` returns false on 401 | Expired/missing session |

### 10.2 Unit Tests — Signaling

| # | Test | Validates |
|---|------|-----------|
| 1 | `watchCamera()` sends SDP offer, returns session_id + answer | Correct request/response shape |
| 2 | `watchCamera()` throws on non-200 | Error propagated with status |
| 3 | `unwatchCamera()` sends DELETE | Session cleanup |
| 4 | `sendIceCandidate()` sends POST with candidate | ICE trickle |
| 5 | `listCameras()` returns parsed array | Initial camera discovery |

### 10.3 Unit Tests — SSE

| # | Test | Validates |
|---|------|-----------|
| 1 | Parse `camera_online` event | device_id + device_name extracted |
| 2 | Parse `camera_offline` event | device_id extracted |
| 3 | Parse `camera_updated` event | Updated name extracted |
| 4 | Error callback fires on connection loss | sseConnected set to false |

### 10.4 Unit Tests — Camera Store

| # | Test | Validates |
|---|------|-----------|
| 1 | `setInitialList()` populates map | All cameras present |
| 2 | `setOnline()` creates entry if missing | New camera handled |
| 3 | `setVideoStream()` updates existing camera | Stream assigned |
| 4 | `clearStreams()` nulls video and audio | Cleanup on disconnect |
| 5 | `onlineList` filters correctly | Only online cameras returned |
| 6 | `clear()` empties the map | Logout cleanup |

### 10.5 Unit Tests — Settings Store

| # | Test | Validates |
|---|------|-----------|
| 1 | Default values when localStorage empty | theme=dark, globalMuted=true |
| 2 | Values persisted to localStorage | Write on change |
| 3 | Values loaded from localStorage | Restored on creation |
| 4 | `toggleMute()` unmutes specific camera | unmutedCameraId set, globalMuted false |
| 5 | `toggleMute()` same camera again mutes | unmutedCameraId null |
| 6 | `isMuted()` respects global and per-camera | Correct mute state |

### 10.6 Unit Tests — Alerts Store

| # | Test | Validates |
|---|------|-----------|
| 1 | `add()` creates alert with unique id | Alert in list |
| 2 | `dismiss()` marks alert dismissed | Not in `active` |
| 3 | Info alerts auto-dismiss after 5s | Timeout fires |
| 4 | `clearAll()` empties list | Full reset |

### 10.7 Integration Tests — WebRTC Connection

| # | Test | Validates |
|---|------|-----------|
| 1 | `CameraConnection.connect()` creates offer and sets remote answer | PeerConnection in stable state |
| 2 | Video track received via `ontrack` | videoStream callback fires |
| 3 | Audio track received via `ontrack` | audioStream callback fires |
| 4 | Telemetry data channel message parsed | telemetry callback fires with data |
| 5 | Connection failure triggers `onDisconnect` | Cleanup callback fires |

### 10.8 Integration Tests — Connection Manager

| # | Test | Validates |
|---|------|-----------|
| 1 | `connectCamera()` creates CameraConnection and calls connect | Connection in map |
| 2 | `disconnectCamera()` calls disconnect and removes | Connection removed, streams cleared |
| 3 | Double `connectCamera()` is a no-op | No duplicate connections |
| 4 | Disconnect triggers reconnect for online cameras | Timer scheduled |
| 5 | Disconnect does not reconnect offline cameras | No timer |
| 6 | `disconnectAll()` cleans up everything | Map empty |

### 10.9 Integration Tests — Transport Store (End-to-End)

| # | Test | Validates |
|---|------|-----------|
| 1 | `initialize()` fetches cameras, opens SSE, connects online cameras | Full startup flow |
| 2 | SSE `camera_online` → camera connected | New PeerConnection created |
| 3 | SSE `camera_offline` → camera disconnected | PeerConnection closed, streams cleared |
| 4 | `logout()` tears down everything | SSE closed, connections closed, stores cleared |
| 5 | SSE reconnect after error | EventSource auto-reconnects, sseConnected restored |

### 10.10 Integration Tests — HLS Playback (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | HLS manifest URL loads in HlsPlayer | Video element plays recorded segments |
| 2 | Safari native HLS works | No hls.js needed |
| 3 | Chrome/Firefox use hls.js fallback | hls.js loaded and attached |
| 4 | Seek within recording | Correct segment loaded |

### 10.11 Manual End-to-End

| # | Test | Validates |
|---|------|-----------|
| 1 | Login → camera grid shows all online cameras with video | Full flow |
| 2 | Start a new camera → SSE event → video appears | Dynamic camera join |
| 3 | Stop a camera → SSE event → video removed | Dynamic camera leave |
| 4 | Camera reconnect → video resumes | Reconnection works |
| 5 | Server restart → SSE reconnects → cameras reconnect | Full recovery |
| 6 | Mute/unmute audio per camera | Settings store controls audio |
| 7 | Multiple cameras streaming simultaneously | No cross-talk, correct streams |

### 10.12 Validation Checklist

```
[ ] Login page authenticates and sets session cookie
[ ] checkSession() correctly identifies valid/expired sessions
[ ] SSE connection established on initialize
[ ] SSE auto-reconnects on error
[ ] Initial camera list fetched and displayed
[ ] camera_online SSE → PeerConnection created → video/audio streams received
[ ] camera_offline SSE → PeerConnection closed → streams cleared
[ ] Telemetry received via data channel and displayed
[ ] Connection failure triggers reconnect attempt
[ ] Per-camera mute/unmute works
[ ] Settings persisted to localStorage
[ ] HLS playback of recorded segments works
[ ] Logout tears down all connections and clears state
[ ] No memory leaks on connect/disconnect cycles
[ ] All unit tests pass
```
