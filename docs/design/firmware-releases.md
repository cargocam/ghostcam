# Firmware Release System — Design Document

## Overview

Cameras self-update on startup by asking the server for the latest firmware version. The server's role is to know what the latest release is (via GitHub webhook) and tell cameras where to get it. The server never pushes binaries or tracks camera versions persistently.

## Design Principles

- **Stateless**: The server does not store or track per-camera firmware versions. Each camera knows its own compiled-in version and decides whether to update.
- **Pull-based updates**: Cameras ask the server for the latest version, then download binaries from GitHub Releases. The server triggers restarts but never transfers firmware.
- **Event-driven**: A GitHub webhook notifies the server of new releases. The server fans out reboot commands to cameras.
- **Single endpoint**: Cameras only need to know the server address. The server proxies release metadata so cameras never hit GitHub directly.

## Architecture

```
GitHub Release ──webhook──► Server ──Redis pub/sub──► All Server Instances
                                                          │
                                          ┌───────────────┼───────────────┐
                                          ▼               ▼               ▼
                                     Camera A        Camera B        Camera C
                                     (reboot)       (reboot)       (reboot)
                                          │               │               │
                                          ▼               ▼               ▼
                              Boot → GET /firmware/latest (enrolled server)
                                   → fallback: GET /firmware/latest (cloud)
                                   → download binary from GitHub if newer
                                   → verify, swap, restart
```

## Server: Firmware Metadata Endpoint

### `GET /api/v1/firmware/latest`

Public endpoint (no auth — cameras call this before completing handshake). Returns the latest known release:

```json
{
  "version": "1.2.0",
  "assets": {
    "aarch64": {
      "url": "https://github.com/cargocam/ghostcam/releases/download/v1.2.0/ghostcam-camera-aarch64",
      "sha256": "abc123..."
    },
    "x86_64": {
      "url": "https://github.com/cargocam/ghostcam/releases/download/v1.2.0/ghostcam-camera-x86_64",
      "sha256": "def456..."
    }
  }
}
```

If no release is known (webhook never received, server just started), returns:

```json
{
  "version": null
}
```

The camera treats a `null` version as "no update available" and proceeds normally.

### Where the Server Gets This Data

The server holds the latest release info **in memory only** — populated by:
1. GitHub webhook (primary, real-time)
2. On startup, a one-time fetch of `GET https://api.github.com/repos/{owner}/{repo}/releases/latest` (so the server knows the current release even if it missed the webhook)

This means the server needs `GHOSTCAM_RELEASE_REPO` configured, but cameras do not.

## Camera Startup Update Check

On every startup, before connecting to the server, the camera:

1. Calls `GET http://{server}/api/v1/firmware/latest` (enrolled server address)
2. If the enrolled server is unreachable (timeout, DNS failure, connection refused), falls back to the **cloud URL**: `GET https://{cloud}/api/v1/firmware/latest`
3. Compares `version` against its compiled-in `CARGO_PKG_VERSION` (semver, stripped `v` prefix)
4. If newer: downloads the correct architecture binary from the `url` in the response, verifies SHA-256, performs atomic swap, exits for watchdog restart
5. If current/newer or `version: null`: proceeds with normal startup, connects to server

### Firmware Check URL Resolution

The camera tries two sources in order:

1. **Enrolled server** — the server address stored from enrollment (self-hosted or cloud). This is the normal path.
2. **Cloud fallback** — a hardcoded URL compiled into the binary. This ensures cameras can always update even if their enrolled server is down, decommissioned, or running an older server version that doesn't have the `/firmware/latest` endpoint yet.

The cloud URL is set at compile time:
- **Build-time env var**: `GHOSTCAM_CLOUD_URL` (e.g., `https://api.ghostcam.io`)
- Baked in via `option_env!("GHOSTCAM_CLOUD_URL")` — if unset at build time, the fallback is disabled (open-source/self-hosted builds)
- The release CI workflow sets this var so all official binaries have the fallback

The fallback is only tried if the enrolled server fails the firmware check (5s timeout). It is **not** tried if the enrolled server responds successfully, even with `version: null`.

### Reusing Existing Infrastructure

