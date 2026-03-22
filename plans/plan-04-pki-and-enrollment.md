# Plan 4: Server Core — PKI & Enrollment

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 1 (shared types, PKI utilities), Plan 2 (database), Plan 3 (ingest pipeline, QUIC accept loop)
**Unlocks:** Plan 5 (Redis — revocation wiring), Plan 6 (HTTP API — enrollment endpoint)

---

## 1. Goal

Implement the server-side PKI lifecycle: Instance CA bootstrap for `server-solo`, intermediate CA loading for `server-multi`, the enrollment handler in the QUIC accept loop, enrollment JWT generation and verification, CSR signing, certificate delivery via `cert_refresh`, unregistration flow, and the revocation cache interface. Also implement server TLS certificate generation for `server-solo`.

After this plan, a camera can connect in enrollment mode, present a CSR, receive a signed user association certificate, and be marked enrolled in the database. A server operator can unregister a camera, which clears enrollment state and revokes the certificate. The revocation cache is stubbed (always empty) — Plan 5 wires it to Redis.

---

## 2. Crate Changes

### 2.1 New Dependencies

**Workspace `Cargo.toml`** — add:
```toml
[workspace.dependencies]
jsonwebtoken = "9"
```

**`server-core/Cargo.toml`** — add:
```toml
[dependencies]
jsonwebtoken.workspace = true
uuid.workspace = true
```

---

## 3. Implementation Details

### 3.1 CA Manager (`server-core/src/pki/ca.rs`)

Holds the CA certificate and key in memory for the lifetime of the server process. Both server variants use the same interface; they differ only in how the CA material is loaded.

```rust
pub struct CaManager {
    /// The CA certificate (PEM).
    ca_cert_pem: String,
    /// The CA certificate (DER, for TLS config).
    ca_cert_der: Vec<u8>,
    /// The CA key pair (for signing CSRs and JWTs).
    ca_key_pair: rcgen::KeyPair,
    /// The rcgen Certificate (for signing CSRs).
    ca_cert: rcgen::Certificate,
    /// ES256 encoding key (for signing JWTs).
    jwt_encoding_key: jsonwebtoken::EncodingKey,
}

impl CaManager {
    /// Load from existing PEM files (for server-solo subsequent startups
    /// and server-multi always).
    pub fn from_pem(cert_pem: &str, key_pem: &str) -> Result<Self>;

    /// Generate a new self-signed Instance CA (server-solo first startup).
    /// Returns (CaManager, cert_pem, key_pem) — caller persists the PEM files.
    pub fn generate_instance_ca() -> Result<(Self, String, String)>;

    /// Get the CA certificate PEM (for delivering to cameras during enrollment).
    pub fn ca_cert_pem(&self) -> &str;

    /// Get the CA certificate DER (for TLS config).
    pub fn ca_cert_der(&self) -> &[u8];

    /// Sign a CSR, issuing a user association certificate.
    /// Subject CN={device_id}. No expiry. clientAuth key usage.
    pub fn sign_csr(&self, csr_pem: &str, device_id: &str) -> Result<String>;

    /// Sign an enrollment JWT.
    pub fn sign_enrollment_jwt(&self, claims: &EnrollmentClaims) -> Result<String>;

    /// Verify and decode an enrollment JWT.
    pub fn verify_enrollment_jwt(&self, token: &str) -> Result<EnrollmentClaims>;
}
```

### 3.2 Server TLS Certificate (`server-core/src/pki/server_tls.rs`)

```rust
pub struct ServerTlsCert {
    pub cert_pem: String,
    pub key_pem: String,
    pub cert_der: Vec<u8>,
    pub fingerprint: String,
}

/// Load from existing PEM files.
pub fn load_server_tls(cert_pem: &str, key_pem: &str) -> Result<ServerTlsCert>;

/// Generate a new self-signed server TLS certificate (server-solo first startup).
/// CN="ghostcam-server", serverAuth key usage, 20-year validity.
pub fn generate_server_tls() -> Result<ServerTlsCert>;
```

