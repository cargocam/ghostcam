# Plan 6: Server Core — WebRTC Egress & HTTP API

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 3 (ingest), Plan 4 (PKI/enrollment), Plan 5 (Redis)
**Unlocks:** Plan 10 (viewer core), Plan 12 (deployment)

---

## 1. Goal

Implement the server's outward-facing surfaces: WebRTC egress via str0m (per-camera PeerConnections), SSE for server-to-client push events, the full Axum HTTP API with auth middleware, HLS proxy endpoints, and static file serving for the Svelte SPA.

After this plan, the server is a complete runnable binary. An observer can authenticate, establish WebRTC sessions to individual cameras, receive live video/audio/telemetry, subscribe to camera online/offline events via SSE, manage cameras and tokens, and access HLS endpoints for playback.

---

## 2. Crate Changes

### 2.1 New Dependencies

**Workspace `Cargo.toml`** — add:
```toml
[workspace.dependencies]
str0m = "0.6"
systemtime-mime = "0.1"    # for static file Content-Type
tower-cookies = "0.10"
cookie = "0.18"
```

**`server-core/Cargo.toml`** — add:
```toml
[dependencies]
str0m.workspace = true
tower.workspace = true
tower-http.workspace = true
tower-cookies.workspace = true
cookie.workspace = true
axum-extra.workspace = true
serde_json.workspace = true
rand.workspace = true
base64.workspace = true
uuid.workspace = true
```

---

## 3. Implementation Details

### 3.1 EgressHandle (`server-core/src/egress/handle.rs`)

One EgressHandle per observer×camera pair. Subscribes to an IngestSlot's broadcast channels and drives the str0m WebRTC send loop.

```rust
pub struct EgressHandle {
    /// Session identifier
    pub session_id: String,
    /// Which camera this handle watches
    pub device_id: DeviceId,
    /// The str0m Rtc instance
    rtc: Rtc,
    /// Video RTP track mid
    video_mid: Mid,
    /// Audio RTP track mid
    audio_mid: Mid,
    /// Telemetry data channel (unreliable, unordered)
    telemetry_channel: ChannelId,
    /// Commands data channel (reliable, ordered) — client → server
    commands_channel: ChannelId,
    /// Broadcast receivers from the IngestSlot
    video_rx: broadcast::Receiver<VideoFrame>,
    audio_rx: broadcast::Receiver<AudioFrame>,
    telemetry_rx: broadcast::Receiver<TelemetryDatagram>,
    /// Reference to IngestSlot subscriber counters
    video_subscribers: Arc<AtomicUsize>,
    audio_subscribers: Arc<AtomicUsize>,
    /// Current client mode
    client_mode: ClientMode,
    /// UDP socket for WebRTC
    udp_socket: UdpSocket,
    /// Cancellation
    cancel: CancellationToken,
}
```

#### Construction

```rust
impl EgressHandle {
    /// Create a new egress handle from an SDP offer.
    ///
    /// 1. Create str0m Rtc with ICE-lite
    /// 2. Add video track (H.264, recvonly from server perspective = sendonly)
    /// 3. Add audio track (Opus, recvonly)
    /// 4. Create telemetry data channel (unreliable, unordered)
    /// 5. Create commands data channel (reliable, ordered)
    /// 6. Apply SDP offer, generate SDP answer
    /// 7. Subscribe to IngestSlot broadcast channels
    /// 8. Return (handle, sdp_answer)
    pub async fn create(
        session_id: String,
        slot: &Arc<IngestSlot>,
        sdp_offer: &str,
        public_addr: SocketAddr,
    ) -> Result<(Self, String)>;
}
```

#### Event Loop

```rust
impl EgressHandle {
    /// Run the WebRTC event loop. Blocks until session ends.
    ///
    /// Concurrently:
    /// - Poll str0m for outgoing UDP packets → send on socket
    /// - Receive UDP packets from socket → feed to str0m
    /// - Receive video frames from broadcast → packetize as H.264 RTP → write via str0m
    /// - Receive audio frames from broadcast → write as Opus RTP via str0m
    /// - Receive telemetry from broadcast → encode MessagePack → send on telemetry data channel
    /// - Receive data channel messages from str0m → dispatch client_mode changes
    /// - Handle str0m timeouts
    pub async fn run(mut self) -> Result<()>;
}
```

