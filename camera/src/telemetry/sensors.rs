use ghostcam::telemetry::TelemetryDatagram;

use crate::config::CameraConfig;

/// Read all available sensor values.
///
/// In test-source mode, falls back to synthetic values for any sensor that
/// returns `None` (e.g. no thermal zone or GPS in Docker containers).
pub async fn read_sensors(config: &CameraConfig) -> TelemetryDatagram {
    let ts = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64;

    let (lat, lon, alt, gps_fix) = if config.no_gps {
        (None, None, None, None)
    } else {
        let real = read_gps();
        if config.test_source && real.0.is_none() {
            synthetic_gps()
        } else {
            real
        }
    };

    TelemetryDatagram {
        ts,
        cpu: read_cpu(),
        mem: read_memory(),
        temp: read_temperature().or_else(|| config.test_source.then_some(45)),
        uptime: read_uptime(),
        sig: read_wifi_signal().or_else(|| config.test_source.then_some(-55)),
        lat,
        lon,
        alt,
        gps_fix,
        fps: None,
        kbps: None,
    }
}

// --- Linux implementations ---

#[cfg(target_os = "linux")]
fn read_cpu() -> Option<u32> {
    // Read from /proc/stat — simplified one-shot read
    let contents = std::fs::read_to_string("/proc/stat").ok()?;
    let line = contents.lines().next()?;
    let parts: Vec<u64> = line
        .split_whitespace()
        .skip(1)
        .filter_map(|s| s.parse().ok())
        .collect();
    if parts.len() < 4 {
        return None;
    }
    let total: u64 = parts.iter().sum();
    let idle = parts[3];
    if total == 0 {
        return Some(0);
    }
    Some(((total - idle) * 100 / total) as u32)
}

#[cfg(target_os = "linux")]
fn read_memory() -> Option<u32> {
    let contents = std::fs::read_to_string("/proc/meminfo").ok()?;
    let mut total = 0u64;
    let mut available = 0u64;
    for line in contents.lines() {
        if let Some(val) = line.strip_prefix("MemTotal:") {
            total = val.trim().split_whitespace().next()?.parse().ok()?;
        } else if let Some(val) = line.strip_prefix("MemAvailable:") {
            available = val.trim().split_whitespace().next()?.parse().ok()?;
        }
    }
    if total == 0 {
        return None;
    }
    Some(((total - available) / 1024) as u32) // MB
}

#[cfg(target_os = "linux")]
fn read_temperature() -> Option<u32> {
    let contents =
        std::fs::read_to_string("/sys/class/thermal/thermal_zone0/temp").ok()?;
    let millideg: u32 = contents.trim().parse().ok()?;
    Some(millideg / 1000)
}

#[cfg(target_os = "linux")]
fn read_uptime() -> Option<u32> {
    let contents = std::fs::read_to_string("/proc/uptime").ok()?;
    let secs: f64 = contents.split_whitespace().next()?.parse().ok()?;
    Some(secs as u32)
}

#[cfg(target_os = "linux")]
fn read_wifi_signal() -> Option<i8> {
    let contents = std::fs::read_to_string("/proc/net/wireless").ok()?;
    // Third line has the data
    let line = contents.lines().nth(2)?;
    let parts: Vec<&str> = line.split_whitespace().collect();
    if parts.len() < 4 {
        return None;
    }
    let level: f64 = parts[3].trim_end_matches('.').parse().ok()?;
    Some(level as i8)
}

// --- Non-Linux fallbacks (synthetic values for development) ---

#[cfg(not(target_os = "linux"))]
fn read_cpu() -> Option<u32> {
    Some(15)
}

#[cfg(not(target_os = "linux"))]
fn read_memory() -> Option<u32> {
    Some(256)
}

#[cfg(not(target_os = "linux"))]
fn read_temperature() -> Option<u32> {
    Some(45)
}

#[cfg(not(target_os = "linux"))]
fn read_uptime() -> Option<u32> {
    // Use real system uptime
    let start = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    // Approximate — just return seconds since epoch mod some value
    Some((start % 86400) as u32)
}

