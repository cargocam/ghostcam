//! gpsd JSON protocol client for reading GPS data.
//!
//! Connects to gpsd at `127.0.0.1:2947`, enables JSON watch mode,
//! and streams TPV (Time-Position-Velocity) reports in a background task.
//! Returns `None` gracefully if gpsd is unavailable.

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use tokio::sync::mpsc;

/// GPS position data parsed from a gpsd TPV report.
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct GpsData {
    pub latitude: f64,
    pub longitude: f64,
    pub altitude: Option<f64>,
    pub speed: Option<f64>,
    pub heading: Option<f64>,
    /// 2 = 2D fix, 3 = 3D fix (mode < 2 is rejected during parsing)
    pub fix_mode: u8,
}

/// Async reader that streams GPS fixes from gpsd.
pub struct GpsdReader {
    latest: Option<GpsData>,
    rx: mpsc::Receiver<GpsData>,
}

impl GpsdReader {
    /// Connect to gpsd. Returns `None` if gpsd is unavailable.
    pub async fn new() -> Option<Self> {
        let stream = match TcpStream::connect("127.0.0.1:2947").await {
            Ok(s) => s,
            Err(e) => {
                tracing::debug!("gpsd not available at 127.0.0.1:2947: {e}");
                return None;
            }
        };

        tracing::info!("connected to gpsd");

        let (tx, rx) = mpsc::channel(16);
        tokio::spawn(async move {
            if let Err(e) = gpsd_read_loop(stream, tx).await {
                tracing::debug!("gpsd reader ended: {e}");
            }
        });

        Some(Self { latest: None, rx })
    }

    /// Get the most recent GPS fix, draining any buffered updates.
    /// Returns the cached latest fix if no new data arrived.
    pub fn latest_fix(&mut self) -> Option<&GpsData> {
        while let Ok(fix) = self.rx.try_recv() {
            self.latest = Some(fix);
        }
        self.latest.as_ref()
    }
}

/// Background loop: enable watch mode, then read and parse JSON lines from gpsd.
async fn gpsd_read_loop(stream: TcpStream, tx: mpsc::Sender<GpsData>) -> Result<()> {
    let (reader, mut writer) = stream.into_split();

    writer
        .write_all(b"?WATCH={\"enable\":true,\"json\":true}\n")
        .await
        .context("failed to send WATCH command to gpsd")?;

    let mut lines = BufReader::new(reader).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        if let Some(gps) = parse_gpsd_tpv(&line) {
            if tx.send(gps).await.is_err() {
                break; // receiver dropped
            }
        }
    }

    Ok(())
}

/// Parse a gpsd TPV JSON report into `GpsData`.
/// Returns `None` for non-TPV messages or fixes with mode < 2 (no fix).
fn parse_gpsd_tpv(line: &str) -> Option<GpsData> {
    let v: serde_json::Value = serde_json::from_str(line).ok()?;

    if v.get("class")?.as_str()? != "TPV" {
        return None;
    }

    let mode = v.get("mode")?.as_u64()? as u8;
    if mode < 2 {
        return None;
    }

    Some(GpsData {
        latitude: v.get("lat")?.as_f64()?,
        longitude: v.get("lon")?.as_f64()?,
        altitude: v.get("alt").and_then(|v| v.as_f64()),
        speed: v.get("speed").and_then(|v| v.as_f64()),
        heading: v.get("track").and_then(|v| v.as_f64()),
        fix_mode: mode,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_tpv_3d_fix() {
        let line = r#"{"class":"TPV","mode":3,"lat":37.7749,"lon":-122.4194,"alt":10.5,"speed":1.2,"track":180.0}"#;
        let gps = parse_gpsd_tpv(line).unwrap();
        assert!((gps.latitude - 37.7749).abs() < 1e-6);
        assert!((gps.longitude - (-122.4194)).abs() < 1e-6);
        assert!((gps.altitude.unwrap() - 10.5).abs() < 1e-6);
        assert!((gps.speed.unwrap() - 1.2).abs() < 1e-6);
        assert!((gps.heading.unwrap() - 180.0).abs() < 1e-6);
        assert_eq!(gps.fix_mode, 3);
    }

    #[test]
    fn parse_tpv_2d_fix() {
        let line = r#"{"class":"TPV","mode":2,"lat":51.5074,"lon":-0.1278}"#;
        let gps = parse_gpsd_tpv(line).unwrap();
        assert!((gps.latitude - 51.5074).abs() < 1e-6);
        assert!(gps.altitude.is_none());
        assert!(gps.speed.is_none());
        assert_eq!(gps.fix_mode, 2);
    }

    #[test]
    fn parse_tpv_no_fix_rejected() {
        let line = r#"{"class":"TPV","mode":1,"lat":0.0,"lon":0.0}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }

    #[test]
    fn parse_tpv_mode_zero_rejected() {
        let line = r#"{"class":"TPV","mode":0}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }

    #[test]
    fn parse_non_tpv_ignored() {
        let line = r#"{"class":"SKY","satellites":[]}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }

    #[test]
    fn parse_version_ignored() {
        let line = r#"{"class":"VERSION","release":"3.25","proto_major":3}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }

    #[test]
    fn parse_invalid_json() {
        assert!(parse_gpsd_tpv("not json").is_none());
    }

    #[test]
    fn parse_missing_lat() {
        let line = r#"{"class":"TPV","mode":3,"lon":-122.4194}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }

    #[test]
    fn parse_missing_lon() {
        let line = r#"{"class":"TPV","mode":3,"lat":37.7749}"#;
        assert!(parse_gpsd_tpv(line).is_none());
    }
}