### 3.3 First-Run Bootstrap (`server-core/src/pki/bootstrap.rs`)

Orchestrates first-run initialization for `server-solo`. Called from the server-solo main before starting the QUIC listener.

```rust
pub struct BootstrapResult {
    pub ca: CaManager,
    pub server_tls: ServerTlsCert,
    pub is_first_run: bool,
}

/// Bootstrap the PKI for server-solo.
///
/// data_dir: path to /var/ghostcam (or test directory)
///
/// On first run:
/// 1. Generate Instance CA → write ca.crt + ca.key
/// 2. Generate server TLS cert → write server.crt + server.key
/// 3. Print backup warning
///
/// On subsequent runs:
/// 1. Load existing CA from ca.crt + ca.key
/// 2. Load existing server TLS from server.crt + server.key
pub async fn bootstrap_pki(data_dir: &Path) -> Result<BootstrapResult>;
```

File layout on disk:
```
/var/ghostcam/
├── ca.crt          # Instance CA certificate (PEM)
├── ca.key          # Instance CA private key (PEM)
├── server.crt      # Server TLS certificate (PEM)
└── server.key      # Server TLS private key (PEM)
```

### 3.4 Enrollment JWT (`server-core/src/pki/enrollment.rs`)

```rust
#[derive(Debug, Serialize, Deserialize)]
pub struct EnrollmentClaims {
    pub iss: String,                    // "ghostcam-server"
    pub exp: u64,                       // Unix seconds
    pub jti: String,                    // UUID
    pub server_addr: String,            // "host:port"
    #[serde(skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub wifi: Option<Vec<WifiCredential>>,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct WifiCredential {
    pub ssid: String,
    pub psk: String,
}

impl EnrollmentClaims {
    /// Create new claims for an enrollment token.
    /// Generates a UUID jti and sets exp to now + ENROLLMENT_TOKEN_TTL_SECS.
    pub fn new(
        server_addr: &str,
        display_name: Option<String>,
        wifi: Option<Vec<WifiCredential>>,
    ) -> Self;
}

/// Generate a signed enrollment JWT and record the token in the database.
/// Returns the JWT string (the client renders this as a QR code).
pub async fn create_enrollment_token(
    ca: &CaManager,
    db: &dyn Database,
    user_id: &UserId,
    server_addr: &str,
    display_name: Option<String>,
    wifi: Option<Vec<WifiCredential>>,
) -> Result<String>;
```

### 3.5 Revocation Cache (`server-core/src/pki/revocation.rs`)

In-memory set of revoked certificate serial numbers. Stubbed to always-empty in this plan; Plan 5 adds the Redis refresh loop.

```rust
pub struct RevocationCache {
    revoked: RwLock<HashSet<String>>,
}

impl RevocationCache {
    pub fn new() -> Self {
        Self { revoked: RwLock::new(HashSet::new()) }
    }

    /// Check if a certificate serial number is revoked.
    pub async fn is_revoked(&self, serial: &str) -> bool;

    /// Replace the entire cache contents (called by the refresh loop in Plan 5).
    pub async fn replace(&self, serials: HashSet<String>);

    /// Add a single serial number (called on unregistration).
    pub async fn add(&self, serial: String);
}
```

### 3.6 Enrollment Handler (`server-core/src/ingest/enrollment.rs`)

Integrated into the QUIC accept loop from Plan 3. When a camera connects without a user association certificate, the accept loop routes to this handler instead of creating an IngestSlot.

```rust
/// Handle an enrollment QUIC connection.
///
/// 1. Open a Commands stream toward the camera
/// 2. Accept the Alerts stream from the camera
/// 3. Read the enrollment JWT from the connection (presented during TLS or as first alert)
/// 4. Verify JWT signature and expiry
/// 5. Check jti against enrollment_tokens table — reject if already claimed
/// 6. Wait for `csr` alert from camera
/// 7. Verify CSR signature
/// 8. Create cameras row in database (generates device_id UUID)
/// 9. Sign the CSR with the CA (subject CN={device_id})
/// 10. Send `cert_refresh` command with signed cert + CA cert
/// 11. Wait for `ack` alert
/// 12. Mark camera as enrolled in database (claim the enrollment token)
/// 13. Close the connection
pub async fn handle_enrollment(
    connection: quinn::Connection,
    fingerprint: CertFingerprint,
    ca: &CaManager,
    db: &dyn Database,
) -> Result<()>;
```