### 3.2 H.264 RTP Packetization (`server-core/src/egress/rtp.rs`)

Reuse the packetization logic from the existing codebase, adapted for str0m 0.6.

```rust
/// NAL accumulator: buffers non-VCL NALs (SPS, PPS, SEI) until a VCL NAL
/// arrives, then sends all together as Annex-B.
pub struct NalAccumulator { ... }

/// Cache of most recent SPS and PPS per camera, for late-joining viewers.
pub struct SpsCache { ... }
```

str0m handles the actual FU-A fragmentation — we feed it complete NAL units (or Annex-B access units) and it handles packetization into RTP.

### 3.3 Data Channel Handling (`server-core/src/egress/data_channel.rs`)

```rust
/// Messages received from the client on the commands data channel (JSON).
#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ClientMessage {
    ClientMode { mode: ClientMode },
}

/// Handle an incoming data channel message.
/// Dispatches client_mode changes to the demand tracker.
pub async fn handle_client_message(
    handle: &mut EgressHandle,
    slot: &Arc<IngestSlot>,
    msg: ClientMessage,
) -> Result<()> {
    match msg {
        ClientMessage::ClientMode { mode } => {
            let old = handle.client_mode;
            handle.client_mode = mode;
            update_subscriber_demand(slot, Some(old), mode).await?;
        }
    }
    Ok(())
}
```

### 3.4 Session Manager (`server-core/src/egress/sessions.rs`)

Tracks active WebRTC sessions for teardown and SSE event scoping.

```rust
pub struct SessionManager {
    /// session_id → (device_id, cancel_token, JoinHandle)
    sessions: RwLock<HashMap<String, SessionEntry>>,
}

struct SessionEntry {
    device_id: DeviceId,
    user_id: UserId,
    cancel: CancellationToken,
    handle: JoinHandle<()>,
}

impl SessionManager {
    pub fn new() -> Self;

    /// Register a new session. Spawns the EgressHandle event loop.
    pub async fn register(
        &self,
        session_id: String,
        egress: EgressHandle,
    );

    /// Tear down a session by ID.
    pub async fn teardown(&self, session_id: &str) -> Result<()>;

    /// Tear down all sessions for a device (called on camera disconnect).
    pub async fn teardown_by_device(&self, device_id: &DeviceId);

    /// Tear down all sessions for a user (called on user logout).
    pub async fn teardown_by_user(&self, user_id: &UserId);

    /// List session IDs for a user.
    pub async fn list_sessions(&self, user_id: &UserId) -> Vec<String>;
}
```

### 3.5 SSE Event Bus (`server-core/src/sse.rs`)

Server-to-client push events via Server-Sent Events.

```rust
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum SseEvent {
    CameraOnline { device_id: String },
    CameraOffline { device_id: String },
}

pub struct SseEventBus {
    /// Per-user broadcast channels for SSE fan-out.
    channels: RwLock<HashMap<UserId, broadcast::Sender<SseEvent>>>,
}

impl SseEventBus {
    pub fn new() -> Self;

    /// Subscribe to events for a user. Returns a broadcast receiver.
    pub async fn subscribe(&self, user_id: &UserId) -> broadcast::Receiver<SseEvent>;

    /// Publish an event to all subscribers for a user.
    pub async fn publish(&self, user_id: &UserId, event: SseEvent);

    /// Clean up channel when no subscribers remain.
    pub async fn cleanup(&self, user_id: &UserId);
}
```

Wire into the routing registry: when a slot is registered → publish `CameraOnline`; when unregistered → publish `CameraOffline`.

### 3.6 Auth Middleware (`server-core/src/api/auth.rs`)

Extracts the authenticated user from either a session cookie or a Bearer API token.

