# GPS Integration — Design Document

## Overview

Replace the GPS stub in `telemetry/sensors.rs` with a real gpsd client that reads position data over TCP. Follows the same pattern proven in Kodama.

## Hardware

- **Modem**: SIM7600G-H USB (provides both cellular and GPS)
- **GPS serial port**: `/dev/ttyUSB1` (second serial port on SIM7600)
- **GPS daemon**: `gpsd` reads NMEA from the serial port, serves JSON on `localhost:2947`

## Architecture

```
SIM7600G-H (/dev/ttyUSB1)
    │ NMEA sentences
    ▼
gpsd daemon (localhost:2947)
    │ JSON (TPV reports)
    ▼
GpsdReader (async TCP client)
    │ GpsData struct
    ▼
Telemetry loop (existing)
    │ SparseTelemetry with GPS fields
    ▼
QUIC datagram / telemetry buffer
```

## Implementation

### GpsdReader

Async TCP client that connects to gpsd and streams position updates:

```rust
pub struct GpsdReader {
    rx: mpsc::Receiver<GpsData>,
}

pub struct GpsData {
    pub latitude: f64,
    pub longitude: f64,
    pub altitude: Option<f64>,
    pub speed: Option<f64>,       // m/s
    pub heading: Option<f64>,     // degrees from true north
    pub fix_mode: u8,             // 2 = 2D, 3 = 3D
}

impl GpsdReader {
    /// Connect to gpsd. Returns None if gpsd is unavailable.
    pub async fn new() -> Option<Self> {
        let stream = match TcpStream::connect("127.0.0.1:2947").await {
            Ok(s) => s,
            Err(e) => {
                tracing::debug!("gpsd not available: {e}");
                return None;
            }
        };
        let (tx, rx) = mpsc::channel(16);
        tokio::spawn(gpsd_read_loop(stream, tx));
        Some(Self { rx })
    }

    /// Get the most recent fix, draining any buffered updates.
    pub fn latest_fix(&mut self) -> Option<GpsData> {
        let mut latest = None;
        while let Ok(fix) = self.rx.try_recv() {
            latest = Some(fix);
        }
        latest
    }
}
```

### gpsd Protocol

gpsd speaks a simple line-based JSON protocol:

1. **Connect** to TCP `127.0.0.1:2947`
2. **Enable watch mode**: send `?WATCH={"enable":true,"json":true}\n`
3. **Read JSON lines**: gpsd streams reports continuously

We only care about **TPV (Time-Position-Velocity)** reports:

```json
{"class":"TPV","mode":3,"lat":37.7749,"lon":-122.4194,"alt":10.5,"speed":0.0,"track":180.0}
```

### TPV Parsing

```rust
fn parse_gpsd_tpv(line: &str) -> Option<GpsData> {
    let v: serde_json::Value = serde_json::from_str(line).ok()?;
    if v.get("class")?.as_str()? != "TPV" { return None; }

    let mode = v.get("mode")?.as_u64()? as u8;
    if mode < 2 { return None; } // no fix

    Some(GpsData {
        latitude: v.get("lat")?.as_f64()?,
        longitude: v.get("lon")?.as_f64()?,
        altitude: v.get("alt").and_then(|v| v.as_f64()),
        speed: v.get("speed").and_then(|v| v.as_f64()),
        heading: v.get("track").and_then(|v| v.as_f64()),
        fix_mode: mode,
    })
}
```

### Integration with Telemetry Loop

The existing telemetry loop in `telemetry/mod.rs` calls `sensors::read_gps()` which currently returns `(None, None, None, None)`. Replace with:

```rust
// At telemetry loop startup:
let mut gps_reader = GpsdReader::new().await;

// Each telemetry cycle:
let gps = gps_reader.as_mut().and_then(|r| r.latest_fix());
if let Some(fix) = &gps {
    sparse.latitude = Some(fix.latitude as f32);
    sparse.longitude = Some(fix.longitude as f32);
    sparse.altitude = Some(fix.altitude.unwrap_or(0.0) as f32);
    // speed and heading go into existing telemetry fields
}
```

Existing sparse telemetry thresholds already handle GPS (0.0001 degree ≈ 11m).

### `--no-gps` Flag

The existing `--no-gps` / `no_gps` config flag skips creating the `GpsdReader`. No changes needed to the flag — it just controls whether we attempt to connect to gpsd.

### Graceful Degradation

- **gpsd not running**: `GpsdReader::new()` returns `None`, telemetry continues without GPS
- **gpsd loses fix**: `parse_gpsd_tpv` returns `None` for mode < 2, GPS fields go to `None` in telemetry
- **gpsd crashes**: Read loop ends, `latest_fix()` returns `None` thereafter. Camera continues.
- **No reconnect**: If gpsd dies and restarts, the camera would need to restart to reconnect. Acceptable for v1 — gpsd is stable.

## System Setup

### Pi Setup (via camera manager `scripts/pi.sh`)

1. **Install packages**: `gpsd gpsd-clients`
2. **Deploy gpsd config** → `/etc/default/gpsd`:
   ```
   DEVICES="/dev/ttyUSB1"
   GPSD_OPTIONS="-n"
   USBAUTO="true"
   ```
3. **Deploy GPS enable script** → `/usr/local/bin/ghostcam-enable-gps.sh`:
   - Waits for ModemManager to register modem
   - Finds modem index via `mmcli -L`
   - Enables GPS: `mmcli -m $IDX --location-enable-gps-nmea --location-enable-gps-raw`
4. **Deploy systemd service** `ghostcam-gps.service`:
   - Type: oneshot, RemainAfterExit=yes
   - After: ModemManager.service
   - Runs enable script at boot
5. **Enable services**: `systemctl enable gpsd ghostcam-gps`

These files live in `pi/` in the ghostcam repo and are deployed by the camera manager.

## Dependencies

No new Rust crate dependencies. Uses `tokio::net::TcpStream` and `serde_json` (both already in the workspace).

## Synthetic GPS Preservation

The existing synthetic GPS in `sensors.rs` (sinusoidal drift around San Francisco) remains for `--test-source` mode and non-Linux development. The dispatch:

```rust
if config.no_gps {
    None
} else if config.test_source || !cfg!(target_os = "linux") {
    Some(synthetic_gps())
} else {
    GpsdReader::new().await  // real gpsd
}
```
