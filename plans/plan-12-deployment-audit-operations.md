# Plan 12: Deployment, Audit & Operations

## Overview

This plan covers audit logging, CI/CD, Docker images, Fly.io deployment for server-multi, GitHub Releases for camera firmware, the camera self-update mechanism, and operational tooling (backup/restore, reboot command). It ties together the full deployment pipeline from code push to running system.

**Depends on**: All previous plans (this is the final plan)

## 1. Audit Logging

### 1.1 Audit Events

Carry forward HMAC-SHA256 signed audit trail from existing codebase. Expanded event set for the rewrite:

```rust
pub enum AuditEvent {
    // Auth
    AuthSuccess { user_id: String, ip: String },
    AuthFailure { username: String, ip: String },

    // Camera lifecycle
    CameraConnected { device_id: String, ip: String, firmware_version: String },
    CameraDisconnected { device_id: String, reason: String },

    // Enrollment
    EnrollmentStarted { device_id: String, owner_id: String },
    EnrollmentCompleted { device_id: String, owner_id: String },
    EnrollmentRejected { device_id: String, reason: String },

    // Camera management
    CameraRenamed { device_id: String, old_name: String, new_name: String },
    CameraRebooted { device_id: String, initiated_by: String },
    CameraUnregistered { device_id: String, initiated_by: String },
    CameraCommandSent { device_id: String, command_type: String },

    // Group/config
    CameraGroupChanged { device_id: String, old_group: String, new_group: String },

    // Session
    SessionCreated { session_id: String, device_id: String, viewer_ip: String },
    SessionDestroyed { session_id: String },

    // API tokens
    TokenCreated { token_name: String, created_by: String },
    TokenRevoked { token_name: String, revoked_by: String },

    // Server
    ServerStarted { version: String },
    ServerStopped {},
}
```

### 1.2 Audit Entry Format

```rust
pub struct AuditEntry {
    pub timestamp: chrono::DateTime<chrono::Utc>,
    pub event: AuditEvent,
    pub hmac: String, // HMAC-SHA256 hex, chained (includes previous entry's HMAC)
}
```

HMAC chaining: each entry's HMAC input includes the previous entry's HMAC, creating a tamper-evident chain. First entry uses a zero HMAC.

### 1.3 Audit Log Output

Append-only file, one JSON line per entry:

```
{"timestamp":"2026-03-19T10:00:00Z","event":{"type":"server_started","version":"0.1.0"},"hmac":"abc123..."}
{"timestamp":"2026-03-19T10:00:01Z","event":{"type":"camera_connected","device_id":"cam-01","ip":"192.168.1.50","firmware_version":"0.1.0"},"hmac":"def456..."}
```

Log path configurable via `--audit-log` CLI flag (default: `audit.jsonl` in working directory). The `AuditLogger` writes via a `tokio::sync::mpsc` channel to avoid blocking request handlers.

### 1.4 Audit Verification CLI

A subcommand to verify the HMAC chain:

```bash
ghostcam-server verify-audit --hmac-key <key> audit.jsonl
```

Reads the file, recomputes each HMAC in sequence, reports any broken links.

## 2. Reboot Command

### 2.1 Command Variant

Add to `Command` enum (Plan 1):

```rust
Reboot {}
```

### 2.2 Camera Handler

```rust
Command::Reboot => {
    info!("Received reboot command — shutting down for restart");
    // Clean exit — systemd/watchdog will restart
    std::process::exit(0);
}
```

### 2.3 API

Operators trigger reboot via the existing command endpoint:

```
POST /api/v1/cameras/{device_id}/command
{
    "type": "reboot"
}
```

Returns 202 Accepted. The camera exits, watchdog/systemd restarts it, and the startup update check runs.

## 3. Camera Self-Update

### 3.1 Update Check on Startup

Before entering the normal connection loop, the camera checks for firmware updates:

```rust
async fn check_for_update(config: &Config) -> Result<Option<UpdateInfo>> {
    let update_url = config.update_url.as_deref()
        .unwrap_or("https://api.github.com/repos/OWNER/ghostcam/releases/latest");

    let current_version = env!("CARGO_PKG_VERSION");

    // Fetch latest release metadata
    let client = reqwest::Client::new();
    let release: GithubRelease = client
        .get(update_url)
        .header("User-Agent", "ghostcam-camera")
        .send()
        .await?
        .json()
        .await?;

    let latest_version = release.tag_name.trim_start_matches('v');

    // Compare versions (semver)
    if !is_newer(latest_version, current_version) {
        info!(current = current_version, latest = latest_version, "Firmware is up to date");
        return Ok(None);
    }

    info!(current = current_version, latest = latest_version, "Firmware update available");

    // Find the binary asset for this platform
    let arch = std::env::consts::ARCH; // "aarch64", "x86_64"
    let binary_name = format!("ghostcam-camera-{arch}");
    let checksum_name = "checksums.txt";

    let binary_asset = release.assets.iter()
        .find(|a| a.name == binary_name)
        .ok_or_else(|| anyhow::anyhow!("No binary asset for {binary_name}"))?;

    let checksum_asset = release.assets.iter()
        .find(|a| a.name == checksum_name)
        .ok_or_else(|| anyhow::anyhow!("No checksums asset"))?;

    // Fetch checksums
    let checksums_text = client
        .get(&checksum_asset.browser_download_url)
        .header("User-Agent", "ghostcam-camera")
        .send()
        .await?
        .text()
        .await?;

    // Parse "sha256  filename" lines
    let expected_sha256 = checksums_text
        .lines()
        .find(|line| line.ends_with(&binary_name))
        .and_then(|line| line.split_whitespace().next())
        .ok_or_else(|| anyhow::anyhow!("No checksum for {binary_name}"))?;

    Ok(Some(UpdateInfo {
        version: latest_version.to_string(),
        download_url: binary_asset.browser_download_url.clone(),
        sha256: expected_sha256.to_string(),
    }))
}
```

### 3.2 Update Application

```rust
async fn apply_update(info: &UpdateInfo, config: &Config) -> Result<()> {
    info!(version = %info.version, "Downloading firmware update");

    let client = reqwest::Client::new();
    let bytes = client
        .get(&info.download_url)
        .header("User-Agent", "ghostcam-camera")
        .send()
        .await?
        .bytes()
        .await?;

    // Verify SHA-256
    use ring::digest;
    let actual = digest::digest(&digest::SHA256, &bytes);
    let actual_hex = hex::encode(actual.as_ref());

    if actual_hex != info.sha256 {
        anyhow::bail!(
            "Firmware hash mismatch: expected {}, got {actual_hex}",
            info.sha256
        );
    }

    let firmware_dir = config.config_dir.join("firmware");
    tokio::fs::create_dir_all(&firmware_dir).await?;

    let temp_path = firmware_dir.join("downloading");
    let current_path = firmware_dir.join("current");
    let previous_path = firmware_dir.join("previous");

    // Write to temp
    tokio::fs::write(&temp_path, &bytes).await?;

    // Set executable
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        tokio::fs::set_permissions(&temp_path, std::fs::Permissions::from_mode(0o755)).await?;
    }

    // Atomic swap
    if current_path.exists() {
        tokio::fs::rename(&current_path, &previous_path).await?;
    }
    tokio::fs::rename(&temp_path, &current_path).await?;

    // Clear health sentinel
    let _ = tokio::fs::remove_file(firmware_dir.join("healthy")).await;

    info!(version = %info.version, "Firmware updated — re-executing");

    // Re-exec with new binary
    let err = exec::execvp(&current_path, &std::env::args().collect::<Vec<_>>());
    anyhow::bail!("exec failed: {err}");
}
```

### 3.3 Version Pinning

In `ghostcam.conf`:

```toml
[firmware]
update_url = "https://api.github.com/repos/OWNER/ghostcam/releases/latest"
pin_version = "1.2.3"  # Optional: pin to specific version instead of latest
```

If `pin_version` is set, the camera fetches that specific release instead of latest:

```rust
let update_url = if let Some(pinned) = &config.firmware.pin_version {
    format!("https://api.github.com/repos/OWNER/ghostcam/releases/tags/v{pinned}")
} else {
    config.firmware.update_url.clone()
};
```

This enables staged rollouts: update `ghostcam.conf` on a subset of cameras to pin the new version, then reboot them.

### 3.4 Startup Flow (Updated)

