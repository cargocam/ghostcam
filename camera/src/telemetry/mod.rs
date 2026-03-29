pub mod buffer;
pub mod gpsd;
pub mod sensors;

use std::time::Duration;

use anyhow::Result;
use ghostcam::config::{TELEMETRY_HEARTBEAT_INTERVAL_SECS, TELEMETRY_POLL_INTERVAL_SECS};
use ghostcam::telemetry::{exceeds_threshold, TelemetryDatagram, TelemetryThresholds};
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use self::buffer::TelemetryBuffer;
use self::sensors::GpsSource;
use crate::config::CameraConfig;

/// Run the telemetry poll-and-send loop.
///
/// `connected_rx` watches for connection status changes. When connected,
/// telemetry is sent as QUIC datagrams. When disconnected, it's buffered.
pub async fn run_telemetry_loop(
    connected_rx: watch::Receiver<Option<quinn::Connection>>,
    buffer: &TelemetryBuffer,
    config: &CameraConfig,
    cancel: CancellationToken,
) -> Result<()> {
    let poll_interval = Duration::from_secs(TELEMETRY_POLL_INTERVAL_SECS);
    let heartbeat_interval = Duration::from_secs(TELEMETRY_HEARTBEAT_INTERVAL_SECS);
    let thresholds = TelemetryThresholds::default();

    let mut previous = TelemetryDatagram::default();
    let mut last_heartbeat = tokio::time::Instant::now();
    let mut gps = GpsSource::new(config).await;

    tracing::info!("telemetry loop started");

    loop {
        tokio::select! {
            _ = cancel.cancelled() => {
                buffer.flush_to_disk().await?;
                return Ok(());
            }
            _ = tokio::time::sleep(poll_interval) => {}
        }

        let current = sensors::read_sensors(config, &mut gps);

        let heartbeat_due = last_heartbeat.elapsed() >= heartbeat_interval;
        let threshold_exceeded = exceeds_threshold(&previous, &current, &thresholds);

        if !heartbeat_due && !threshold_exceeded {
            continue;
        }

        if heartbeat_due {
            last_heartbeat = tokio::time::Instant::now();
        }

        // Try to send via QUIC datagram
        let conn = connected_rx.borrow().clone();
        if let Some(ref conn) = conn {
            let encoded = match current.encode() {
                Ok(v) => v,
                Err(e) => {
                    tracing::warn!("telemetry encode failed: {e}");
                    continue;
                }
            };
            match conn.send_datagram(encoded.into()) {
                Ok(()) => {}
                Err(e) => {
                    tracing::debug!("datagram send failed, buffering: {e}");
                    buffer.push(current.clone()).await;
                }
            }
        } else {
            buffer.push(current.clone()).await;
        }

        previous = current;
    }
}
