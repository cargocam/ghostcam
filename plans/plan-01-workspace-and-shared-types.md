# Plan 1: Workspace Restructure & Shared Types

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Nothing
**Unlocks:** All other plans

---

## 1. Goal

Establish the new workspace structure, delete old crate contents, create all crate shells, and implement the shared `ghostcam` library with wire protocol types, stream framing, telemetry schema, PKI utilities, and the `Database` trait definition in `server-core`.

This plan produces no runnable binaries — only libraries with comprehensive unit tests.

---

## 2. Workspace Structure

Delete the contents of the existing `ghostcam/`, `camera/`, `server/` crates. Create the new layout:

```
ghostcam/
├── Cargo.toml              # workspace root
├── ghostcam/               # shared library crate
│   ├── Cargo.toml
│   └── src/
│       ├── lib.rs
│       ├── wire/
│       │   ├── mod.rs
│       │   ├── alert.rs        # Alert enum + variants
│       │   ├── command.rs      # Command enum + variants
│       │   ├── framing.rs      # Length-prefixed stream codec
│       │   └── handshake.rs    # Handshake alert (first message)
│       ├── telemetry.rs        # TelemetryDatagram schema
│       ├── pki.rs              # P-256 key gen, cert creation, CSR, fingerprint
│       ├── types.rs            # DeviceId, UserId, SessionId, newtypes
│       └── config.rs           # Port constants, keepalive intervals, capacities
├── camera/                 # camera firmware crate (stub)
│   ├── Cargo.toml
│   └── src/
│       └── main.rs             # fn main() { println!("camera stub"); }
├── server-core/            # shared server logic crate
│   ├── Cargo.toml
│   └── src/
│       ├── lib.rs
│       └── db.rs               # Database trait definition
├── server-solo/            # single-user server binary (stub)
│   ├── Cargo.toml
│   └── src/
│       └── main.rs             # fn main() { println!("server-solo stub"); }
├── server-multi/           # multi-user server binary (stub)
│   ├── Cargo.toml
│   └── src/
│       └── main.rs             # fn main() { println!("server-multi stub"); }
├── ui/                     # Svelte SPA (untouched in this plan)
├── test-data/              # Retained (test.h264)
├── specs/                  # Retained
└── plans/                  # Implementation plans
```

### 2.1 Workspace Cargo.toml

```toml
[workspace]
members = ["ghostcam", "camera", "server-core", "server-solo", "server-multi"]
resolver = "2"

[workspace.dependencies]
# Serialization
serde = { version = "1", features = ["derive"] }
serde_json = "1"
rmp-serde = "1"

# Crypto / TLS
rcgen = "0.13"
rustls = { version = "0.23", features = ["ring", "std"], default-features = false }
ring = "0.17"

# Async
tokio = { version = "1", features = ["full"] }

# Error handling
anyhow = "1"
thiserror = "2"

# Logging
tracing = "0.1"

# Testing
bytes = "1"
```

### 2.2 Crate dependencies

**`ghostcam`** (library):
- `serde`, `serde_json`, `rmp-serde` — serialization for alerts, commands, telemetry
- `rcgen` — P-256 key pair generation, CSR creation, certificate signing
- `rustls` — certificate parsing, fingerprint extraction
- `ring` — HMAC-SHA256 (for API token verification trait)
- `bytes` — efficient byte buffer handling for framing
- `thiserror` — typed errors for library code
- `tracing` — structured logging

**`server-core`** (library):
- `ghostcam` (path dependency)
- `tokio` — async trait bounds
- `anyhow` — Result types in trait definitions

**`camera`**, **`server-solo`**, **`server-multi`** (stubs):
- Minimal dependencies, just enough to compile

---

## 3. Implementation Details

### 3.1 Type Newtypes (`types.rs`)