```rust
/// Authenticated user identity, extracted by middleware.
#[derive(Debug, Clone)]
pub struct AuthUser {
    pub user_id: UserId,
}

/// Axum middleware extractor. Checks (in order):
/// 1. `Authorization: Bearer <token>` header → HMAC verify against api_tokens
/// 2. `ghostcam-session` cookie → look up in sessions table
/// Returns 401 if neither is present or valid.
pub async fn auth_middleware(
    State(state): State<Arc<AppState>>,
    mut request: Request,
    next: Next,
) -> Response;

/// Axum extractor for the authenticated user.
/// Use in handler signatures: `Extension(user): Extension<AuthUser>`
```

### 3.7 AppState (`server-core/src/api/state.rs`)

Shared application state passed to all Axum handlers.

```rust
pub struct AppState {
    pub db: Arc<dyn Database>,
    pub redis: Arc<RedisManager>,
    pub registry: Arc<RoutingRegistry>,
    pub sessions: Arc<SessionManager>,
    pub sse_bus: Arc<SseEventBus>,
    pub ca: Arc<CaManager>,
    pub revocation_cache: Arc<RevocationCache>,
    pub hmac_secret: Vec<u8>,
    pub server_addr: String,
    pub public_addr: SocketAddr,
}
```

### 3.8 HTTP API Routes (`server-core/src/api/routes.rs`)

```rust
pub fn build_router(state: Arc<AppState>) -> Router {
    let api = Router::new()
        // Auth (no auth middleware)
        .route("/api/v1/auth/login", post(auth::login))
        .route("/api/v1/auth/logout", post(auth::logout))
        .route("/api/v1/auth/password", patch(auth::change_password))

        // Cameras (auth required)
        .route("/api/v1/cameras", get(cameras::list).post(cameras::enroll))
        .route("/api/v1/cameras/:device_id",
            get(cameras::get).patch(cameras::update).delete(cameras::delete))

        // Camera networks (auth required)
        .route("/api/v1/cameras/:device_id/networks",
            get(cameras::list_networks).post(cameras::add_network))
        .route("/api/v1/cameras/:device_id/networks/:ssid",
            delete(cameras::remove_network))

        // Watch / session (auth required)
        .route("/api/v1/watch", post(watch::create_session))
        .route("/api/v1/session/:id", delete(watch::teardown_session))
        .route("/api/v1/session/:id/ice", post(watch::ice_candidate))

        // API tokens (auth required)
        .route("/api/v1/tokens", get(tokens::list).post(tokens::create))
        .route("/api/v1/tokens/:token_id", delete(tokens::revoke))

        // Telemetry (auth required, already implemented in Plan 5)
        .route("/api/v1/telemetry/:device_id/latest", get(telemetry_api::handle_latest))
        .route("/api/v1/telemetry/:device_id", get(telemetry_api::handle_range))

        // SSE (auth required)
        .route("/events", get(sse::handle_sse))

        // HLS (auth required)
        .route("/hls/:device_id/init.mp4", get(hls::get_init))
        .route("/hls/:device_id/playlist.m3u8", get(hls::get_manifest))
        .route("/hls/:device_id/:segment_id", get(hls::get_segment))

        // Apply auth middleware to protected routes
        .layer(middleware::from_fn_with_state(state.clone(), auth_middleware));

    let public = Router::new()
        .route("/healthz", get(health::healthz))
        .route("/readyz", get(health::readyz));

    // Static files (SPA fallback)
    let static_files = get_service(ServeDir::new("ui/build")
        .fallback(ServeFile::new("ui/build/index.html")));

    Router::new()
        .merge(api)
        .merge(public)
        .fallback_service(static_files)
        .with_state(state)
}
```

### 3.9 API Handlers

#### Auth (`server-core/src/api/auth.rs`)

```rust
/// POST /api/v1/auth/login
/// Body: { "password": "..." } (server-solo)
/// Returns: Set-Cookie with session token
pub async fn login(State(state), Json(body)) -> Response;

/// POST /api/v1/auth/logout
/// Clears session cookie, deletes session from DB.
pub async fn logout(State(state), Extension(user)) -> Response;

/// PATCH /api/v1/auth/password
/// Body: { "current_password": "...", "new_password": "..." }
pub async fn change_password(State(state), Extension(user), Json(body)) -> Response;
```

