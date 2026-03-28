# Cellular Failover — Design Document

## Overview

Add WiFi-to-cellular failover so cameras maintain connectivity when WiFi drops. The SIM7600G-H modem provides both LTE and GPS. Failover is handled at two layers: the OS (NetworkManager routing) and the application (interface monitoring + QUIC reconnection).

## Hardware

- **Modem**: SIM7600G-H USB (LTE Cat-4, global bands)
- **SIM**: Pre-configured with APN (carrier-dependent)
- **Interfaces**: `wlan0` (WiFi, metric 600), `wwan0` (cellular, metric 700)
- WiFi is preferred when available due to lower metric

## Architecture

```
           ┌─────────┐        ┌─────────┐
           │  wlan0  │        │  wwan0  │
           │ (WiFi)  │        │ (LTE)   │
           └────┬────┘        └────┬────┘
                │                   │
                └───────┬───────────┘
                        │
              /proc/net/route (kernel)
                        │
              NetworkMonitor (500ms poll)
                        │ watch::channel
                        ▼
              Connection Loop (camera main)
                        │
              ├── detect interface change
              ├── reconnect QUIC to server
              └── ABR starts at minimum tier
```

## OS Layer: NetworkManager Configuration

### Problem

When WiFi drops, NetworkManager removes the cellular default route — even though `wwan0` is still connected. This breaks all outbound traffic until NM decides to re-add the route (which can take 30-60s or not happen at all).

### Solution: Dispatcher Script

`pi/networkmanager/99-keep-cellular-route`:

```bash
#!/bin/bash
# Re-add cellular default route when WiFi drops
IFACE="$1"
ACTION="$2"

if [ "$IFACE" = "wlan0" ] && [ "$ACTION" = "down" ]; then
    GW=$(ip route show dev wwan0 | grep default | awk '{print $3}')
    if [ -n "$GW" ]; then
        ip route add default via "$GW" dev wwan0 metric 700 2>/dev/null
        logger -t ghostcam "WiFi down — restored cellular route via $GW"
    fi
fi
```

Deployed to `/etc/NetworkManager/dispatcher.d/99-keep-cellular-route` (must be `chmod 755`, owned by root).

### Disable Connectivity Checks

`pi/networkmanager/no-connectivity-check.conf`:

```ini
[connectivity]
enabled=false
```

Without this, NetworkManager polls a connectivity URL every 10-15s. When WiFi is down, these checks cause the cellular modem to cycle (disconnect/reconnect), disrupting active QUIC connections.

Deployed to `/etc/NetworkManager/conf.d/no-connectivity-check.conf`.

## Application Layer: Network Monitor

### Interface Change Detection

A background task polls `/proc/net/route` every 500ms to detect default route changes:

```rust
async fn network_monitor(change_tx: watch::Sender<()>) {
    let mut last_iface: Option<String> = None;
    let mut debounce_until: Option<Instant> = None;

    loop {
        tokio::time::sleep(Duration::from_millis(500)).await;

        let current = read_default_interface().await;

        // Debounce: ignore changes within 1s of last change
        if let Some(until) = debounce_until {
            if Instant::now() < until { continue; }
            debounce_until = None;
        }

        // Only signal on real interface transitions (not drops to None)
        if current != last_iface && current.is_some() {
            tracing::info!(
                from = ?last_iface,
                to = ?current,
                "network interface changed"
            );
            last_iface = current;
            debounce_until = Some(Instant::now() + Duration::from_secs(1));
            let _ = change_tx.send(());
        } else if current.is_none() && last_iface.is_some() {
            // Interface dropped — don't signal yet, wait for new route
            last_iface = None;
        }
    }
}
```

### `/proc/net/route` Parsing

```rust
async fn read_default_interface() -> Option<String> {
    let content = tokio::fs::read_to_string("/proc/net/route").await.ok()?;
    for line in content.lines().skip(1) {
        let fields: Vec<&str> = line.split('\t').collect();
        if fields.len() >= 2 && fields[1] == "00000000" {
            return Some(fields[0].to_string());
        }
    }
    None
}
```

`00000000` in the destination column = default route. The first match is the active default route.

### Non-Linux Fallback