```rust
/// Server-assigned UUID for an enrolled camera.
pub struct DeviceId(pub String);

/// User identifier. In server-solo this is always the single operator.
/// In server-multi this is a UUID from the users table.
pub struct UserId(pub String);

/// Cryptographically random session token.
pub struct SessionId(pub String);

/// API token identifier (safe to expose in management UI).
pub struct TokenId(pub String);

/// SHA-256 fingerprint of a certificate's public key, hex-encoded.
pub struct CertFingerprint(pub String);

/// Monotonically increasing command sequence number.
pub struct Seq(pub u64);
```

All newtypes derive `Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize`. `Display` and `FromStr` where appropriate.

### 3.2 Alerts (`wire/alert.rs`)

Tagged enum with `#[serde(tag = "type", rename_all = "snake_case")]`:

```rust
pub enum Alert {
    Handshake {
        protocol_version: u32,
        fw_version: String,
        streams: Vec<StreamKind>,
    },
    CapabilityUpdate {
        streams: Vec<StreamKind>,
    },
    RecordingSegment {
        device_id: String,
        segment_id: String,
        start_ts: u64,
        end_ts: u64,
        size_bytes: u64,
    },
    SegmentEvicted {
        segment_id: String,
    },
    SegmentUploaded {
        seq: u64,
        segment_id: String,
    },
    SegmentUploadFailed {
        seq: u64,
        segment_id: String,
        reason: UploadFailReason,
    },
    Ack {
        cmd: String,
        seq: u64,
    },
    Csr {
        csr_pem: String,
    },
    StorageFull {
        free_bytes: u64,
    },
    StorageResumed {
        free_bytes: u64,
    },
    UpdateApplying {
        version: String,
    },
    UpdateSucceeded {
        version: String,
    },
    UpdateFailed {
        version_attempted: String,
        version_current: String,
        reason: UpdateFailReason,
    },
    Networks {
        networks: Vec<NetworkEntry>,
    },
}

pub enum StreamKind { Video, Audio, Telemetry }

pub enum UploadFailReason { Evicted, NotFound, IoError }

pub enum UpdateFailReason { Watchdog, HashMismatch, DownloadFailed }

pub struct NetworkEntry {
    pub ssid: String,
    pub signal_dbm: Option<i8>,
}
```

### 3.3 Commands (`wire/command.rs`)

Tagged enum with `#[serde(tag = "type", rename_all = "snake_case")]`:

```rust
pub enum Command {
    StartVideo { seq: u64 },
    StopVideo { seq: u64 },
    StartAudio { seq: u64 },
    StopAudio { seq: u64 },
    UploadSegment { seq: u64, segment_id: String },
    UploadInit { seq: u64 },
    Reboot { seq: u64 },
    NetworkConfig { seq: u64, ssid: String, psk: String },
    RemoveNetwork { seq: u64, ssid: String },
    ListNetworks { seq: u64 },
    UpdateAvailable {
        seq: u64,
        version: String,
        url: String,
        sha256: String,
        #[serde(default)]
        force: bool,
    },
    CertRefresh {
        seq: u64,
        cert_pem: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        ca_pem: Option<String>,
    },
    Unregister { seq: u64 },
}
```

### 3.4 Stream Framing (`wire/framing.rs`)

Length-prefixed codec for reading/writing messages on QUIC streams. 4-byte big-endian length prefix, 4MB max frame size.

```rust
const MAX_FRAME_SIZE: u32 = 4 * 1024 * 1024; // 4 MB

/// Write a length-prefixed frame to an async writer.
pub async fn write_frame(writer: &mut W, data: &[u8]) -> Result<()>
where W: AsyncWrite + Unpin;

/// Read a length-prefixed frame from an async reader.
/// Returns None on clean stream close (EOF before length prefix).
/// Returns Err on truncated reads or oversized frames.
pub async fn read_frame(reader: &mut R) -> Result<Option<Vec<u8>>>
where R: AsyncRead + Unpin;

/// Convenience: serialize a value as JSON and write as a length-prefixed frame.
pub async fn write_json<T: Serialize>(writer: &mut W, value: &T) -> Result<()>
where W: AsyncWrite + Unpin;

/// Convenience: read a length-prefixed frame and deserialize from JSON.
pub async fn read_json<T: DeserializeOwned>(reader: &mut R) -> Result<Option<T>>
where R: AsyncRead + Unpin;
```