#### Cameras (`server-core/src/api/cameras.rs`)

```rust
/// GET /api/v1/cameras
/// Returns all cameras (online and offline) for the authenticated user.
/// Augments DB records with online status from the routing registry.
pub async fn list(State(state), Extension(user)) -> Json<Vec<CameraResponse>>;

/// POST /api/v1/cameras/enroll
/// Body: { "display_name"?: string, "wifi"?: [{ "ssid", "psk" }] }
/// Returns: { "token": "<JWT string>", "expires_at": unix_ts }
/// The client renders the JWT as a QR code.
pub async fn enroll(State(state), Extension(user), Json(body)) -> Json<EnrollResponse>;

/// GET /api/v1/cameras/:device_id
pub async fn get(State(state), Extension(user), Path(device_id)) -> Result<Json<CameraResponse>>;

/// PATCH /api/v1/cameras/:device_id
/// Body: { "display_name"?: string, "notes"?: string }
pub async fn update(State(state), Extension(user), Path(device_id), Json(body)) -> Response;

/// DELETE /api/v1/cameras/:device_id
/// Triggers unregistration flow (Plan 4).
pub async fn delete(State(state), Extension(user), Path(device_id)) -> Response;

/// GET /api/v1/cameras/:device_id/networks
/// Reads cached network list from the IngestSlot (populated from `networks` alert).
pub async fn list_networks(...) -> Json<Vec<NetworkEntry>>;

/// POST /api/v1/cameras/:device_id/networks
/// Body: { "ssid": string, "psk": string }
/// Sends network_config command to camera.
pub async fn add_network(...) -> Response;

/// DELETE /api/v1/cameras/:device_id/networks/:ssid
/// Sends remove_network command to camera.
pub async fn remove_network(...) -> Response;

#[derive(Serialize)]
pub struct CameraResponse {
    pub device_id: String,
    pub display_name: String,
    pub enrolled_at: u64,
    pub last_seen_at: Option<u64>,
    pub notes: Option<String>,
    pub online: bool,
}

#[derive(Serialize)]
pub struct EnrollResponse {
    pub token: String,
    pub expires_at: u64,
}
```

#### Watch (`server-core/src/api/watch.rs`)

```rust
/// POST /api/v1/watch
/// Body: { "sdp_offer": string, "device_id": string }
/// Creates a WebRTC session for the specified camera.
///
/// 1. Verify camera belongs to authenticated user
/// 2. Look up IngestSlot in routing registry
/// 3. Create EgressHandle from SDP offer
/// 4. Register session in SessionManager
/// 5. Return { session_id, sdp_answer }
pub async fn create_session(
    State(state),
    Extension(user),
    Json(body),
) -> Result<Json<WatchResponse>>;

/// DELETE /api/v1/session/:id
/// Tears down a WebRTC session.
pub async fn teardown_session(
    State(state),
    Extension(user),
    Path(session_id),
) -> Response;

/// POST /api/v1/session/:id/ice
/// Body: { "candidate": string, "sdp_mid": string, "sdp_mline_index": u32 }
/// ICE trickle candidate. With ICE-lite on the server, this may be a no-op
/// but the endpoint must exist.
pub async fn ice_candidate(...) -> Response;

#[derive(Serialize)]
pub struct WatchResponse {
    pub session_id: String,
    pub sdp_answer: String,
}
```

#### Tokens (`server-core/src/api/tokens.rs`)

```rust
/// GET /api/v1/tokens
pub async fn list(State(state), Extension(user)) -> Json<Vec<TokenResponse>>;

/// POST /api/v1/tokens
/// Body: { "label": string, "expires_at"?: unix_ts }
/// Returns { token_id, raw_token } — raw_token shown once.
pub async fn create(State(state), Extension(user), Json(body)) -> Json<CreateTokenResponse>;

/// DELETE /api/v1/tokens/:token_id
pub async fn revoke(State(state), Extension(user), Path(token_id)) -> Response;
```

