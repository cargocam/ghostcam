# Plan 9: Camera Firmware — Enrollment, QR/OTA & Network

## Overview

This plan covers the camera-side firmware logic for device enrollment, trust establishment, configuration, network management, and over-the-air updates. It builds on the camera core from Plan 7, adding the registration mode that detects an unenrolled device, scans a QR code to obtain enrollment credentials, completes the enrollment QUIC handshake, and stores the resulting certificates. It also covers TOFU server pinning, `ghostcam.conf` parsing, network helpers, OTA firmware updates, and a watchdog wrapper script.

**Depends on**: Plan 1 (shared types, PKI utilities), Plan 4 (PKI & enrollment server-side), Plan 7 (camera core)

## 1. Registration Mode Detection

### Startup Flow

On camera startup (before the normal QUIC connect loop from Plan 7), check for enrollment state:

```
main() startup:
  1. Parse CLI args + load ghostcam.conf (Plan 7)
  2. Check for user.crt at config_dir/certs/user.crt
     - EXISTS → normal mode (Plan 7 connect loop)
     - MISSING → registration mode (this plan)
```

### File Layout

```
/etc/ghostcam/              (or ~/.config/ghostcam/ for dev)
├── ghostcam.conf            TOML config
├── certs/
│   ├── device.key           P-256 private key (generated on first boot, never leaves device)
│   ├── device.crt           Self-signed device certificate
│   ├── user.crt             CA-signed user association certificate (from enrollment)
│   └── server_fingerprint   SHA-256 of server TLS cert DER (TOFU pin)
└── firmware/
    ├── current              Symlink to active binary
    └── previous             Previous binary (rollback target)
```

The `config_dir` is resolved from (in order):
1. `--config-dir` CLI flag
2. `GHOSTCAM_CONFIG_DIR` env var
3. `/etc/ghostcam` (Linux default)
4. `~/.config/ghostcam` (fallback / dev)

### Device Identity Bootstrap

On first boot, if `device.key` doesn't exist:
1. Generate P-256 keypair via `rcgen`
2. Create self-signed device certificate (CN = random UUID, 10-year validity)
3. Write `device.key` (PEM) and `device.crt` (PEM) to `certs/`
4. Set file permissions: `device.key` is 0600, `device.crt` is 0644

If `device.key` exists but `device.crt` doesn't, regenerate the self-signed cert from the existing key.

## 2. QR Code Scanning

### Capture Loop

Registration mode runs a QR scan loop:

```rust
async fn registration_loop(config: &Config) -> Result<EnrollmentData> {
    info!("No user certificate found — entering registration mode");
    info!("Present enrollment QR code to camera");

    let mut interval = tokio::time::interval(Duration::from_millis(500));

    loop {
        interval.tick().await;

        // Capture a still frame
        let jpeg_data = capture_still().await?;

        // Decode JPEG → grayscale
        let img = image::load_from_memory_with_format(&jpeg_data, ImageFormat::Jpeg)?;
        let gray = img.to_luma8();

        // Scan for QR codes
        let mut decoder = rqrr::PreparedImage::prepare(gray);
        let grids = decoder.detect_grids();

        for grid in grids {
            if let Ok((_, content)) = grid.decode() {
                match parse_enrollment_jwt(&content) {
                    Ok(data) => {
                        info!("Enrollment QR code detected");
                        return Ok(data);
                    }
                    Err(e) => {
                        debug!("QR found but not a valid enrollment JWT: {e}");
                    }
                }
            }
        }
    }
}
```

### Still Capture

Two modes for `capture_still()`:

**Real mode (Linux with rpicam-still)**:
```rust
async fn capture_still_rpicam() -> Result<Vec<u8>> {
    let output = tokio::process::Command::new("rpicam-still")
        .args(["--timeout", "100", "--nopreview", "-o", "-", "--encoding", "jpg", "--width", "640", "--height", "480"])
        .output()
        .await?;

    if !output.status.success() {
        anyhow::bail!("rpicam-still failed: {}", String::from_utf8_lossy(&output.stderr));
    }
    Ok(output.stdout)
}
```