Uses `tokio::io::{AsyncRead, AsyncWrite}` traits so it works with any async stream (QUIC, TCP, in-memory for tests).

### 3.5 Telemetry Schema (`telemetry.rs`)

MessagePack-encoded telemetry datagram. All fields except `ts` are optional.

```rust
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TelemetryDatagram {
    pub ts: u64,                          // Unix ms, camera clock
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sig: Option<i8>,                  // WiFi signal (dBm)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temp: Option<u32>,                // SoC temp (°C)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fps: Option<f32>,                 // Capture frame rate
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kbps: Option<u32>,                // Video bitrate
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<u32>,                 // CPU usage (%)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<u32>,                 // Memory (MB)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uptime: Option<u32>,              // Uptime (seconds)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lat: Option<f64>,                 // GPS latitude
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lon: Option<f64>,                 // GPS longitude
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alt: Option<f32>,                 // GPS altitude (metres)
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gps_fix: Option<u8>,             // 0=none, 1=2D, 2=3D
}
```

Provide `encode()` → `Vec<u8>` (MessagePack) and `decode(&[u8])` → `Result<Self>` helpers.

Also provide a helper for encoding/decoding a `Vec<TelemetryDatagram>` as a MessagePack array (used by the telemetry buffer upload stream).

### 3.6 Telemetry Diffing

Threshold-based diff logic for the camera-side telemetry loop. This is shared library code because thresholds are part of the protocol contract.

```rust
pub struct TelemetryThresholds {
    pub sig: i8,        // 5 dBm
    pub temp: u32,      // 1°C
    pub fps: f32,       // 2 fps
    pub kbps: u32,      // 500 Kbps
    pub cpu: u32,       // 5%
    pub mem: u32,       // 5 MB
    pub lat_lon: f64,   // 0.0001° (~11m)
    pub alt: f32,       // 10m
}

impl Default for TelemetryThresholds { /* spec values */ }

/// Returns true if `current` exceeds thresholds compared to `previous`.
pub fn exceeds_threshold(
    previous: &TelemetryDatagram,
    current: &TelemetryDatagram,
    thresholds: &TelemetryThresholds,
) -> bool;
```

### 3.7 PKI Utilities (`pki.rs`)

All operations use P-256 (ECDSA). Provides building blocks — the enrollment *flow* that uses these is Plan 4.

```rust
/// Generate a new P-256 key pair. Returns (KeyPair, private key DER bytes).
pub fn generate_key_pair() -> Result<(rcgen::KeyPair, Vec<u8>)>;

/// Create a self-signed CA certificate (for server-solo Instance CA).
/// validity_years: typically 20.
pub fn create_self_signed_ca(
    cn: &str,
    validity_years: u32,
) -> Result<(rcgen::Certificate, rcgen::KeyPair)>;

/// Create a self-signed server TLS certificate.
/// validity_years: typically 20.
pub fn create_self_signed_server_cert(
    cn: &str,
    validity_years: u32,
) -> Result<(rcgen::Certificate, rcgen::KeyPair)>;

/// Create a self-signed device identity certificate.
/// Subject CN is "ghostcam-device" (static). Validity 20 years.
pub fn create_device_cert() -> Result<(rcgen::Certificate, rcgen::KeyPair)>;

/// Create a Certificate Signing Request from a key pair.
/// Subject CN will be set to the provided value.
pub fn create_csr(cn: &str, key_pair: &rcgen::KeyPair) -> Result<String>;

/// Sign a CSR with a CA, issuing a user association certificate.
/// Subject CN={device_id}. No expiry (permanent). Key usage: digitalSignature, clientAuth.
pub fn sign_csr(
    csr_pem: &str,
    ca_cert: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    device_id: &str,
) -> Result<String>;

/// Compute SHA-256 fingerprint of a certificate's public key (hex-encoded).
pub fn cert_fingerprint(cert_der: &[u8]) -> Result<CertFingerprint>;

/// Compute SHA-256 fingerprint of a DER-encoded public key (hex-encoded).
/// Used for TOFU server TLS pinning.
pub fn pubkey_fingerprint(pubkey_der: &[u8]) -> String;

/// Parse a PEM certificate and extract the serial number (hex-encoded).
pub fn cert_serial_number(cert_pem: &str) -> Result<String>;

/// Load a PEM certificate from bytes.
pub fn parse_cert_pem(pem: &str) -> Result<Vec<u8>>;

/// Load a PEM private key from bytes.
pub fn parse_key_pem(pem: &str) -> Result<Vec<u8>>;
```