#[cfg(not(target_os = "linux"))]
fn read_wifi_signal() -> Option<i8> {
    Some(-55)
}

// GPS — real implementation would use gpsd

#[cfg(target_os = "linux")]
fn read_gps() -> (Option<f64>, Option<f64>, Option<f32>, Option<u8>) {
    (None, None, None, None)
}

#[cfg(not(target_os = "linux"))]
fn read_gps() -> (Option<f64>, Option<f64>, Option<f32>, Option<u8>) {
    synthetic_gps()
}

/// Synthetic GPS coordinates for test-source cameras.
/// Each camera gets a unique offset based on a hash seed derived from the
/// hostname (Docker container ID) + PID, so containers that all run as PID 1
/// still get distinct positions.
fn synthetic_gps() -> (Option<f64>, Option<f64>, Option<f32>, Option<u8>) {
    // Base: San Francisco (37.7749, -122.4194)
    let seed = gps_seed();
    let t = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs_f64();

    let hash1 = (seed * 2654435761.0) % 1.0;
    let hash2 = ((seed + 1.0) * 2654435761.0) % 1.0;
    let base_lat_offset = hash1 * 0.012 - 0.006; // ±0.006° (~670m)
    let base_lon_offset = hash2 * 0.015 - 0.0075; // ±0.0075° (~670m)

    // Slow sinusoidal drift: different frequencies per axis, ~200m amplitude
    let drift_lat = 0.002 * (t / 120.0 + seed).sin(); // 2min period
    let drift_lon = 0.002 * (t / 90.0 + seed * 1.7).cos(); // 1.5min period

    let lat = 37.7749 + base_lat_offset + drift_lat;
    let lon = -122.4194 + base_lon_offset + drift_lon;
    let alt = 10.0 + (5.0 * (t / 60.0 + seed).sin()) as f32;

    (Some(lat), Some(lon), Some(alt), Some(2))
}

/// Produce a per-process seed from hostname + PID.
/// Hostname differentiates Docker containers (all PID 1); PID differentiates
/// multiple cameras on the same host.
fn gps_seed() -> f64 {
    let mut h: u64 = std::process::id() as u64;
    // HOSTNAME is set by Docker; fall back to /etc/hostname on bare metal.
    let name = std::env::var("HOSTNAME")
        .or_else(|_| std::fs::read_to_string("/etc/hostname"))
        .unwrap_or_default();
    for b in name.bytes() {
        h = h.wrapping_mul(31).wrapping_add(b as u64);
    }
    (h % 100_000) as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn read_sensors_returns_datagram() {
        let config = CameraConfig {
            server_addr: String::new(),
            test_source: true,
            test_video: String::new(),
            no_audio: false,
            no_gps: false,
            no_tofu: true,
            data_dir: String::new(),
        };
        let d = read_sensors(&config).await;
        assert!(d.ts > 0);
    }

    #[tokio::test]
    async fn read_sensors_has_cpu() {
        let config = CameraConfig {
            server_addr: String::new(),
            test_source: true,
            test_video: String::new(),
            no_audio: false,
            no_gps: false,
            no_tofu: true,
            data_dir: String::new(),
        };
        let d = read_sensors(&config).await;
        assert!(d.cpu.is_some());
    }

    #[tokio::test]
    async fn read_sensors_has_memory() {
        let config = CameraConfig {
            server_addr: String::new(),
            test_source: true,
            test_video: String::new(),
            no_audio: false,
            no_gps: false,
            no_tofu: true,
            data_dir: String::new(),
        };
        let d = read_sensors(&config).await;
        assert!(d.mem.is_some());
    }

    #[tokio::test]
    async fn read_sensors_no_gps_when_disabled() {
        let config = CameraConfig {
            server_addr: String::new(),
            test_source: true,
            test_video: String::new(),
            no_audio: false,
            no_gps: true,
            no_tofu: true,
            data_dir: String::new(),
        };
        let d = read_sensors(&config).await;
        assert!(d.lat.is_none());
        assert!(d.lon.is_none());
        assert!(d.alt.is_none());
        assert!(d.gps_fix.is_none());
    }
}