```
main() startup:
  1. Parse CLI args + load ghostcam.conf
  2. Check for firmware update (§3.1)
     - Update available → download, verify, swap, re-exec (§3.2)
     - Up to date or no network → continue
  3. Write health sentinel (watchdog success)
  4. Check for user.crt (Plan 9)
     - Missing → registration mode
     - Present → normal connect loop
```

### 3.5 Version Reporting

Camera includes firmware version in DeviceHello:

```rust
pub struct DeviceHello {
    pub device_id: String,
    pub capabilities: Vec<String>,
    pub firmware_version: String,  // env!("CARGO_PKG_VERSION")
}
```

Server stores `firmware_version` per camera, visible in:
- `GET /api/v1/cameras/{device_id}/status`
- Dashboard per-camera table

### 3.6 Watchdog Script

Same as Plan 9 §8. Key behavior:
- Removes health sentinel before starting camera
- Waits 60s for sentinel to appear
- If no sentinel and a previous binary exists, rolls back
- Clean exit (code 0) → immediate restart (update case)
- Non-zero exit → restart after 5s

## 4. CI Pipeline

### 4.1 CI (Push/PR to main)

`.github/workflows/ci.yml`:

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  rust:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
        with:
          components: rustfmt, clippy
      - run: sudo apt-get install -y libasound2-dev libopus-dev
      - run: cargo fmt --all -- --check
      - run: cargo clippy --all-targets -- -D warnings
      - run: cargo test --all

  ui:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: oven-sh/setup-bun@v2
      - run: cd ui && bun install --frozen-lockfile
      - run: cd ui && bun run check
      - run: cd ui && bun run build

  docker:
    needs: [rust, ui]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - run: docker build --target server-solo -t ghostcam-server-solo .
      - run: docker build --target server-multi -t ghostcam-server-multi .
      - run: docker build --target camera -t ghostcam-camera .
```

### 4.2 Release (Tag Push)

`.github/workflows/release.yml`:

```yaml
name: Release
on:
  push:
    tags: ['v*']

permissions:
  contents: write
  packages: write

env:
  REGISTRY: ghcr.io
  IMAGE_PREFIX: ghcr.io/${{ github.repository_owner }}/ghostcam

jobs:
  build-camera:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - target: aarch64-unknown-linux-gnu
            name: ghostcam-camera-aarch64
          - target: x86_64-unknown-linux-gnu
            name: ghostcam-camera-x86_64
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
        with:
          targets: ${{ matrix.target }}
      - name: Install cross-compilation deps
        run: |
          sudo apt-get install -y gcc-aarch64-linux-gnu libasound2-dev libopus-dev
      - name: Build camera binary
        run: cargo build --release --target ${{ matrix.target }} -p camera
      - name: Rename binary
        run: cp target/${{ matrix.target }}/release/camera ${{ matrix.name }}
      - name: Upload artifact
        uses: actions/upload-artifact@v4
        with:
          name: ${{ matrix.name }}
          path: ${{ matrix.name }}

  build-docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build and push server-solo
        uses: docker/build-push-action@v5
        with:
          push: true
          target: server-solo
          tags: ${{ env.IMAGE_PREFIX }}-server-solo:${{ github.ref_name }},${{ env.IMAGE_PREFIX }}-server-solo:latest
          cache-from: type=gha
          cache-to: type=gha,mode=max
      - name: Build and push server-multi
        uses: docker/build-push-action@v5
        with:
          push: true
          target: server-multi
          tags: ${{ env.IMAGE_PREFIX }}-server-multi:${{ github.ref_name }},${{ env.IMAGE_PREFIX }}-server-multi:latest
          cache-from: type=gha
          cache-to: type=gha,mode=max

  release:
    needs: [build-camera, build-docker]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          merge-multiple: true
      - name: Generate checksums
        run: sha256sum ghostcam-camera-* > checksums.txt
      - name: Create GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          files: |
            ghostcam-camera-aarch64
            ghostcam-camera-x86_64
            checksums.txt
          generate_release_notes: true
```

## 5. Docker

### 5.1 Dockerfile

Multi-stage build with three targets:

```dockerfile
# --- Chef stage (dependency caching) ---
FROM rust:1.82-bookworm AS chef
RUN cargo install cargo-chef
WORKDIR /app

FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS builder
COPY --from=planner /app/recipe.json recipe.json
RUN apt-get update && apt-get install -y libasound2-dev libopus-dev
RUN cargo chef cook --release --recipe-path recipe.json
COPY . .
RUN cargo build --release -p server-solo -p server-multi

# --- UI build ---
FROM oven/bun:1 AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/bun.lockb ./
RUN bun install --frozen-lockfile
COPY ui/ .
RUN bun run build

# --- Test data ---
FROM debian:bookworm-slim AS test-data
RUN apt-get update && apt-get install -y ffmpeg
WORKDIR /data
RUN ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test.h264

# --- server-solo target ---
FROM debian:bookworm-slim AS server-solo
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/server-solo /usr/local/bin/ghostcam-server-solo
COPY --from=ui-builder /app/ui/build /app/static
COPY --from=test-data /data/test.h264 /app/test-data/test.h264
EXPOSE 3000 4433/udp
ENTRYPOINT ["ghostcam-server-solo"]

# --- server-multi target ---
FROM debian:bookworm-slim AS server-multi
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/server-multi /usr/local/bin/ghostcam-server-multi
COPY --from=ui-builder /app/ui/build /app/static
EXPOSE 3000 4433/udp
ENTRYPOINT ["ghostcam-server-multi"]

# --- camera target (for testing) ---
FROM debian:bookworm-slim AS camera
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/camera /usr/local/bin/ghostcam-camera
COPY --from=test-data /data/test.h264 /app/test-data/test.h264
ENTRYPOINT ["ghostcam-camera"]
```

### 5.2 docker-compose.yml (Local Dev)

```yaml
version: "3.8"

services:
  # Solo mode (simple)
  server-solo:
    build:
      context: .
      target: server-solo
    ports:
      - "3000:3000"
      - "4433:4433/udp"
    volumes:
      - solo-data:/var/lib/ghostcam
    environment:
      - GHOSTCAM_PUBLIC_IP=127.0.0.1
      - GHOSTCAM_API_KEY=dev-key
      - GHOSTCAM_HMAC_KEY=dev-hmac-key
    command: ["--public-ip", "127.0.0.1", "--static-dir", "/app/static"]

  # Test cameras
  camera-1:
    build:
      context: .
      target: camera
    depends_on: [server-solo]
    command: ["--test-source", "--device-id", "cam-01", "--server-addr", "server-solo:4433"]

  camera-2:
    build:
      context: .
      target: camera
    depends_on: [server-solo]
    command: ["--test-source", "--device-id", "cam-02", "--server-addr", "server-solo:4433"]

volumes:
  solo-data:
```

### 5.3 docker-compose.multi.yml (Multi Mode Local Dev)

```yaml
version: "3.8"

services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_DB: ghostcam
      POSTGRES_USER: ghostcam
      POSTGRES_PASSWORD: dev-password
    ports:
      - "5432:5432"
    volumes:
      - pg-data:/var/lib/postgresql/data

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  server-multi:
    build:
      context: .
      target: server-multi
    ports:
      - "3000:3000"
      - "4433:4433/udp"
    depends_on: [postgres, redis]
    environment:
      - GHOSTCAM_PUBLIC_IP=127.0.0.1
      - GHOSTCAM_DATABASE_URL=postgres://ghostcam:dev-password@postgres:5432/ghostcam
      - GHOSTCAM_REDIS_URL=redis://redis:6379
      - GHOSTCAM_API_KEY=dev-key
      - GHOSTCAM_HMAC_KEY=dev-hmac-key
    command: ["--public-ip", "127.0.0.1", "--static-dir", "/app/static"]

  camera-1:
    build:
      context: .
      target: camera
    depends_on: [server-multi]
    command: ["--test-source", "--device-id", "cam-01", "--server-addr", "server-multi:4433"]

volumes:
  pg-data:
```

## 6. Fly.io Deployment (server-multi)

### 6.1 fly.toml

```toml
app = "ghostcam"
primary_region = "ord"

[build]
  dockerfile = "Dockerfile"
  target = "server-multi"

[env]
  GHOSTCAM_PUBLIC_IP = "0.0.0.0"  # Fly handles external IP
  RUST_LOG = "server_multi=info,server_core=info,str0m=warn"