### 3.8 Config Constants (`config.rs`)

```rust
pub const QUIC_PORT: u16 = 4433;
pub const HTTP_PORT: u16 = 3000;
pub const BROADCAST_CAPACITY: usize = 512;
pub const MAX_FRAME_SIZE: u32 = 4 * 1024 * 1024; // 4 MB
pub const KEEPALIVE_INTERVAL_SECS: u64 = 15;
pub const DISCONNECT_TIMEOUT_SECS: u64 = 30;
pub const TELEMETRY_POLL_INTERVAL_SECS: u64 = 2;
pub const TELEMETRY_HEARTBEAT_INTERVAL_SECS: u64 = 30;
pub const TELEMETRY_BUFFER_CAP: usize = 100_000;
pub const SEGMENT_DURATION_SECS: u64 = 10;
pub const SEGMENT_BUFFER_TTL_SECS: u64 = 60;
pub const ENROLLMENT_TOKEN_TTL_SECS: u64 = 600; // 10 minutes
pub const SESSION_TTL_DAYS: u64 = 30;
pub const TELEMETRY_RETENTION_HOURS: u64 = 72;
pub const REVOCATION_CACHE_REFRESH_SECS: u64 = 60;
pub const RECONNECT_BACKOFF_INITIAL_SECS: u64 = 1;
pub const RECONNECT_BACKOFF_MAX_SECS: u64 = 30;
pub const PROTOCOL_VERSION: u32 = 1;
```

### 3.9 Database Trait (`server-core/src/db.rs`)

Async trait defining the interface both server variants implement. Uses `anyhow::Result` for simplicity at this layer.

```rust
#[async_trait]
pub trait Database: Send + Sync + 'static {
    // --- Camera operations ---
    async fn get_camera_by_fingerprint(&self, fingerprint: &CertFingerprint) -> Result<Option<CameraRecord>>;
    async fn get_camera(&self, device_id: &DeviceId) -> Result<Option<CameraRecord>>;
    async fn list_cameras(&self, user_id: &UserId) -> Result<Vec<CameraRecord>>;
    async fn create_camera(&self, record: &NewCameraRecord) -> Result<CameraRecord>;
    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()>;
    async fn delete_camera(&self, device_id: &DeviceId) -> Result<()>;
    async fn update_last_seen(&self, device_id: &DeviceId) -> Result<()>;

    // --- Enrollment tokens ---
    async fn create_enrollment_token(&self, token: &NewEnrollmentToken) -> Result<()>;
    async fn claim_enrollment_token(&self, jti: &str, device_id: &DeviceId) -> Result<bool>;
    async fn cleanup_expired_tokens(&self) -> Result<u64>;

    // --- Sessions ---
    async fn create_session(&self, session: &NewSession) -> Result<()>;
    async fn get_session(&self, session_id: &SessionId) -> Result<Option<SessionRecord>>;
    async fn delete_session(&self, session_id: &SessionId) -> Result<()>;
    async fn extend_session(&self, session_id: &SessionId) -> Result<()>;
    async fn cleanup_expired_sessions(&self) -> Result<u64>;

    // --- API tokens ---
    async fn create_api_token(&self, token: &NewApiToken) -> Result<()>;
    async fn list_api_tokens(&self, user_id: &UserId) -> Result<Vec<ApiTokenRecord>>;
    async fn verify_api_token(&self, token_hash: &str) -> Result<Option<ApiTokenRecord>>;
    async fn delete_api_token(&self, token_id: &TokenId) -> Result<()>;

    // --- Auth (server-solo: password check; server-multi: user CRUD) ---
    async fn verify_password(&self, user_id: &UserId, password_hash: &str) -> Result<bool>;
    async fn set_password(&self, user_id: &UserId, password_hash: &str) -> Result<()>;

    // --- Server config ---
    async fn get_hmac_secret(&self) -> Result<Vec<u8>>;
}
```