**Enrollment JWT delivery:** The camera presents the enrollment JWT as the first message on the Alerts stream (before the normal `handshake`). The enrollment handler reads this first, then waits for the `csr` alert. This means the enrollment Alerts stream carries: JWT string (length-prefixed) → `csr` alert → `ack` alert.

Alternatively, the JWT could be carried in a custom TLS extension or QUIC transport parameter, but using the Alerts stream is simpler and consistent with the existing framing.

**Decision: JWT on Alerts stream.** The camera sends a special `enrollment` alert as the first message:

```rust
// Add to Alert enum:
Alert::Enrollment {
    token: String,  // The raw JWT string
}
```

This is a wire protocol addition — the `enrollment` alert type is only valid as the first message on an enrollment connection.

### 3.7 Unregistration Flow (`server-core/src/pki/unregister.rs`)

```rust
/// Unregister a camera: send unregister command, wait for ack,
/// delete from database, revoke certificate.
///
/// If the camera is online (slot exists in registry):
/// 1. Send `unregister` command via the slot
/// 2. Wait for `ack` (with timeout)
/// 3. After ack: hard-delete cameras row, add cert serial to revocation cache
/// 4. Slot will be torn down when camera closes connection
///
/// If the camera is offline:
/// 1. Mark camera as pending-unregistration in database (add a flag or separate table)
/// 2. On next connection, deliver the `unregister` command before creating a slot
/// 3. After ack: proceed with deletion and revocation
pub async fn unregister_camera(
    device_id: &DeviceId,
    registry: &RoutingRegistry,
    db: &dyn Database,
    revocation_cache: &RevocationCache,
) -> Result<UnregisterResult>;

pub enum UnregisterResult {
    /// Camera was online, unregister command delivered and acknowledged.
    Completed,
    /// Camera is offline, unregister queued for next connection.
    Queued,
}
```

### 3.8 Accept Loop Updates

Update `handle_connection` from Plan 3 to integrate enrollment routing and revocation checking:

```rust
async fn handle_connection(
    incoming: quinn::Incoming,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
    ca: Arc<CaManager>,
    revocation_cache: Arc<RevocationCache>,
) -> Result<()> {
    let connection = incoming.await?;
    let peer_certs = extract_peer_certs(&connection)?;
    let fingerprint = ghostcam::pki::cert_fingerprint(&peer_certs[0])?;

    let has_association_cert = peer_certs.len() >= 2;

    if !has_association_cert {
        // Enrollment path
        return handle_enrollment(connection, fingerprint, &ca, db.as_ref()).await;
    }

    // Normal path — verify user association cert
    // 1. Extract serial number from user association cert
    let user_cert_der = &peer_certs[1];
    let serial = ghostcam::pki::cert_serial_number_from_der(user_cert_der)?;

    // 2. Check revocation cache
    if revocation_cache.is_revoked(&serial).await {
        tracing::warn!(?fingerprint, serial, "revoked certificate — rejecting");
        connection.close(3u32.into(), b"certificate revoked");
        return Ok(());
    }

    // 3. Verify user association cert was signed by our CA
    // (application-layer check — TLS layer accepted any cert)
    ca.verify_user_cert(user_cert_der)?;

    // 4. Extract device_id from user association cert CN
    let device_id_from_cert = ghostcam::pki::extract_cn(user_cert_der)?;

    // 5. Database lookup by fingerprint
    let camera = db.get_camera_by_fingerprint(&fingerprint).await?
        .ok_or_else(|| anyhow::anyhow!("device not enrolled"))?;

    // 6. Verify device_id from cert matches database record
    if camera.device_id.0 != device_id_from_cert {
        connection.close(4u32.into(), b"device identity mismatch");
        return Err(anyhow::anyhow!("device_id mismatch"));
    }

    // ... (rest of accept flow from Plan 3: update last_seen, accept streams, handshake, create slot)
}
```

