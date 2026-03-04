use anyhow::Result;
use ghostcam::telemetry::{GpsData, SparseTelemetry, TelemetryData, TelemetryThresholds};
use tokio::sync::mpsc;
use tracing::{info, warn};

use super::CaptureMessage;

#[derive(Debug, Clone)]
pub struct TelemetryCaptureConfig {
    /// Polling interval in seconds.
    pub interval_secs: u64,
    /// Full heartbeat interval in seconds.
    pub heartbeat_secs: u64,
    /// Whether to enable GPS via gpsd.
    pub enable_gps: bool,
}

impl Default for TelemetryCaptureConfig {
    fn default() -> Self {
        Self {
            interval_secs: 2,
            heartbeat_secs: 30,
            enable_gps: false,
        }
    }
}

/// Start telemetry collection, sending SparseTelemetry messages on `tx`.
pub fn start(config: TelemetryCaptureConfig, tx: mpsc::Sender<CaptureMessage>) {
    tokio::spawn(async move {
        if let Err(e) = run_telemetry_loop(config, tx).await {
            warn!(error = %e, "telemetry capture ended");
        }
    });
}

async fn run_telemetry_loop(
    config: TelemetryCaptureConfig,
    tx: mpsc::Sender<CaptureMessage>,
) -> Result<()> {
    let mut interval = tokio::time::interval(std::time::Duration::from_secs(config.interval_secs));
    let thresholds = TelemetryThresholds::default();
    let mut prev = TelemetryData::default();
    let mut heartbeat_counter: u64 = 0;
    let heartbeat_every = config.heartbeat_secs / config.interval_secs;

    // Initial CPU read for delta calculation
    #[cfg(target_os = "linux")]
    let mut prev_cpu = read_cpu_times();

    // GPS connection (if enabled)
    let mut gps_reader = if config.enable_gps {
        match GpsReader::connect().await {
            Ok(r) => {
                info!("connected to gpsd");
                Some(r)
            }
            Err(e) => {
                warn!(error = %e, "failed to connect to gpsd, continuing without GPS");
                None
            }
        }
    } else {
        None
    };

    loop {
        interval.tick().await;
        heartbeat_counter += 1;

        let mut current = TelemetryData::default();

        // CPU
        #[cfg(target_os = "linux")]
        {
            let curr_cpu = read_cpu_times();
            current.cpu_percent = compute_cpu_percent(&prev_cpu, &curr_cpu);
            prev_cpu = curr_cpu;
        }

        // Temperature
        current.temp_celsius = read_temperature();

        // Memory
        current.memory_mb = read_memory_used_mb();

        // Uptime
        current.uptime_secs = read_uptime_secs();

        // Load average
        current.load_average = read_load_average();

        // Network
        let (tx_bytes, rx_bytes) = read_network_bytes();
        current.network_tx_bytes = tx_bytes;
        current.network_rx_bytes = rx_bytes;

        // GPS
        if let Some(ref mut gps) = gps_reader {
            if let Some(fix) = gps.latest_fix() {
                current.gps = Some(fix);
            }
        }

        let sparse = if heartbeat_counter % heartbeat_every == 0 {
            SparseTelemetry::from_full(&current)
        } else {
            SparseTelemetry::diff(&prev, &current, &thresholds)
        };

        prev = current;

        match sparse.encode() {
            Ok(encoded) => {
                if tx
                    .send(CaptureMessage::Telemetry {
                        msgpack_data: encoded,
                    })
                    .await
                    .is_err()
                {
                    return Ok(()); // receiver dropped
                }
            }
            Err(e) => {
                warn!(error = %e, "failed to encode telemetry");
            }
        }
    }
}

// ─── Linux system readers ───────────────────────────────────────────────────

#[cfg(target_os = "linux")]
struct CpuTimes {
    user: u64,
    nice: u64,
    system: u64,
    idle: u64,
    iowait: u64,
}

#[cfg(target_os = "linux")]
fn read_cpu_times() -> CpuTimes {
    let contents = std::fs::read_to_string("/proc/stat").unwrap_or_default();
    let first_line = contents.lines().next().unwrap_or("");
    let parts: Vec<u64> = first_line
        .split_whitespace()
        .skip(1) // skip "cpu"
        .take(5)
        .filter_map(|s| s.parse().ok())
        .collect();
    CpuTimes {
        user: parts.first().copied().unwrap_or(0),
        nice: parts.get(1).copied().unwrap_or(0),
        system: parts.get(2).copied().unwrap_or(0),
        idle: parts.get(3).copied().unwrap_or(0),
        iowait: parts.get(4).copied().unwrap_or(0),
    }
}

#[cfg(target_os = "linux")]
fn compute_cpu_percent(prev: &CpuTimes, curr: &CpuTimes) -> f32 {
    let prev_total = prev.user + prev.nice + prev.system + prev.idle + prev.iowait;
    let curr_total = curr.user + curr.nice + curr.system + curr.idle + curr.iowait;
    let total_delta = curr_total.saturating_sub(prev_total);
    let idle_delta = curr.idle.saturating_sub(prev.idle) + curr.iowait.saturating_sub(prev.iowait);
    if total_delta == 0 {
        return 0.0;
    }
    ((total_delta - idle_delta) as f32 / total_delta as f32) * 100.0
}