#### SSE (`server-core/src/api/sse.rs`)

```rust
/// GET /events
/// SSE stream of camera_online/camera_offline events.
/// Held open for the session lifetime. Client auto-reconnects on disconnect.
pub async fn handle_sse(
    State(state),
    Extension(user),
) -> Sse<impl Stream<Item = Result<Event, Infallible>>> {
    let rx = state.sse_bus.subscribe(&user.user_id).await;
    let stream = BroadcastStream::new(rx).filter_map(|event| {
        match event {
            Ok(sse_event) => {
                let data = serde_json::to_string(&sse_event).ok()?;
                let event_type = match &sse_event {
                    SseEvent::CameraOnline { .. } => "camera_online",
                    SseEvent::CameraOffline { .. } => "camera_offline",
                };
                Some(Ok(Event::default().event(event_type).data(data)))
            }
            Err(_) => None,
        }
    });
    Sse::new(stream).keep_alive(
        axum::response::sse::KeepAlive::new()
            .interval(Duration::from_secs(15))
    )
}
```

#### HLS (`server-core/src/api/hls.rs`)

```rust
/// GET /hls/:device_id/playlist.m3u8
/// Serves the in-memory manifest from the IngestSlot.
pub async fn get_manifest(
    State(state), Extension(user), Path(device_id),
) -> Result<Response>;

/// GET /hls/:device_id/init.mp4
/// Triggers upload_init command to camera, waits for upload stream, returns bytes.
/// Uses request coalescing — only one upload in flight per camera.
pub async fn get_init(
    State(state), Extension(user), Path(device_id),
) -> Result<Response>;

/// GET /hls/:device_id/:segment_id
/// Triggers upload_segment command to camera (or serves from in-flight buffer).
/// Cache-Control: private, max-age=3600
pub async fn get_segment(
    State(state), Extension(user), Path((device_id, segment_id)),
) -> Result<Response>;
```

#### Health (`server-core/src/api/health.rs`)

```rust
/// GET /healthz — always 200
pub async fn healthz() -> &'static str { "ok" }

/// GET /readyz — 200 if DB + QUIC + HTTP ready; Redis optional
pub async fn readyz(State(state)) -> Json<ReadyResponse>;

#[derive(Serialize)]
pub struct ReadyResponse {
    pub status: String,    // "ready" or "not_ready"
    pub database: String,  // "ok" or "unavailable"
    pub redis: String,     // "ok" or "unavailable"
    pub quic: String,      // "ok"
}
```

### 3.10 SSE Wiring to Registry

Update the routing registry (Plan 3) to publish SSE events on slot register/unregister:

```rust
// In registry.rs:
impl RoutingRegistry {
    pub async fn register(&self, slot: Arc<IngestSlot>, sse_bus: &SseEventBus) {
        // ... existing insert logic ...
        sse_bus.publish(&slot.user_id, SseEvent::CameraOnline {
            device_id: slot.device_id.0.clone(),
        }).await;
    }

    pub async fn unregister(&self, device_id: &DeviceId, sse_bus: &SseEventBus) {
        // ... existing remove logic ...
        // also teardown all sessions for this device
        sse_bus.publish(&user_id, SseEvent::CameraOffline {
            device_id: device_id.0.clone(),
        }).await;
    }
}
```

### 3.11 Server-Solo Main Update

Update `server-solo/src/main.rs` to boot the full server:

```rust
#[tokio::main]
async fn main() -> Result<()> {
    // 1. Parse CLI args / env vars
    // 2. Bootstrap PKI (Plan 4)
    // 3. Open database (Plan 2)
    // 4. Connect to Redis (Plan 5)
    // 5. Build AppState
    // 6. Spawn QUIC accept loop (Plan 3)
    // 7. Spawn revocation refresh loop (Plan 5)
    // 8. Start Axum HTTP server
    // 9. Await shutdown signal
}
```

### 3.12 Module Structure