### 3.9 CaManager Additional Methods

```rust
impl CaManager {
    /// Verify a user association certificate was signed by this CA.
    pub fn verify_user_cert(&self, cert_der: &[u8]) -> Result<()>;
}
```

### 3.10 PKI Utility Additions (`ghostcam/src/pki.rs`)

Add to the shared library:

```rust
/// Extract the CN from a DER-encoded certificate.
pub fn extract_cn(cert_der: &[u8]) -> Result<String>;

/// Compute the serial number from a DER-encoded certificate (hex string).
pub fn cert_serial_number_from_der(cert_der: &[u8]) -> Result<String>;
```

### 3.11 Module Structure

```
server-core/src/
├── lib.rs
├── db.rs
├── auth.rs
├── frames.rs
├── ingest/
│   ├── mod.rs
│   ├── slot.rs
│   ├── alerts.rs
│   ├── uploads.rs
│   ├── demand.rs
│   ├── registry.rs
│   ├── accept.rs              # Updated with enrollment routing + revocation
│   ├── enrollment.rs          # NEW — enrollment handler
│   └── quic_config.rs
└── pki/
    ├── mod.rs                 # re-exports
    ├── ca.rs                  # CaManager
    ├── server_tls.rs          # Server TLS cert generation/loading
    ├── bootstrap.rs           # First-run PKI bootstrap
    ├── enrollment.rs          # EnrollmentClaims, JWT creation
    ├── revocation.rs          # RevocationCache
    └── unregister.rs          # Unregistration flow
```

---

## 4. Spec Updates

### 4.1 Wire Protocol — Enrollment Alert

Add a new alert type to `specs/wire-protocol.md` §6:

> ### 6.15 `enrollment`
>
> Sent as the first message on the Alerts stream during an enrollment QUIC connection. Carries the enrollment JWT. Only valid on connections where no user association certificate is presented.
>
> ```json
> {
>   "type": "enrollment",
>   "token": "<JWT string>"
> }
> ```

### 4.2 Update Spec Open Questions

Mark the following as resolved:
- `ingest.md` §10 — upload stream disambiguation: resolved by 1-byte type tag (Plan 3)

---

## 5. Testing Plan

### 5.1 Unit Tests — CaManager

**Location:** `server-core/src/pki/ca.rs`

| Test | Description |
|------|-------------|
| `generate_instance_ca` | Generate CA → cert is self-signed, CN="Ghostcam Instance CA", has keyCertSign usage |
| `from_pem_roundtrip` | Generate CA, export PEM, reconstruct from PEM → sign_csr produces same results |
| `sign_csr_subject_cn` | Sign a CSR → resulting cert CN matches the provided device_id |
| `sign_csr_issuer` | Sign a CSR → resulting cert issuer matches CA CN |
| `sign_csr_client_auth` | Sign a CSR → resulting cert has clientAuth key usage |
| `sign_csr_no_expiry` | Sign a CSR → resulting cert has a far-future notAfter (effectively permanent) |
| `sign_csr_invalid_pem` | Sign garbage PEM → error |
| `verify_user_cert_valid` | Sign a CSR, then verify the resulting cert → success |
| `verify_user_cert_wrong_ca` | Generate two CAs, sign with CA-A, verify against CA-B → error |
| `sign_enrollment_jwt` | Sign a JWT → non-empty string, starts with "ey" |
| `verify_enrollment_jwt_roundtrip` | Sign then verify → claims match |
| `verify_enrollment_jwt_expired` | Sign with exp in the past → verification fails |
| `verify_enrollment_jwt_wrong_key` | Sign with CA-A key, verify with CA-B key → fails |