Supporting types (also in `server-core`):

```rust
pub struct CameraRecord {
    pub device_id: DeviceId,
    pub user_id: UserId,
    pub cert_fingerprint: CertFingerprint,
    pub display_name: String,
    pub enrolled_at: u64,
    pub last_seen_at: Option<u64>,
    pub notes: Option<String>,
}

pub struct NewCameraRecord {
    pub user_id: UserId,
    pub cert_fingerprint: CertFingerprint,
    pub display_name: String,
}

pub struct CameraUpdate {
    pub display_name: Option<String>,
    pub notes: Option<String>,
}

pub struct NewEnrollmentToken {
    pub jti: String,
    pub user_id: UserId,
    pub expires_at: u64,
}

pub struct NewSession {
    pub session_id: SessionId,
    pub user_id: UserId,
    pub user_agent: Option<String>,
    pub ip_address: Option<String>,
}

pub struct SessionRecord {
    pub session_id: SessionId,
    pub user_id: UserId,
    pub created_at: u64,
    pub expires_at: u64,
    pub last_active_at: Option<u64>,
}

pub struct NewApiToken {
    pub token_id: TokenId,
    pub user_id: UserId,
    pub token_hash: String,
    pub label: String,
    pub expires_at: Option<u64>,
}

pub struct ApiTokenRecord {
    pub token_id: TokenId,
    pub user_id: UserId,
    pub label: String,
    pub created_at: u64,
    pub expires_at: Option<u64>,
    pub last_used_at: Option<u64>,
}
```

---

## 4. Files to Delete

Remove all source files from the old crate structure. The following are deleted:

- `ghostcam/src/**` — all old shared library code
- `camera/src/**` — all old camera code
- `server/` — entire old server crate (directory removed)
- Old `Cargo.toml` workspace members referencing `server`
- `Dockerfile` — will be rewritten in Plan 12
- `docker-compose.yml` — will be rewritten in Plan 12
- `.github/workflows/ci.yml` — will be rewritten in Plan 12
- `camera/launch-cameras.sh` — will be rewritten when camera is implemented

Retained:
- `test-data/test.h264`
- `ui/` — untouched
- `specs/` — untouched
- `plans/` — this directory
- `.gitignore`

---

## 5. Testing Plan

### 5.1 Unit Tests — Wire Protocol Serialization

**Location:** `ghostcam/src/wire/` test modules