```
server-core/src/
├── egress/
│   ├── mod.rs
│   ├── handle.rs               # EgressHandle
│   ├── rtp.rs                  # H.264 NAL accumulator, SPS cache
│   ├── data_channel.rs         # ClientMessage, dispatch
│   └── sessions.rs             # SessionManager
├── sse.rs                      # SseEventBus
├── api/
│   ├── mod.rs
│   ├── state.rs                # AppState
│   ├── routes.rs               # Router builder
│   ├── auth.rs                 # Auth middleware + login/logout/password
│   ├── cameras.rs              # Camera CRUD + enrollment endpoint + networks
│   ├── watch.rs                # WebRTC session create/teardown/ice
│   ├── tokens.rs               # API token CRUD
│   ├── sse.rs                  # SSE handler
│   ├── hls.rs                  # HLS proxy endpoints
│   └── health.rs               # healthz, readyz
```

---

## 4. Testing Plan

### 4.1 Test Infrastructure

Extend `TestEnv` to include the full server stack:

```rust
struct FullTestEnv {
    /// Server HTTP address (for API calls)
    http_addr: SocketAddr,
    /// Server QUIC address (for camera connections)
    quic_addr: SocketAddr,
    /// HTTP client
    client: reqwest::Client,
    /// Pre-created session cookie for authenticated requests
    session_cookie: String,
    /// Everything from previous TestEnv
    registry: Arc<RoutingRegistry>,
    db: Arc<SqliteDatabase>,
    redis: Arc<RedisManager>,
    cancel: CancellationToken,
}

impl FullTestEnv {
    /// Start a full server (QUIC + HTTP), create the operator account,
    /// log in, return a ready-to-use test environment.
    async fn setup() -> Self;
    async fn teardown(self);

    /// Helper: make an authenticated GET request.
    async fn get(&self, path: &str) -> reqwest::Response;
    /// Helper: make an authenticated POST request with JSON body.
    async fn post(&self, path: &str, body: impl Serialize) -> reqwest::Response;
    /// etc.
}
```

### 4.2 Unit Tests — EgressHandle

**Location:** `server-core/src/egress/handle.rs`

| Test | Description |
|------|-------------|
| `create_produces_sdp_answer` | Create EgressHandle with a valid SDP offer → returns non-empty SDP answer |
| `sdp_answer_has_video_audio` | SDP answer contains video and audio media lines |
| `sdp_answer_has_data_channels` | SDP answer contains data channel negotiation |

### 4.3 Unit Tests — Data Channel

**Location:** `server-core/src/egress/data_channel.rs`

| Test | Description |
|------|-------------|
| `parse_client_mode_live` | `{"type":"client_mode","mode":"live"}` → `ClientMode::Live` |
| `parse_client_mode_playback` | `{"type":"client_mode","mode":"playback"}` → `ClientMode::Playback` |
| `parse_client_mode_map` | `{"type":"client_mode","mode":"map"}` → `ClientMode::Map` |
| `parse_unknown_message` | Unknown type field → error |

### 4.4 Unit Tests — SSE Event Bus

**Location:** `server-core/src/sse.rs`

| Test | Description |
|------|-------------|
| `subscribe_and_receive` | Subscribe, publish CameraOnline → subscriber receives it |
| `multiple_subscribers` | 3 subscribers, publish 1 event → all 3 receive it |
| `events_scoped_to_user` | Subscribe user A, publish event for user B → user A does not receive |
| `subscribe_no_events` | Subscribe, no publish → receiver blocks (does not receive) |

### 4.5 Unit Tests — Auth Middleware

**Location:** `server-core/src/api/auth.rs`

| Test | Description |
|------|-------------|
| `no_auth_returns_401` | Request with no cookie and no Bearer header → 401 |
| `invalid_bearer_returns_401` | Request with `Authorization: Bearer garbage` → 401 |
| `invalid_cookie_returns_401` | Request with expired or unknown session cookie → 401 |
| `valid_bearer_passes` | Create API token, request with correct Bearer → 200, AuthUser extracted |
| `valid_cookie_passes` | Create session, request with cookie → 200, AuthUser extracted |
| `bearer_takes_precedence` | Both cookie and Bearer present → Bearer is checked first |