**Test mode** (`--test-source`):
```rust
async fn capture_still_test() -> Result<Vec<u8>> {
    // Read from a test JPEG file or generate a synthetic image
    // For testing enrollment flow without a real camera
    let img = image::RgbaImage::new(640, 480);
    let mut buf = Vec::new();
    img.write_to(&mut std::io::Cursor::new(&mut buf), ImageFormat::Jpeg)?;
    Ok(buf)
}
```

In test mode, the registration loop can also accept the enrollment JWT via stdin or a `--enrollment-jwt` CLI flag to bypass QR scanning entirely.

### JWT Parsing

```rust
struct EnrollmentData {
    server_addr: SocketAddr,    // From JWT claims
    token: String,              // The raw JWT string
    owner_id: String,           // Owner/user ID
    device_name: Option<String>,// Optional display name
}

fn parse_enrollment_jwt(content: &str) -> Result<EnrollmentData> {
    // Decode JWT without verification (camera doesn't have the CA cert yet)
    // The JWT signature will be verified server-side during enrollment
    let token_data = jsonwebtoken::decode::<EnrollmentClaims>(
        content,
        &jsonwebtoken::DecodingKey::from_secret(&[]), // No verification
        &{
            let mut v = jsonwebtoken::Validation::default();
            v.insecure_disable_signature_validation();
            v.validate_exp = false; // Server validates expiry
            v
        },
    )?;

    let claims = token_data.claims;
    let server_addr = claims.server_addr.parse::<SocketAddr>()?;

    Ok(EnrollmentData {
        server_addr,
        token: content.to_string(),
        owner_id: claims.sub,
        device_name: claims.device_name,
    })
}
```

### Dependencies

```toml
[dependencies]
rqrr = "0.8"           # QR code detection + decode
image = { version = "0.25", default-features = false, features = ["jpeg"] }
```

Both are pure Rust, no native dependencies.

## 3. Enrollment QUIC Handshake

### Connection

After QR decode, the camera connects to the server's QUIC endpoint using only the device certificate (no user cert yet):

```rust
async fn enroll(
    enrollment: &EnrollmentData,
    device_key: &[u8],
    device_cert: &[u8],
) -> Result<EnrollmentResult> {
    // Build QUIC client config with device cert only
    let client_config = build_enrollment_quic_config(device_key, device_cert)?;

    let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse()?)?;
    endpoint.set_default_client_config(client_config);

    let connection = endpoint
        .connect(enrollment.server_addr, "ghostcam")?
        .await?;

    // Open bidirectional stream for enrollment
    let (mut send, mut recv) = connection.open_bi().await?;

    // Send enrollment alert (contains JWT + CSR)
    let csr = generate_csr(device_key)?;
    let alert = Alert::Enrollment {
        token: enrollment.token.clone(),
        csr: base64_encode(&csr),
    };
    send_alert(&mut send, &alert).await?;

    // Receive response (Command::CertRefresh or Command::EnrollmentRejected)
    let response = recv_command(&mut recv).await?;

    match response {
        Command::CertRefresh { certificate } => {
            Ok(EnrollmentResult {
                user_cert: base64_decode(&certificate)?,
                server_fingerprint: get_peer_fingerprint(&connection)?,
            })
        }
        Command::EnrollmentRejected { reason } => {
            anyhow::bail!("Enrollment rejected: {reason}");
        }
        other => {
            anyhow::bail!("Unexpected response during enrollment: {other:?}");
        }
    }
}
```

### CSR Generation

```rust
fn generate_csr(device_key_pem: &[u8]) -> Result<Vec<u8>> {
    let key_pair = rcgen::KeyPair::from_pem(
        &String::from_utf8(device_key_pem.to_vec())?
    )?;

    let mut params = rcgen::CertificateSigningRequestParams::default();
    // Subject will be set by the CA during signing

    let csr = params.serialize_der(&key_pair)?;
    Ok(csr)
}
```

### Certificate Storage

On successful enrollment:

```rust
async fn store_enrollment(
    config_dir: &Path,
    result: &EnrollmentResult,
) -> Result<()> {
    let certs_dir = config_dir.join("certs");

    // Write user certificate
    tokio::fs::write(certs_dir.join("user.crt"), &result.user_cert).await?;

    // Write server fingerprint (TOFU pin)
    tokio::fs::write(
        certs_dir.join("server_fingerprint"),
        hex::encode(&result.server_fingerprint),
    ).await?;

    info!("Enrollment complete — certificates stored");
    Ok(())
}
```

After successful enrollment, the camera exits registration mode and proceeds to the normal connect loop (Plan 7) without restarting.

## 4. TOFU (Trust on First Use)

### Server Fingerprint Pinning

On first enrollment, the camera captures the SHA-256 fingerprint of the server's TLS certificate:

```rust
fn get_peer_fingerprint(connection: &quinn::Connection) -> Result<[u8; 32]> {
    let peer_certs = connection
        .peer_identity()
        .and_then(|id| id.downcast::<Vec<rustls::pki_types::CertificateDer>>().ok())
        .ok_or_else(|| anyhow::anyhow!("No peer certificate"))?;

    let leaf = peer_certs.first()
        .ok_or_else(|| anyhow::anyhow!("Empty cert chain"))?;

    use ring::digest;
    let fp = digest::digest(&digest::SHA256, leaf.as_ref());
    let mut result = [0u8; 32];
    result.copy_from_slice(fp.as_ref());
    Ok(result)
}
```

### Verification on Subsequent Connects

In the normal QUIC connect path (Plan 7), after the TLS handshake completes:

```rust
fn verify_server_fingerprint(
    connection: &quinn::Connection,
    config_dir: &Path,
) -> Result<()> {
    let fp_path = config_dir.join("certs/server_fingerprint");

    if !fp_path.exists() {
        // No pin stored — skip verification (first connect or TOFU disabled)
        return Ok(());
    }

    let stored_hex = std::fs::read_to_string(&fp_path)?;
    let stored = hex::decode(stored_hex.trim())?;

    let actual = get_peer_fingerprint(connection)?;

    if stored.as_slice() != actual.as_slice() {
        anyhow::bail!(
            "Server TLS fingerprint mismatch! Expected {}, got {}. \
             This may indicate a MITM attack or server certificate rotation. \
             Delete {} to re-pin.",
            hex::encode(&stored),
            hex::encode(&actual),
            fp_path.display()
        );
    }

    Ok(())
}
```

### Disabling TOFU

For development, TOFU can be disabled via:
- `--no-tofu` CLI flag
- `tofu = false` in `ghostcam.conf`

When disabled, the fingerprint file is neither written nor checked.

## 5. Unregistration