[http_service]
  internal_port = 3000
  force_https = true
  auto_stop_machines = "stop"
  auto_start_machines = true
  min_machines_running = 1

[[services]]
  protocol = "udp"
  internal_port = 4433
  [[services.ports]]
    port = 4433

[checks]
  [checks.health]
    type = "http"
    port = 3000
    path = "/healthz"
    interval = "10s"
    timeout = "5s"
```

### 6.2 Secrets

Set via `fly secrets set`:

```bash
fly secrets set \
  GHOSTCAM_DATABASE_URL="postgres://..." \
  GHOSTCAM_REDIS_URL="redis://..." \
  GHOSTCAM_API_KEY="..." \
  GHOSTCAM_HMAC_KEY="..."
```

### 6.3 Deploy

```bash
fly deploy
```

Or automated via GitHub Actions (optional step in release workflow):

```yaml
  deploy-fly:
    needs: [build-docker]
    runs-on: ubuntu-latest
    if: github.event_name == 'push' && startsWith(github.ref, 'refs/tags/')
    steps:
      - uses: actions/checkout@v4
      - uses: superfly/flyctl-actions/setup-flyctl@master
      - run: flyctl deploy --remote-only
        env:
          FLY_API_TOKEN: ${{ secrets.FLY_API_TOKEN }}
```

## 7. Backup / Restore (server-solo)

### 7.1 Backup

SQLite backup via `VACUUM INTO`:

```rust
// In server-solo CLI
#[derive(clap::Subcommand)]
enum Commands {
    /// Start the server
    Serve { /* ... */ },
    /// Backup the database
    Backup {
        /// Output path for backup file
        output: PathBuf,
    },
    /// Verify audit log integrity
    VerifyAudit {
        /// Path to audit log file
        log: PathBuf,
    },
}

async fn backup(db_path: &Path, output: &Path) -> Result<()> {
    let conn = rusqlite::Connection::open(db_path)?;
    conn.execute(&format!("VACUUM INTO '{}'", output.display()), [])?;
    info!(output = %output.display(), "Database backup created");
    Ok(())
}
```

Usage:

```bash
# From host, exec into container
docker exec ghostcam-server-solo ghostcam-server-solo backup /var/lib/ghostcam/backup-2026-03-19.db

# Or mount backup volume
docker run --rm -v solo-data:/data -v ./backups:/backups \
  ghostcam-server-solo backup /data/ghostcam.db /backups/backup-$(date +%Y%m%d).db