### 4.6 Unit Tests — Session Manager

**Location:** `server-core/src/egress/sessions.rs`

| Test | Description |
|------|-------------|
| `register_and_list` | Register session → list returns it |
| `teardown_removes` | Register then teardown → list empty |
| `teardown_by_device` | Register 2 sessions for device A, 1 for device B → teardown_by_device(A) → only B remains |
| `teardown_nonexistent` | Teardown unknown session_id → no error |

### 4.7 Integration Tests — Auth Flow

**Location:** `server-core/tests/api_auth.rs`

| Test | Description |
|------|-------------|
| `login_with_initial_password` | Boot server, login with printed initial password → 200, Set-Cookie present |
| `login_wrong_password` | Login with bad password → 401 |
| `logout_clears_session` | Login, logout → session cookie invalidated, subsequent request → 401 |
| `change_password` | Login, change password → old password no longer works, new one does |
| `protected_endpoint_requires_auth` | GET /api/v1/cameras without auth → 401 |
| `api_token_auth` | Create API token, use Bearer header → 200 |

### 4.8 Integration Tests — Camera API

**Location:** `server-core/tests/api_cameras.rs`

| Test | Description |
|------|-------------|
| `list_cameras_empty` | No cameras enrolled → 200, empty array |
| `list_cameras_with_enrolled` | Enroll a camera (via full QUIC enrollment), list → 1 camera with correct fields |
| `list_cameras_shows_online_status` | Enroll and connect camera → list shows `online: true`. Disconnect → `online: false` |
| `get_camera` | Enroll camera → GET by device_id → 200, correct fields |
| `get_camera_not_found` | GET unknown device_id → 404 |
| `update_camera_display_name` | PATCH display_name → 200, subsequent GET shows new name |
| `update_camera_notes` | PATCH notes → 200 |
| `delete_camera` | DELETE → 200, camera unregistered, subsequent GET → 404 |
| `enroll_returns_jwt` | POST /cameras/enroll → 200, response has `token` (valid JWT) and `expires_at` |
| `enroll_with_display_name` | POST with display_name → JWT claims contain display_name |
| `enroll_with_wifi` | POST with wifi credentials → JWT claims contain wifi array |

### 4.9 Integration Tests — Watch / WebRTC Session

**Location:** `server-core/tests/api_watch.rs`

| Test | Description |
|------|-------------|
| `create_session_returns_answer` | Enroll + connect camera, POST /watch with SDP offer → 200 with session_id + sdp_answer |
| `create_session_camera_not_found` | POST /watch with unknown device_id → 404 |
| `create_session_camera_offline` | Enroll camera but don't connect → POST /watch → 404 (or 409 "camera offline") |
| `teardown_session` | Create session, DELETE /session/{id} → 200, session removed |
| `teardown_unknown_session` | DELETE /session/unknown → 404 |

### 4.10 Integration Tests — API Tokens

**Location:** `server-core/tests/api_tokens.rs`

| Test | Description |
|------|-------------|
| `create_token` | POST /tokens → 200, response has token_id + raw_token |
| `create_token_with_label` | POST with label → list shows label |
| `list_tokens` | Create 3 tokens → list returns 3 |
| `revoke_token` | Create then DELETE → list no longer includes it |
| `revoked_token_rejected` | Create token, revoke, use in Bearer header → 401 |
| `raw_token_shown_once` | raw_token is in create response but NOT in list response |

### 4.11 Integration Tests — SSE

**Location:** `server-core/tests/api_sse.rs`

| Test | Description |
|------|-------------|
| `sse_camera_online` | Subscribe to SSE, connect camera → receive `camera_online` event with device_id |
| `sse_camera_offline` | Camera connected, subscribe to SSE, disconnect camera → receive `camera_offline` event |
| `sse_scoped_to_user` | Two users (server-multi or simulate), camera belongs to user A → user B does not receive events |
| `sse_reconnect` | Subscribe, disconnect SSE, reconnect → new events still arrive |