The camera's `firmware.rs` already handles:
- Binary download (curl) with timeout
- SHA-256 verification
- Atomic swap (`current` ← new, `previous` ← old)
- Health sentinel (`firmware/healthy`) for rollback detection
- Alert lifecycle (`UpdateApplying`, `UpdateSucceeded`, `UpdateFailed`)

The startup check reuses this same code path. The only new logic is the `/firmware/latest` call and version comparison.

## Server: GitHub Webhook

### Endpoint

```
POST /api/v1/webhooks/github
```

- Verifies the `X-Hub-Signature-256` header using `GITHUB_WEBHOOK_SECRET`
- Filters for `release` events with `action: "published"`
- Extracts the release tag and asset URLs
- Fetches `checksums.txt` from the release assets, parses SHA-256 values
- Updates in-memory latest release
- Publishes to Redis channel `ghostcam:firmware:release`

### Redis Fan-Out

When a server instance receives the webhook, it publishes to Redis pub/sub:

```
PUBLISH ghostcam:firmware:release {"version": "1.2.0", "assets": {...}}
```

All server instances subscribe to this channel on startup. On receiving the message, each instance:
1. Updates its in-memory latest release
2. Initiates a staggered reboot of its connected cameras

### Why Redis Pub/Sub (Not Streams)

Pub/sub is fire-and-forget — if an instance is down when the release happens, it doesn't matter. It fetches the latest release from GitHub on startup anyway. No need for durable message delivery.

## Reboot Command

The existing `Reboot` command in the wire protocol is used as-is:

```rust
Reboot { seq: u64 }
```

Hard reboot only — immediate `exit(0)`, watchdog/systemd restarts the process. On restart, the camera hits `/api/v1/firmware/latest` and picks up the new version.

No soft reboot mode. Cameras are designed to restart cleanly at any time (segments are atomic, telemetry is buffered, QUIC handles connection drops). Overcomplicating restart semantics isn't worth it.

## Restart Scenarios

### 1. Camera powers on (no server involvement in triggering)

Camera startup → `GET /api/v1/firmware/latest` → update if newer → connect to server.

### 2. Offline camera reconnects

During the QUIC handshake, the camera sends `fw_version` in `Alert::Handshake`. The server compares against its in-memory latest release version. If the camera's version is older:

- Server sends `Reboot { seq }` immediately after handshake
- Camera restarts, hits `/api/v1/firmware/latest`, updates, comes back on the new version

Best-effort: if the server doesn't have a latest release in memory (missed webhook + GitHub fetch failed on startup), the camera proceeds normally and will update on its next natural restart.

### 3. Online cameras (staggered reboot)

When the server receives a release notification (via Redis pub/sub), it schedules reboots for all connected cameras:

- Each camera gets a random delay: `rand(0..stagger_window)`
- Default `stagger_window`: 5 minutes (configurable via `GHOSTCAM_UPDATE_STAGGER_SECS`, default 300)

The stagger prevents:
- Thundering herd on GitHub's release asset CDN
- All cameras going offline simultaneously (surveillance gap)

### Distributed Stagger Coordination

Each server instance independently staggers its own connected cameras. No cross-instance coordination needed — cameras are distributed across instances, so each instance handling its own subset is sufficient.