### 5.2 Unit Tests — Server TLS

**Location:** `server-core/src/pki/server_tls.rs`

| Test | Description |
|------|-------------|
| `generate_server_tls` | Generate cert → has serverAuth key usage, CN="ghostcam-server" |
| `load_roundtrip` | Generate, export PEM, load from PEM → fingerprint matches |
| `fingerprint_deterministic` | Same cert → same fingerprint |

### 5.3 Unit Tests — Bootstrap

**Location:** `server-core/src/pki/bootstrap.rs`

| Test | Description |
|------|-------------|
| `first_run_generates_files` | Bootstrap on empty dir → ca.crt, ca.key, server.crt, server.key all created |
| `first_run_is_first_run` | First bootstrap → `is_first_run == true` |
| `second_run_loads_existing` | Bootstrap twice on same dir → second run loads existing, `is_first_run == false` |
| `second_run_same_fingerprint` | CA cert fingerprint same across restarts |
| `files_are_valid_pem` | Generated files are valid PEM that can be parsed |

### 5.4 Unit Tests — Enrollment JWT

**Location:** `server-core/src/pki/enrollment.rs`

| Test | Description |
|------|-------------|
| `claims_new_sets_jti` | `EnrollmentClaims::new()` → jti is a valid UUID |
| `claims_new_sets_exp` | exp is approximately now + 600 seconds |
| `claims_new_sets_iss` | iss == "ghostcam-server" |
| `claims_with_display_name` | display_name is preserved |
| `claims_with_wifi` | wifi credentials are preserved |
| `claims_without_optionals` | display_name and wifi are None → not present in serialized JWT |
| `create_enrollment_token_records_jti` | Create token → jti exists in enrollment_tokens table |
| `create_enrollment_token_jwt_verifies` | Create token → returned JWT verifies against CA |

### 5.5 Unit Tests — Revocation Cache

**Location:** `server-core/src/pki/revocation.rs`

| Test | Description |
|------|-------------|
| `empty_cache_not_revoked` | New cache → `is_revoked("anything")` returns false |
| `add_and_check` | Add serial "abc" → `is_revoked("abc")` returns true |
| `add_does_not_affect_others` | Add "abc" → `is_revoked("def")` returns false |
| `replace_clears_old` | Add "abc", replace with {"def"} → "abc" not revoked, "def" revoked |
| `concurrent_reads` | Spawn 100 concurrent `is_revoked` calls → no panic |
| `concurrent_add_and_read` | Spawn concurrent add + read tasks → no panic, eventually consistent |

### 5.6 Unit Tests — Unregistration

**Location:** `server-core/src/pki/unregister.rs`

| Test | Description |
|------|-------------|
| `unregister_online_camera` | Camera is in registry → command sent, after ack: camera deleted from DB, serial added to revocation cache |
| `unregister_offline_camera` | Camera not in registry → returns `Queued` |

These use mock/in-memory implementations rather than real QUIC connections.

### 5.7 Unit Tests — PKI Utility Additions

**Location:** `ghostcam/src/pki.rs`

| Test | Description |
|------|-------------|
| `extract_cn_from_cert` | Create cert with CN="test-device", extract CN → "test-device" |
| `serial_number_from_der` | Create cert, extract serial from DER → non-empty hex string |
| `serial_number_matches_pem_extraction` | Same cert: serial from DER == serial from PEM |

### 5.8 Integration Tests — Enrollment Flow

**Location:** `server-core/tests/enrollment_integration.rs`

These use the `TestEnv` from Plan 3 extended with CA and revocation cache.