```

### 7.2 Restore

Replace the database file:

```bash
docker stop ghostcam-server-solo
cp backup-2026-03-19.db /path/to/volume/ghostcam.db
docker start ghostcam-server-solo
```

## 8. Wire Protocol Additions

### 8.1 New Command Variant

Add to `Command` enum:

```rust
Reboot {}
```

Remove the `FirmwareUpdate` variant from Plan 9 — no longer needed.

### 8.2 DeviceHello Update

Add firmware version:

```rust
pub struct DeviceHello {
    pub device_id: String,
    pub capabilities: Vec<String>,
    pub firmware_version: String,
}
```

## 9. Test Plan

### 9.1 Unit Tests — Audit

| # | Test | Validates |
|---|------|-----------|
| 1 | AuditEntry serializes to JSON line | Correct format |
| 2 | HMAC chain: entry N includes entry N-1's HMAC | Chaining works |
| 3 | First entry uses zero HMAC | Bootstrap case |
| 4 | HMAC verification passes for valid chain | verify_audit succeeds |
| 5 | HMAC verification fails for tampered entry | Detects corruption |
| 6 | All AuditEvent variants serialize/deserialize | Roundtrip |

### 9.2 Unit Tests — Self-Update

| # | Test | Validates |
|---|------|-----------|
| 1 | Parse GitHub release JSON | Version, assets, download URLs |
| 2 | Parse checksums.txt format | Correct SHA-256 for platform |
| 3 | Version comparison: newer detected | "1.2.0" > "1.1.0" |
| 4 | Version comparison: same version skipped | "1.1.0" == "1.1.0" |
| 5 | Version comparison: older skipped | "1.0.0" < "1.1.0" |
| 6 | SHA-256 verification passes for correct hash | Update proceeds |
| 7 | SHA-256 verification fails for wrong hash | Error, no swap |
| 8 | Atomic swap: current → previous, new → current | File positions correct |
| 9 | Pin version: fetches specific release tag | Correct URL constructed |
| 10 | No network: update check skipped gracefully | Camera starts normally |

### 9.3 Unit Tests — Reboot Command

| # | Test | Validates |
|---|------|-----------|
| 1 | Reboot command deserializes | `{ "type": "reboot" }` |
| 2 | Reboot command serializes | Roundtrip |

### 9.4 Unit Tests — DeviceHello

| # | Test | Validates |
|---|------|-----------|
| 1 | DeviceHello with firmware_version serializes | Version field present |
| 2 | DeviceHello without firmware_version deserializes (backcompat) | Defaults to "unknown" |

### 9.5 Integration Tests — Docker Build

| # | Test | Validates |
|---|------|-----------|
| 1 | `docker build --target server-solo` succeeds | Image builds |
| 2 | `docker build --target server-multi` succeeds | Image builds |
| 3 | `docker build --target camera` succeeds | Image builds |
| 4 | server-solo container starts and serves /healthz | HTTP 200 |
| 5 | server-multi container starts (with postgres+redis) and serves /healthz | HTTP 200 |

### 9.6 Integration Tests — CI Pipeline

| # | Test | Validates |
|---|------|-----------|
| 1 | Push to main triggers CI | Workflow runs |
| 2 | CI passes on clean codebase | All jobs green |
| 3 | Tag push triggers release | Release workflow runs |
| 4 | Release creates GitHub Release with binaries | Assets present |
| 5 | Release pushes Docker images to GHCR | Images pullable |

### 9.7 Integration Tests — Self-Update (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | Camera starts, finds no update, proceeds normally | No-op update check |
| 2 | Camera starts, finds update, downloads, verifies, re-execs | Full update flow |
| 3 | Operator sends reboot command → camera restarts → update check → new version | Operator-triggered update |
| 4 | Bad checksum in release → camera rejects update | Hash mismatch error |
| 5 | No network on startup → camera starts with current version | Graceful degradation |
| 6 | Watchdog rolls back after unhealthy start | Previous binary restored |
| 7 | Pin version in config → camera fetches specific release | Version pinning works |

### 9.8 Integration Tests — Audit (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | Server writes audit entries on camera connect/disconnect | Entries in log file |
| 2 | Auth failure logged | Event recorded |
| 3 | verify-audit subcommand validates clean log | No errors |
| 4 | Tamper with log entry → verify-audit detects | Chain broken |

### 9.9 Integration Tests — Backup (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | `backup` subcommand creates valid SQLite file | File opens, schema intact |
| 2 | Restore from backup → server starts with restored data | Cameras, config preserved |

### 9.10 Integration Tests — Fly.io (Manual)

| # | Test | Validates |
|---|------|-----------|
| 1 | `fly deploy` succeeds | App running |
| 2 | /healthz returns 200 | Health check passes |
| 3 | Camera connects via QUIC (UDP 4433) | QUIC through Fly works |
| 4 | Viewer loads and streams video | Full E2E through Fly |
| 5 | Auto-stop/start works | Machine stops on idle, starts on request |

### 9.11 Validation Checklist

```
[ ] Audit events logged for all key actions
[ ] HMAC chain is tamper-evident
[ ] verify-audit detects corruption
[ ] Reboot command stops camera process
[ ] Watchdog/systemd restarts camera after reboot
[ ] Camera checks for update on startup
[ ] Update downloaded and SHA-256 verified
[ ] Atomic binary swap works
[ ] Watchdog rolls back on unhealthy start
[ ] Version pinning via ghostcam.conf
[ ] firmware_version reported in DeviceHello
[ ] firmware_version visible in camera status API
[ ] CI runs on push/PR to main
[ ] Release workflow builds binaries + Docker images
[ ] GitHub Release has camera binaries + checksums.txt
[ ] Docker images pushed to GHCR
[ ] server-solo runs in Docker with SQLite volume
[ ] server-multi runs on Fly.io with external Postgres + Redis
[ ] Backup/restore works for server-solo
[ ] docker-compose.yml works for local dev (solo + multi)
[ ] No network on startup → camera starts normally
```