## Configuration

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_RELEASE_REPO` | _(none)_ | GitHub `owner/repo`. Unset = firmware endpoint returns `null`, no webhook processing |
| `GITHUB_WEBHOOK_SECRET` | _(none)_ | Webhook signature verification. Unset = webhook endpoint disabled |
| `GHOSTCAM_UPDATE_STAGGER_SECS` | `300` | Window over which to spread reboot commands |

### Camera

No runtime configuration needed. The camera uses its enrolled server address for update checks.

The cloud fallback URL is a **compile-time** constant, not runtime config:

| Build-time Env Var | Default | Description |
|---------------------|---------|-------------|
| `GHOSTCAM_CLOUD_URL` | _(none)_ | Cloud API base URL baked into binary. Unset = no fallback (self-hosted builds) |

This is set in the GitHub Actions release workflow, so official binaries always have the fallback. Self-hosted builds from source won't have it unless explicitly set, which is the correct behavior — a self-hosted deployment shouldn't phone home to Ghostcam cloud.

## Wire Protocol Changes

### Removed

- `UpdateAvailable` command — no longer needed. Cameras pull updates themselves.
- `UpdateFailReason::Watchdog` — only `HashMismatch` and `DownloadFailed` remain relevant.

### Unchanged

- `Reboot { seq }` — used as-is for triggering restarts
- `UpdateApplying`, `UpdateSucceeded`, `UpdateFailed` alerts — still sent by camera during the update flow so the server can log progress

## Failure Modes

| Scenario | Behavior |
|----------|----------|
| Enrolled server unreachable on camera boot | Camera falls back to cloud URL. If cloud also unreachable, skips update check and starts normally |
| Enrolled server responds but cloud is unreachable | No issue — cloud is only tried if enrolled server fails |
| Server returns `version: null` | Camera starts normally (no update available) |
| Binary download fails | Camera logs error, starts with current version |
| SHA-256 mismatch | Camera rejects binary, starts with current version |
| New binary crashes on start | Health sentinel missing → watchdog rolls back to `firmware/previous` |
| Webhook replayed/duplicated | Cameras receive duplicate reboot — idempotent (already restarting or already updated) |
| Server missed pub/sub (was down) | Startup fetch from GitHub API populates latest release. Cameras update on next restart |
| Camera mid-update when server restarts | Camera completes update independently — no server involvement in the download |
| GitHub API down on server startup | Server starts with no known release. Cameras proceed without updates until webhook arrives |

## Sequence Diagram

### New Release → Online Camera Update

```
GitHub                Server              Redis             Server(all)         Camera
  │                     │                   │                   │                 │
  ├──webhook POST──────►│                   │                   │                 │
  │                     ├──PUBLISH──────────►│                   │                 │
  │                     │                   ├──notify───────────►│                 │
  │                     │                   │                   ├─schedule─┐       │
  │                     │                   │                   │  stagger │       │
  │                     │                   │                   │◄─────────┘       │
  │                     │                   │                   ├──Reboot─────────►│
  │                     │                   │                   │                 ├─exit(0)
  │                     │                   │                   │                 │
  │                     │                   │                   │          [watchdog restart]
  │                     │                   │                   │                 │
  │                     │                   │   GET /firmware/latest◄─────────────┤
  │                     │                   │   {version, assets}────────────────►│
  │                     │                   │      (or fallback to cloud URL      │
  │                     │                   │       if enrolled server fails)     │
  │                     │                   │                   │                 ├─compare versions
  │  ◄───────────────────────────────────────────────GET binary + checksums──────┤
  │  ────────────────────────────────────────────────────binary bytes───────────►│
  │                     │                   │                   │                 ├─verify + swap
  │                     │                   │                   │                 ├─exit(0)
  │                     │                   │                   │                 │
  │                     │                   │                   │          [watchdog restart]
  │                     │                   │                   │                 │
  │                     │                   │  GET /firmware/latest◄──────────────┤
  │                     │                   │  (version matches)────────────────►│
  │                     │                   │                   │                 ├─skip update
  │                     │                   │                   ◄──QUIC connect───┤
```

## Implementation Order

1. **Server firmware metadata endpoint** — `GET /api/v1/firmware/latest`, in-memory latest release, startup fetch from GitHub API
2. **Camera startup update check** — call enrolled server, cloud fallback, version comparison, trigger existing `firmware.rs` flow
3. **Release workflow: `GHOSTCAM_CLOUD_URL`** — add env var to `release.yml` build step so official binaries have the cloud fallback baked in
4. **Remove `UpdateAvailable`** — delete from wire protocol and camera command handler
5. **GitHub webhook endpoint** — signature verification, parse release, update in-memory state, Redis publish
6. **Redis subscription + staggered reboot** — server subscribes on startup, schedules camera reboots on release notification
7. **Reconnection check** — compare `fw_version` from handshake against latest known release, send `Reboot` if stale
8. **Config + docs** — environment variables, CLAUDE.md updates, example TOML updates