| Test | Description |
|------|-------------|
| `full_enrollment_flow` | 1. Create enrollment token via `create_enrollment_token`. 2. MockCamera connects with device cert only (no user association cert). 3. MockCamera sends `enrollment` alert with JWT. 4. MockCamera sends `csr` alert. 5. MockCamera receives `cert_refresh` command with cert PEM + CA PEM. 6. MockCamera sends `ack`. 7. Verify: camera row exists in DB with correct fingerprint, enrollment token is claimed, returned cert CN matches device_id. |
| `enrollment_expired_jwt` | Create token, advance time past expiry, MockCamera connects with expired JWT → connection closed, no camera row created |
| `enrollment_replayed_jwt` | Create token, complete enrollment, attempt second enrollment with same JWT → rejected (jti already claimed) |
| `enrollment_invalid_jwt_signature` | MockCamera presents a JWT signed by a different key → rejected |
| `enrollment_invalid_csr` | MockCamera sends a `csr` with garbage PEM → server rejects, no camera row |
| `enrollment_no_ack_timeout` | MockCamera sends CSR, receives cert_refresh, but never sends ack → server does not mark enrolled (test with timeout) |

### 5.9 Integration Tests — Revocation

**Location:** `server-core/tests/revocation_integration.rs`

| Test | Description |
|------|-------------|
| `revoked_cert_rejected` | Enroll camera, add its cert serial to revocation cache, MockCamera reconnects → connection rejected |
| `unrevoked_cert_accepted` | Enroll camera, do NOT revoke, MockCamera reconnects → accepted |

### 5.10 Integration Tests — Unregistration

**Location:** `server-core/tests/unregister_integration.rs`

| Test | Description |
|------|-------------|
| `unregister_online_e2e` | Enroll camera, connect, call `unregister_camera` → MockCamera receives `unregister` command, sends `ack`, connection closes, camera deleted from DB, cert serial in revocation cache |
| `unregister_camera_cannot_reconnect` | After unregistration, MockCamera reconnects with same certs → rejected (revoked) |

### 5.11 Integration Tests — Normal Connection with PKI Verification

**Location:** `server-core/tests/pki_connection_integration.rs`

| Test | Description |
|------|-------------|
| `enrolled_camera_connects` | Full flow: bootstrap PKI, enroll camera, MockCamera reconnects with device cert + user association cert → accepted, slot created |
| `wrong_ca_cert_rejected` | MockCamera presents a user association cert signed by a different CA → rejected |
| `device_id_mismatch_rejected` | MockCamera presents a user association cert with CN="wrong-id" that doesn't match DB record → rejected |
| `unenrolled_fingerprint_rejected` | MockCamera with valid-looking certs but unknown fingerprint → rejected |

### 5.12 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All crates compile |
| ghostcam tests | `cargo test -p ghostcam` | PKI utility tests pass |
| server-core unit tests | `cargo test -p server-core` | All unit tests pass |
| Integration tests | `cargo test -p server-core --test enrollment_integration --test revocation_integration --test unregister_integration --test pki_connection_integration` | All pass |
| Clippy | `cargo clippy -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 6. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test` passes all tests listed above
- [ ] Instance CA is generated on first server-solo startup and persisted
- [ ] Instance CA is loaded from disk on subsequent startups (not regenerated)
- [ ] Server TLS cert is generated on first startup and persisted
- [ ] CA cert, CA key, server cert, server key are all valid PEM files on disk
- [ ] Enrollment JWT is signed with ES256 using the CA key
- [ ] Enrollment JWT verification rejects expired tokens
- [ ] Enrollment JWT verification rejects tokens signed by wrong key
- [ ] Enrollment token jti is recorded in database and prevents replay
- [ ] Full enrollment flow: camera connects → sends JWT → sends CSR → receives cert → acks → enrolled in DB
- [ ] Signed user association cert has CN={device_id}, clientAuth usage, no expiry
- [ ] Signed user association cert verifies against the CA
- [ ] Revocation cache rejects connections with revoked cert serials
- [ ] Unregistration sends unregister command, waits for ack, deletes camera, revokes cert
- [ ] Unregistered camera cannot reconnect (cert is revoked)
- [ ] Camera with unknown fingerprint is rejected at application layer
- [ ] Camera with device_id mismatch between cert CN and DB record is rejected
- [ ] `specs/wire-protocol.md` updated with `enrollment` alert type
- [ ] `CLAUDE.md` updated with PKI module structure