### 4.12 Integration Tests — HLS Endpoints

**Location:** `server-core/tests/api_hls.rs`

| Test | Description |
|------|-------------|
| `get_manifest` | Connect camera, push manifest via upload stream → GET /hls/{id}/playlist.m3u8 → 200, body matches |
| `get_manifest_no_camera` | GET /hls/unknown/playlist.m3u8 → 404 |
| `get_segment_triggers_upload` | GET /hls/{id}/{seg}.m4s → server sends upload_segment to camera, camera uploads, client receives bytes |
| `get_segment_coalesced` | Two concurrent GET for same segment → only one upload_segment command, both receive data |
| `get_segment_from_buffer` | GET segment (triggers upload), GET same segment again within 60s → served from buffer, no second upload |
| `get_segment_evicted` | Camera sends segment_upload_failed(evicted) → GET returns 404 |
| `get_init` | GET /hls/{id}/init.mp4 → triggers upload_init, returns bytes |

### 4.13 Integration Tests — Health Endpoints

**Location:** `server-core/tests/api_health.rs`

| Test | Description |
|------|-------------|
| `healthz_returns_200` | GET /healthz → 200, body "ok" |
| `healthz_no_auth` | GET /healthz without auth → 200 (no auth required) |
| `readyz_all_ok` | Server fully initialized → 200, status "ready", all components "ok" |
| `readyz_redis_unavailable` | Redis disconnected → 200, status "ready", redis "unavailable" |

### 4.14 Integration Tests — Full Live Streaming Path

**Location:** `server-core/tests/live_streaming.rs`

End-to-end test: camera → server → observer.

| Test | Description |
|------|-------------|
| `video_frame_reaches_observer` | MockCamera sends video frames → EgressHandle (created via /watch) receives them as RTP (verify via str0m test utilities or by checking broadcast receiver) |
| `audio_frame_reaches_observer` | Same for audio |
| `telemetry_reaches_data_channel` | MockCamera sends telemetry → EgressHandle sends MessagePack on telemetry data channel |
| `client_mode_live_starts_camera` | Observer sends client_mode: live → camera receives start_video + start_audio commands |
| `client_mode_playback_stops_camera` | Observer in live mode switches to playback → camera receives stop_video + stop_audio (if last observer) |
| `camera_disconnect_closes_sessions` | Camera disconnects → all associated WebRTC sessions are torn down |

### 4.15 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All crates compile |
| Unit tests | `cargo test -p server-core` (non-ignored) | Pass |
| Integration tests | `cargo test -p server-core -- --ignored` | Pass (requires Redis) |
| server-solo runs | `cargo run -p server-solo` | Boots, listens on QUIC + HTTP |
| Clippy | `cargo clippy -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 5. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test` passes all tests
- [ ] `server-solo` boots and listens on QUIC (4433) and HTTP (3000)
- [ ] Login with initial password works, returns session cookie
- [ ] Protected endpoints reject unauthenticated requests with 401
- [ ] API token auth works via Bearer header
- [ ] Camera enrollment endpoint returns a valid JWT
- [ ] Camera list shows online/offline status
- [ ] Camera CRUD (get, update, delete) works
- [ ] Camera delete triggers unregistration flow
- [ ] POST /watch creates a WebRTC session and returns SDP answer
- [ ] DELETE /session tears down the session
- [ ] SSE stream delivers camera_online/camera_offline events
- [ ] Video frames flow from MockCamera through broadcast to EgressHandle
- [ ] Audio frames flow correctly
- [ ] Telemetry flows from camera to data channel (MessagePack)
- [ ] client_mode changes trigger start/stop commands to camera
- [ ] HLS manifest is served from in-memory cache
- [ ] HLS segment request triggers camera upload and returns bytes
- [ ] Segment request coalescing works (one upload for multiple clients)
- [ ] /healthz always returns 200
- [ ] /readyz returns component status
- [ ] Static files served from ui/build/ with SPA fallback
- [ ] Camera disconnect tears down all associated WebRTC sessions
- [ ] `CLAUDE.md` updated with egress + API module structure