#[cfg(target_os = "linux")]
fn read_temperature() -> Option<f32> {
    std::fs::read_to_string("/sys/class/thermal/thermal_zone0/temp")
        .ok()
        .and_then(|s| s.trim().parse::<f32>().ok())
        .map(|millideg| millideg / 1000.0)
}

#[cfg(not(target_os = "linux"))]
fn read_temperature() -> Option<f32> {
    None
}

#[cfg(target_os = "linux")]
fn read_memory_used_mb() -> f32 {
    let contents = std::fs::read_to_string("/proc/meminfo").unwrap_or_default();
    let mut total_kb = 0u64;
    let mut available_kb = 0u64;
    for line in contents.lines() {
        if let Some(rest) = line.strip_prefix("MemTotal:") {
            total_kb = parse_meminfo_kb(rest);
        } else if let Some(rest) = line.strip_prefix("MemAvailable:") {
            available_kb = parse_meminfo_kb(rest);
        }
    }
    (total_kb.saturating_sub(available_kb)) as f32 / 1024.0
}

#[cfg(target_os = "linux")]
fn parse_meminfo_kb(s: &str) -> u64 {
    s.trim()
        .split_whitespace()
        .next()
        .and_then(|v| v.parse().ok())
        .unwrap_or(0)
}

#[cfg(not(target_os = "linux"))]
fn read_memory_used_mb() -> f32 {
    0.0
}

#[cfg(target_os = "linux")]
fn read_uptime_secs() -> u64 {
    std::fs::read_to_string("/proc/uptime")
        .ok()
        .and_then(|s| {
            s.split_whitespace()
                .next()
                .and_then(|v| v.parse::<f64>().ok())
        })
        .map(|v| v as u64)
        .unwrap_or(0)
}

#[cfg(not(target_os = "linux"))]
fn read_uptime_secs() -> u64 {
    0
}

#[cfg(target_os = "linux")]
fn read_load_average() -> [f32; 3] {
    std::fs::read_to_string("/proc/loadavg")
        .ok()
        .map(|s| {
            let parts: Vec<f32> = s
                .split_whitespace()
                .take(3)
                .filter_map(|v| v.parse().ok())
                .collect();
            [
                parts.first().copied().unwrap_or(0.0),
                parts.get(1).copied().unwrap_or(0.0),
                parts.get(2).copied().unwrap_or(0.0),
            ]
        })
        .unwrap_or([0.0; 3])
}

#[cfg(not(target_os = "linux"))]
fn read_load_average() -> [f32; 3] {
    [0.0; 3]
}

#[cfg(target_os = "linux")]
fn read_network_bytes() -> (u64, u64) {
    let contents = std::fs::read_to_string("/proc/net/dev").unwrap_or_default();
    let mut tx_total = 0u64;
    let mut rx_total = 0u64;
    for line in contents.lines().skip(2) {
        // Skip header lines
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() < 10 {
            continue;
        }
        let iface = parts[0].trim_end_matches(':');
        if iface == "lo" {
            continue;
        }
        rx_total += parts[1].parse::<u64>().unwrap_or(0);
        tx_total += parts[9].parse::<u64>().unwrap_or(0);
    }
    (tx_total, rx_total)
}

#[cfg(not(target_os = "linux"))]
fn read_network_bytes() -> (u64, u64) {
    (0, 0)
}

// ─── GPS via gpsd ───────────────────────────────────────────────────────────

struct GpsReader {
    latest: std::sync::Arc<std::sync::Mutex<Option<GpsData>>>,
}

impl GpsReader {
    async fn connect() -> Result<Self> {
        use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
        use tokio::net::TcpStream;

        let stream = TcpStream::connect("127.0.0.1:2947").await?;
        let latest = std::sync::Arc::new(std::sync::Mutex::new(None::<GpsData>));
        let latest_for_task = latest.clone();

        tokio::spawn(async move {
            let (reader, mut writer) = stream.into_split();
            // Enable JSON watch mode
            if let Err(e) = writer
                .write_all(b"?WATCH={\"enable\":true,\"json\":true}\n")
                .await
            {
                warn!(error = %e, "failed to send WATCH to gpsd");
                return;
            }

            let mut lines = BufReader::new(reader).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                if let Some(fix) = parse_tpv_line(&line) {
                    *latest_for_task.lock().unwrap() = Some(fix);
                }
            }
            warn!("gpsd connection closed");
        });

        Ok(Self { latest })
    }

    fn latest_fix(&self) -> Option<GpsData> {
        self.latest.lock().unwrap().clone()
    }
}

fn parse_tpv_line(line: &str) -> Option<GpsData> {
    let v: serde_json::Value = serde_json::from_str(line).ok()?;
    if v.get("class")?.as_str()? != "TPV" {
        return None;
    }
    let lat = v.get("lat")?.as_f64()?;
    let lon = v.get("lon")?.as_f64()?;
    Some(GpsData {
        latitude: lat,
        longitude: lon,
        altitude: v.get("altHAE").and_then(|v| v.as_f64()),
        speed: v.get("speed").and_then(|v| v.as_f64()),
        fix_mode: v.get("mode").and_then(|v| v.as_u64()).unwrap_or(0) as u8,
    })
}