| Test | Description |
|------|-------------|
| `alert_handshake_roundtrip` | Serialize a `Handshake` alert to JSON and deserialize back; verify all fields match |
| `alert_all_variants_roundtrip` | Roundtrip test for every `Alert` variant; verify serde tag is correct snake_case |
| `alert_unknown_type_rejected` | Deserialize JSON with an unknown `type` field; verify it returns an error |
| `command_all_variants_roundtrip` | Roundtrip test for every `Command` variant |
| `command_optional_fields` | `UpdateAvailable` with and without `force`; `CertRefresh` with and without `ca_pem` |
| `command_seq_preserved` | Verify `seq` survives roundtrip for all command variants |
| `stream_kind_serde` | `StreamKind::Video` serializes as `"video"`, etc. |
| `upload_fail_reason_serde` | All `UploadFailReason` variants serialize correctly |
| `update_fail_reason_serde` | All `UpdateFailReason` variants serialize correctly |
| `network_entry_with_signal` | `NetworkEntry` with `signal_dbm: Some(-62)` |
| `network_entry_without_signal` | `NetworkEntry` with `signal_dbm: None` serializes without the field |

### 5.2 Unit Tests — Stream Framing

**Location:** `ghostcam/src/wire/framing.rs` test module

| Test | Description |
|------|-------------|
| `write_read_roundtrip` | Write a frame, read it back from an in-memory buffer; verify contents match |
| `write_read_empty_frame` | Write a zero-length frame, read it back; verify empty payload |
| `write_read_max_size_frame` | Write a frame at exactly `MAX_FRAME_SIZE` bytes; verify it succeeds |
| `read_oversized_frame_rejected` | Write a length prefix of `MAX_FRAME_SIZE + 1`, attempt to read; verify error |
| `read_eof_returns_none` | Read from an empty buffer; verify `None` is returned |
| `read_truncated_length_returns_error` | Write only 2 bytes of a 4-byte length prefix; verify error |
| `read_truncated_payload_returns_error` | Write a length prefix for 100 bytes but only 50 bytes of payload; verify error |
| `write_read_multiple_frames` | Write 3 frames sequentially, read all 3 back; verify ordering and contents |
| `write_read_json_roundtrip` | Use `write_json` / `read_json` with an `Alert`; verify deserialized value matches |
| `write_read_json_command` | Use `write_json` / `read_json` with a `Command`; verify roundtrip |

### 5.3 Unit Tests — Telemetry

**Location:** `ghostcam/src/telemetry.rs` test module

| Test | Description |
|------|-------------|
| `datagram_msgpack_roundtrip` | Encode a full datagram to MessagePack and decode; verify all fields |
| `datagram_sparse_roundtrip` | Encode a datagram with only `ts` and `cpu`; decode and verify absent fields are `None` |
| `datagram_gps_fields` | Encode/decode with all GPS fields present |
| `datagram_no_gps` | Encode/decode with all GPS fields absent |
| `datagram_array_roundtrip` | Encode a `Vec<TelemetryDatagram>` and decode; verify length and contents |
| `datagram_empty_array` | Encode/decode empty vec |
| `threshold_cpu_triggers` | Previous CPU=20, current CPU=26 (diff=6 > threshold 5); verify `exceeds_threshold` returns true |
| `threshold_cpu_within` | Previous CPU=20, current CPU=24 (diff=4 < threshold 5); verify returns false |
| `threshold_temp_triggers` | Previous temp=50, current temp=52; verify triggers |
| `threshold_gps_triggers` | Previous lat=37.7749, current lat=37.7750 (diff=0.0001); verify triggers |
| `threshold_gps_within` | Previous lat=37.7749, current lat=37.77495; verify does not trigger |
| `threshold_multiple_fields` | Only one field exceeds; verify triggers |
| `threshold_no_previous_gps` | Previous has no GPS, current has GPS; verify triggers (any change from None) |
| `threshold_no_current_gps` | Previous has GPS, current has no GPS; verify triggers |
| `threshold_uptime_any_change` | Uptime changed by 1; verify triggers |
| `threshold_nothing_changed` | All fields identical; verify does not trigger |

### 5.4 Unit Tests — PKI

**Location:** `ghostcam/src/pki.rs` test module