A camera can be unregistered via the `unregister` CameraCommand (from Plan 7's command handler):

```rust
Command::Unregister => {
    info!("Received unregister command — clearing enrollment state");

    let certs_dir = config_dir.join("certs");

    // Remove user certificate
    let _ = tokio::fs::remove_file(certs_dir.join("user.crt")).await;

    // Remove server fingerprint
    let _ = tokio::fs::remove_file(certs_dir.join("server_fingerprint")).await;

    // Keep device.key and device.crt — device identity persists

    // Signal main loop to disconnect and re-enter registration mode
    unregister_tx.send(()).ok();
}
```

The main loop structure becomes:

```rust
loop {
    if !user_cert_exists(&config_dir) {
        // Registration mode
        let enrollment = registration_loop(&config).await?;
        let result = enroll(&enrollment, &device_key, &device_cert).await?;
        store_enrollment(&config_dir, &result).await?;
        // Fall through to connect loop
    }

    // Normal connect loop (Plan 7)
    match run_session(&config, &config_dir, &mut unregister_rx).await {
        Ok(Disconnected::Unregistered) => {
            info!("Unregistered — re-entering registration mode");
            continue; // Back to top, will enter registration mode
        }
        Ok(Disconnected::ServerGone) => {
            // Reconnect with backoff
            backoff.wait().await;
        }
        Err(e) => {
            error!("Session error: {e}");
            backoff.wait().await;
        }
    }
}
```

## 6. ghostcam.conf Parsing

### Format

TOML configuration file:

```toml
# Server connection
server_addr = "192.168.1.100:4433"

# Device identity
device_id = "cam-front-door"

# Capture settings
[capture]
width = 1920
height = 1080
fps = 30
bitrate = 4000000
audio = true

# Recording
[recording]
enabled = true
ring_hours = 72
segment_seconds = 6
storage_path = "/var/lib/ghostcam/recordings"
max_storage_mb = 32000

# Network
[network]
wifi_ssid = "CameraNet"
wifi_psk = "hunter2"

# Security
[security]
tofu = true

# Telemetry
[telemetry]
interval_secs = 2
heartbeat_secs = 30
gps = false
```

### Config Resolution

```rust
#[derive(Debug, Deserialize)]
struct ConfigFile {
    server_addr: Option<String>,
    device_id: Option<String>,

    #[serde(default)]
    capture: CaptureConfig,

    #[serde(default)]
    recording: RecordingConfig,

    #[serde(default)]
    network: NetworkConfig,

    #[serde(default)]
    security: SecurityConfig,

    #[serde(default)]
    telemetry: TelemetryConfig,
}

fn resolve_config(cli: &CliArgs) -> Result<Config> {
    // Load config file (optional)
    let config_file = load_config_file(&cli.config_dir)?;

    // CLI flags override config file, which overrides defaults
    // server_addr: CLI > config > enrollment data (stored separately)
    // device_id: CLI > config > hostname

    Ok(Config {
        server_addr: cli.server_addr
            .or(config_file.server_addr)
            .or_else(|| read_enrolled_server_addr(&cli.config_dir)),
        device_id: cli.device_id
            .or(config_file.device_id)
            .unwrap_or_else(|| hostname()),
        capture: merge_capture(cli, &config_file.capture),
        recording: config_file.recording,
        network: config_file.network,
        security: config_file.security,
        telemetry: config_file.telemetry,
    })
}
```

### Enrolled Server Address

After enrollment, the server address from the JWT is persisted so the camera knows where to reconnect:

```rust
fn store_enrolled_server_addr(config_dir: &Path, addr: &SocketAddr) -> Result<()> {
    let path = config_dir.join("enrolled_server");
    std::fs::write(&path, addr.to_string())?;
    Ok(())
}

fn read_enrolled_server_addr(config_dir: &Path) -> Option<String> {
    std::fs::read_to_string(config_dir.join("enrolled_server")).ok()
}
```

Precedence for server address: CLI flag > ghostcam.conf > enrolled_server file.

## 7. Network Management

### WiFi Connection on Boot

If `network.wifi_ssid` is set in config, attempt WiFi connection on startup:

```rust
async fn ensure_wifi(config: &NetworkConfig) -> Result<()> {
    let ssid = match &config.wifi_ssid {
        Some(s) => s,
        None => return Ok(()), // No WiFi config, skip
    };

    // Check if already connected
    let status = tokio::process::Command::new("nmcli")
        .args(["connection", "show", "--active"])
        .output()
        .await?;

    let stdout = String::from_utf8_lossy(&status.stdout);
    if stdout.contains(ssid) {
        return Ok(()); // Already connected
    }

    // Connect
    info!(ssid, "Connecting to WiFi network");
    let connect = tokio::process::Command::new("nmcli")
        .args(["device", "wifi", "connect", ssid])
        .args(config.wifi_psk.as_ref().map(|p| vec!["password", p]).unwrap_or_default())
        .output()
        .await?;

    if !connect.status.success() {
        let err = String::from_utf8_lossy(&connect.stderr);
        anyhow::bail!("WiFi connection failed: {err}");
    }

    info!(ssid, "WiFi connected");
    Ok(())
}
```

### Remote Network Commands

Handled as `Custom` CameraCommands via the command handler (Plan 7):

```rust
Command::Custom { name, params } => match name.as_str() {
    "network_config" => {
        let ssid = params.get("ssid").and_then(|v| v.as_str())
            .ok_or_else(|| anyhow::anyhow!("missing ssid"))?;
        let psk = params.get("psk").and_then(|v| v.as_str());

        let mut cmd = tokio::process::Command::new("nmcli");
        cmd.args(["device", "wifi", "connect", ssid]);
        if let Some(psk) = psk {
            cmd.args(["password", psk]);
        }
        let output = cmd.output().await?;

        if output.status.success() {
            info!(ssid, "Network configured via command");
        } else {
            warn!(ssid, "Network config failed: {}", String::from_utf8_lossy(&output.stderr));
        }
    }
    "remove_network" => {
        let ssid = params.get("ssid").and_then(|v| v.as_str())
            .ok_or_else(|| anyhow::anyhow!("missing ssid"))?;

        let _ = tokio::process::Command::new("nmcli")
            .args(["connection", "delete", ssid])
            .output()
            .await;

        info!(ssid, "Network removed via command");
    }
    "list_networks" => {
        let output = tokio::process::Command::new("nmcli")
            .args(["device", "wifi", "list"])
            .output()
            .await?;

        // Send result back as an alert (best-effort)
        let networks = String::from_utf8_lossy(&output.stdout).to_string();
        debug!(networks, "Available networks");
        // Could send as a custom alert if needed
    }
    _ => {
        warn!(name, "Unknown custom command — ignoring");
    }
}
```

## 8. OTA Firmware Updates

### New Command Variant

Add to the `Command` enum (Plan 1):

```rust
/// In Command enum:
FirmwareUpdate {
    url: String,           // HTTPS download URL
    sha256: String,        // Expected SHA-256 hex digest
    version: String,       // Version string for logging
}
```

### Update Handler

```rust
async fn handle_firmware_update(
    url: &str,
    expected_sha256: &str,
    version: &str,
    config_dir: &Path,
) -> Result<()> {
    info!(version, "Firmware update received — downloading");

    let firmware_dir = config_dir.join("firmware");
    tokio::fs::create_dir_all(&firmware_dir).await?;

    let temp_path = firmware_dir.join("downloading");
    let current_path = firmware_dir.join("current");
    let previous_path = firmware_dir.join("previous");

    // Download to temp file
    let response = reqwest::get(url).await?;
    if !response.status().is_success() {
        anyhow::bail!("Download failed: HTTP {}", response.status());
    }

    let bytes = response.bytes().await?;

    // Verify SHA-256
    use ring::digest;
    let actual = digest::digest(&digest::SHA256, &bytes);
    let actual_hex = hex::encode(actual.as_ref());

    if actual_hex != expected_sha256 {
        anyhow::bail!(
            "Firmware hash mismatch: expected {expected_sha256}, got {actual_hex}"
        );
    }

    // Write to temp file
    tokio::fs::write(&temp_path, &bytes).await?;

    // Set executable
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::set_permissions(&temp_path, std::fs::Permissions::from_mode(0o755)).await?;
    }

    // Atomic swap: current → previous, temp → current
    if current_path.exists() {
        tokio::fs::rename(&current_path, &previous_path).await?;
    }
    tokio::fs::rename(&temp_path, &current_path).await?;

    // Write sentinel absence (watchdog will check)
    let sentinel = firmware_dir.join("healthy");
    let _ = tokio::fs::remove_file(&sentinel).await;

    info!(version, "Firmware downloaded and verified — exiting for restart");

    // Exit cleanly — systemd/watchdog will restart with new binary
    std::process::exit(0);
}
```

### Health Sentinel

After the camera starts successfully and connects to the server, it writes a sentinel file:

```rust
// In the main connect loop, after first successful connection:
async fn mark_healthy(config_dir: &Path) {
    let sentinel = config_dir.join("firmware/healthy");
    let _ = tokio::fs::write(&sentinel, "ok").await;
}
```

### Watchdog Script

`camera/watchdog.sh`:

```bash
#!/bin/bash
# Ghostcam camera watchdog
# Runs the camera binary and handles firmware rollback

FIRMWARE_DIR="${GHOSTCAM_CONFIG_DIR:-/etc/ghostcam}/firmware"
CURRENT="$FIRMWARE_DIR/current"
PREVIOUS="$FIRMWARE_DIR/previous"
SENTINEL="$FIRMWARE_DIR/healthy"
HEALTH_TIMEOUT=60

# Use the firmware binary if it exists, otherwise use the system binary
if [ -x "$CURRENT" ]; then
    BINARY="$CURRENT"
else
    BINARY="$(which ghostcam-camera)"
fi

while true; do
    # Remove sentinel before starting
    rm -f "$SENTINEL"

    # Start camera in background
    "$BINARY" "$@" &
    PID=$!

    # Wait for health sentinel
    WAITED=0
    while [ ! -f "$SENTINEL" ] && [ $WAITED -lt $HEALTH_TIMEOUT ]; do
        sleep 1
        WAITED=$((WAITED + 1))

        # Check if process died
        if ! kill -0 $PID 2>/dev/null; then
            break
        fi
    done

    # If no sentinel after timeout and we have a previous version, rollback
    if [ ! -f "$SENTINEL" ] && [ -x "$PREVIOUS" ] && [ "$BINARY" = "$CURRENT" ]; then
        echo "WATCHDOG: Camera unhealthy after ${HEALTH_TIMEOUT}s — rolling back"
        kill $PID 2>/dev/null
        wait $PID 2>/dev/null

        mv "$CURRENT" "${CURRENT}.failed"
        mv "$PREVIOUS" "$CURRENT"
        BINARY="$CURRENT"
        continue
    fi

    # Wait for camera to exit
    wait $PID
    EXIT_CODE=$?

    if [ $EXIT_CODE -eq 0 ]; then
        echo "WATCHDOG: Camera exited cleanly (likely firmware update) — restarting"
    else
        echo "WATCHDOG: Camera crashed (exit $EXIT_CODE) — restarting in 5s"
        sleep 5
    fi
done
```

### Systemd Unit

`camera/ghostcam-camera.service`:

```ini
[Unit]
Description=Ghostcam Camera Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ghostcam-watchdog.sh
Restart=always
RestartSec=5
Environment=RUST_LOG=camera=info
Environment=GHOSTCAM_CONFIG_DIR=/etc/ghostcam

[Install]
WantedBy=multi-user.target
```

## 9. New Dependencies

Add to camera's `Cargo.toml`:

```toml
rqrr = "0.8"
image = { version = "0.25", default-features = false, features = ["jpeg"] }
reqwest = { version = "0.12", default-features = false, features = ["rustls-tls"] }
hex = "0.4"
```

`rqrr` and `image` are only used in registration mode. `reqwest` is only used for OTA downloads. All are pure Rust or use rustls (no OpenSSL dependency).

## 10. Wire Protocol Additions

### New Alert Variant

Add to `Alert` enum (from Plan 1):

```rust
/// Camera → Server
Enrollment {
    token: String,      // The enrollment JWT from QR code
    csr: String,        // Base64-encoded DER CSR
}
```

### New Command Variants

Add to `Command` enum (from Plan 1):

```rust
/// Server → Camera
CertRefresh {
    certificate: String,  // Base64-encoded PEM certificate
}

EnrollmentRejected {
    reason: String,
}

Unregister {}

FirmwareUpdate {
    url: String,
    sha256: String,
    version: String,
}
```

## 11. Test Plan

### 11.1 Unit Tests — Config Parsing

| # | Test | Validates |
|---|------|-----------|
| 1 | Parse full ghostcam.conf with all sections | All fields deserialize correctly |
| 2 | Parse minimal ghostcam.conf (only server_addr) | Defaults applied for missing sections |
| 3 | Empty/missing config file | All defaults used, no error |
| 4 | Invalid TOML syntax | Clear error message |
| 5 | CLI overrides config file values | Precedence: CLI > file > default |
| 6 | Server address resolution order | CLI > config > enrolled_server |
| 7 | device_id defaults to hostname | When not specified in CLI or config |

### 11.2 Unit Tests — JWT Parsing

| # | Test | Validates |
|---|------|-----------|
| 1 | Parse valid enrollment JWT | Extracts server_addr, token, owner_id |
| 2 | Parse JWT with optional device_name | device_name present |
| 3 | Parse JWT without device_name | device_name is None |
| 4 | Reject non-JWT string | Error, not panic |
| 5 | Reject JWT with missing required claims | Error for missing server_addr or sub |
| 6 | Handle JWT with extra/unknown claims | Ignored, no error |

### 11.3 Unit Tests — TOFU

| # | Test | Validates |
|---|------|-----------|
| 1 | Store fingerprint as hex | File written, readable |
| 2 | Verify matching fingerprint | Ok(()) |
| 3 | Reject mismatched fingerprint | Error with both fingerprints in message |
| 4 | Skip verification when no pin file | Ok(()) |
| 5 | Skip verification when tofu disabled | Ok(()) even with pin file |

### 11.4 Unit Tests — OTA

| # | Test | Validates |
|---|------|-----------|
| 1 | SHA-256 verification passes for correct hash | Update proceeds |
| 2 | SHA-256 verification fails for wrong hash | Error, temp file cleaned up |
| 3 | Atomic swap: current → previous, new → current | File positions correct |
| 4 | First update (no previous binary) | No error on missing previous |
| 5 | FirmwareUpdate command deserialization | All fields parsed |

### 11.5 Unit Tests — Network Commands

| # | Test | Validates |
|---|------|-----------|
| 1 | network_config command parsing | ssid and psk extracted from params |
| 2 | remove_network command parsing | ssid extracted |
| 3 | Unknown custom command | Logged and ignored, no error |

### 11.6 Unit Tests — Device Identity

| # | Test | Validates |
|---|------|-----------|
| 1 | Generate device keypair + self-signed cert | Files written with correct permissions |
| 2 | Regenerate cert from existing key | Same public key, new cert |
| 3 | CSR generation from device key | Valid DER CSR |

### 11.7 Integration Tests — Registration Flow

| # | Test | Validates |
|---|------|-----------|
| 1 | Full registration with --enrollment-jwt bypass | Skips QR scanning, connects to server, receives cert |
| 2 | Registration → normal mode transition | After enrollment, camera enters connect loop without restart |
| 3 | Unregister → re-registration | Camera re-enters registration mode after unregister command |
| 4 | Enrollment with invalid JWT | Camera retries scan loop (doesn't crash) |
| 5 | Enrollment with expired token | Server rejects, camera logs error and retries |

### 11.8 Integration Tests — QR Scanning (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | Display enrollment QR on screen, point camera at it | Camera detects QR, parses JWT, begins enrollment |
| 2 | QR at various angles and distances | Robust detection within reasonable range |
| 3 | Multiple QR codes in frame | Camera picks the valid enrollment JWT |
| 4 | Non-enrollment QR codes | Ignored, scan continues |

### 11.9 Integration Tests — OTA (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | Send firmware_update command with valid binary | Camera downloads, verifies, swaps, exits |
| 2 | Watchdog restarts with new binary | New version runs, writes sentinel |
| 3 | Bad binary (crashes immediately) | Watchdog rolls back to previous after timeout |
| 4 | Hash mismatch in firmware_update | Camera rejects update, continues running |
| 5 | Download URL unreachable | Camera logs error, continues running |

### 11.10 Validation Checklist

```
[ ] ghostcam.conf parsing works for all field combinations
[ ] Device keypair generated on first boot
[ ] --enrollment-jwt flag bypasses QR scanning
[ ] Enrollment QUIC connection uses device cert only (no user cert)
[ ] CSR sent to server, signed cert received back
[ ] user.crt stored, camera transitions to normal mode
[ ] TOFU fingerprint stored on first enrollment
[ ] TOFU rejects mismatched server on reconnect
[ ] --no-tofu disables fingerprint check
[ ] Unregister command clears user.crt and server_fingerprint
[ ] Camera re-enters registration after unregister
[ ] OTA download verifies SHA-256
[ ] Atomic binary swap (current ↔ previous)
[ ] Watchdog rolls back on unhealthy startup
[ ] WiFi connection on boot (if configured)
[ ] Network commands via custom CameraCommand
[ ] All unit tests pass
```