On macOS (development), the monitor is a no-op — it never signals changes. Network failover is a Linux/Pi-only feature.

## Connection Loop Integration

The camera's main reconnection loop gains network awareness:

```rust
loop {
    // 1. Wait for a default route to exist
    wait_for_route().await;

    // 2. Connect to server
    let session = match connect_with_backoff(&config, &cancel).await {
        Some(s) => s,
        None => continue, // cancelled
    };

    // 3. Stream loop with network change detection
    loop {
        tokio::select! {
            msg = capture_rx.recv() => {
                match msg {
                    Some(frame) => {
                        if send_frame(&session, frame).await.is_err() {
                            break; // connection dead, reconnect
                        }
                    }
                    None => return, // capture ended, shutdown
                }
            }
            _ = net_change_rx.changed() => {
                tracing::info!("network change detected, reconnecting");
                break; // drop current session, reconnect on new interface
            }
            _ = tokio::time::sleep(Duration::from_secs(5)) => {
                // Send timeout — connection is dead but hasn't errored yet
                tracing::warn!("send timeout, reconnecting");
                break;
            }
        }
    }

    // Drain buffered frames during reconnection (prevent backpressure on capture)
    while capture_rx.try_recv().is_ok() {}
}
```

### Wait for Route

Before attempting to connect, wait until a default route exists:

```rust
async fn wait_for_route() {
    loop {
        if read_default_interface().await.is_some() {
            return;
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }
}
```

This handles the gap between WiFi dropping and cellular route being established (~5-10s).

### 5-Second Send Timeout

QUIC connections can hang for 30s+ before detecting a dead link (especially over cellular). The `select!` with a 5s timeout ensures we detect dead connections quickly and reconnect rather than waiting for QUIC's idle timeout.

## ABR Integration

When reconnecting after a network change, ABR starts at the **minimum** tier (500 Kbps). This is critical for cellular where bandwidth is unpredictable. The ABR controller ramps up as throughput is proven.

See `video-capture.md` for ABR tier definitions.

## Failover Timeline

```
T=0s     WiFi drops
T=0.5s   NetworkMonitor detects route gone (poll interval)
T=0.5s   NM dispatcher re-adds cellular route (99-keep-cellular-route)
T=1.0s   NetworkMonitor detects new route on wwan0
T=1.5s   Debounce settles, watch::channel signals
T=2.0s   Camera drops QUIC session, starts reconnect
T=2-5s   QUIC handshake + mTLS on cellular interface
T=5-10s  First frames flowing on cellular
T=10s+   ABR ramps up from minimum tier

Total failover: ~10-25 seconds
```

## Configuration

No new camera config needed. The network monitor runs automatically on Linux when a default route exists. Cellular setup is OS-level (NetworkManager + SIM).

Server-side: no changes. The server sees the camera disconnect and reconnect — same as any other network interruption. The camera's device identity (mTLS cert) ensures seamless re-authentication.

## System Setup (via camera manager)

The `scripts/pi.sh setup` command deploys:

1. **NM dispatcher script**: `pi/networkmanager/99-keep-cellular-route` → `/etc/NetworkManager/dispatcher.d/`
2. **NM config**: `pi/networkmanager/no-connectivity-check.conf` → `/etc/NetworkManager/conf.d/`
3. **Packages**: `modemmanager`, `network-manager`, `libqmi-utils`, `usb-modeswitch`

SIM/APN configuration is carrier-specific and assumed to be pre-configured on the SIM or set via `nmcli connection add type gsm`.

## Dependencies

No new Rust crate dependencies. Uses `tokio::fs`, `tokio::time`, `tokio::sync::watch` (all already available).

## Failure Modes

| Scenario | Behavior |
|----------|----------|
| WiFi drops, cellular available | Failover in ~10-25s |
| WiFi drops, no cellular | Camera waits for any route, retries indefinitely |
| Both interfaces down | Camera buffers telemetry to disk, drops video/audio frames, waits for route |
| Cellular modem crashes | ModemManager restarts it; NM re-establishes connection; camera detects new route |
| WiFi returns after cellular failover | NM prefers WiFi (lower metric); camera detects route change, reconnects over WiFi |
| Rapid WiFi flapping | 1s debounce prevents reconnect storm |