| Test | Description |
|------|-------------|
| `generate_key_pair_produces_valid_p256` | Generate a key pair; verify it is P-256 by checking the algorithm |
| `create_ca_cert` | Create a self-signed CA cert; verify it is self-signed, has `keyCertSign` usage, CN matches |
| `create_server_cert` | Create a server cert; verify `serverAuth` usage |
| `create_device_cert_cn` | Create a device cert; verify CN is `ghostcam-device` |
| `csr_roundtrip` | Generate key, create CSR, verify CSR PEM can be parsed |
| `sign_csr_produces_cert` | Create CA, generate camera key, create CSR, sign it; verify resulting cert is valid, CN matches device_id, issuer matches CA |
| `sign_csr_client_auth` | Verify signed cert has `digitalSignature` and `clientAuth` key usage |
| `cert_fingerprint_deterministic` | Compute fingerprint of same cert twice; verify identical |
| `cert_fingerprint_different_certs` | Compute fingerprints of two different certs; verify different |
| `pubkey_fingerprint_deterministic` | Compute pubkey fingerprint twice; verify identical |
| `cert_serial_number_extraction` | Sign a CSR, extract serial number from resulting cert; verify non-empty hex string |
| `parse_cert_pem_valid` | Parse a valid PEM cert string; verify success |
| `parse_cert_pem_invalid` | Parse garbage; verify error |

### 5.5 Unit Tests — Types

**Location:** `ghostcam/src/types.rs` test module

| Test | Description |
|------|-------------|
| `device_id_display` | `DeviceId("abc".into()).to_string()` == `"abc"` |
| `device_id_equality` | Two `DeviceId` with same value are equal |
| `device_id_hash` | Can be used as a `HashMap` key |
| `all_newtypes_clone` | All newtypes can be cloned |
| `all_newtypes_serde` | All newtypes roundtrip through JSON |

### 5.6 Integration Tests — Database Trait

**Location:** `server-core/tests/` (or `server-core/src/db.rs` doc tests)

No integration tests in this plan — the trait has no implementation yet. Verify only that the trait compiles and is object-safe:

| Test | Description |
|------|-------------|
| `trait_is_object_safe` | `let _: Box<dyn Database>` compiles (may require a test that references the type) |

### 5.7 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All 5 crates compile cleanly |
| No warnings | `cargo build 2>&1 \| grep warning` | No warnings (or only from dependencies) |
| All tests pass | `cargo test` | All unit tests pass |
| Clippy clean | `cargo clippy -- -D warnings` | No clippy warnings |
| Format check | `cargo fmt --check` | No formatting issues |
| Stub binaries run | `cargo run -p camera`, `cargo run -p server-solo`, `cargo run -p server-multi` | Each prints its stub message and exits |

---

## 6. Validation Checklist

After completing this plan, verify the following before moving to Plan 2:

- [ ] Old `server/` directory is removed
- [ ] Old `ghostcam/src/` contents are replaced with new module structure
- [ ] Old `camera/src/` contents are replaced with stub
- [ ] `Cargo.toml` workspace members list: `ghostcam`, `camera`, `server-core`, `server-solo`, `server-multi`
- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test` passes all tests listed above
- [ ] `cargo clippy -- -D warnings` is clean
- [ ] `cargo fmt --check` is clean
- [ ] Every `Alert` variant serializes with the correct `"type"` tag matching the spec
- [ ] Every `Command` variant serializes with the correct `"type"` tag matching the spec
- [ ] Telemetry MessagePack encoding produces compact binary (no string keys bloat)
- [ ] PKI functions produce valid P-256 certificates parseable by rustls
- [ ] CSR signing produces a cert whose issuer matches the CA
- [ ] `Database` trait is defined in `server-core` and is object-safe
- [ ] `test-data/test.h264` is retained
- [ ] `ui/` directory is untouched
- [ ] `specs/` and `plans/` directories are present
