use anyhow::Result;
use ghostcam::telemetry::TelemetryDatagram;
use ghostcam::types::DeviceId;
use serde::Serialize;

use super::connection::RedisManager;

const TELEMETRY_KEY_PREFIX: &str = "telemetry:";
const RETENTION_MS: u64 = ghostcam::config::TELEMETRY_RETENTION_SECS * 1000;

/// Response type for telemetry queries.
#[derive(Debug, Clone, Serialize)]
pub struct TelemetryEntry {
    pub ts: u64,
    pub server_ts: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sig: Option<i8>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temp: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fps: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kbps: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uptime: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lat: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lon: Option<f64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alt: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gps_fix: Option<u8>,
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64
}

/// Write a single telemetry datagram to Redis.
pub async fn write_telemetry(
    redis: &RedisManager,
    device_id: &DeviceId,
    datagram: &TelemetryDatagram,
) {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, "redis unavailable — dropping telemetry write");
        return;
    };

    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);
    let server_ts = now_ms();
    let min_id = server_ts.saturating_sub(RETENTION_MS);
    let fields = datagram_to_fields(datagram, server_ts);

    let result: Result<String, redis::RedisError> = redis::cmd("XADD")
        .arg(&key)
        .arg("MINID")
        .arg("~")
        .arg(min_id)
        .arg("*")
        .arg(&fields)
        .query_async(&mut conn)
        .await;

    if let Err(e) = result {
        redis.record_write_error();
        tracing::debug!(device_id = %device_id, "redis telemetry write error: {e}");
    }
}

/// Write a batch of buffered telemetry datagrams to Redis.
pub async fn write_telemetry_batch(
    redis: &RedisManager,
    device_id: &DeviceId,
    datagrams: &[TelemetryDatagram],
) {
    let Some(mut conn) = redis.get_conn().await else {
        tracing::debug!(device_id = %device_id, "redis unavailable — dropping telemetry batch");
        return;
    };

    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);
    let server_ts = now_ms();
    let min_id = server_ts.saturating_sub(RETENTION_MS);

    // Use pipeline for batch writes
    let mut pipe = redis::pipe();
    for datagram in datagrams {
        let fields = datagram_to_fields(datagram, server_ts);
        pipe.cmd("XADD")
            .arg(&key)
            .arg("MINID")
            .arg("~")
            .arg(min_id)
            .arg("*")
            .arg(&fields);
    }

    let result: Result<Vec<String>, redis::RedisError> = pipe.query_async(&mut conn).await;
    if let Err(e) = result {
        redis.record_write_error();
        tracing::debug!(device_id = %device_id, "redis telemetry batch write error: {e}");
    }
}

/// Convert a TelemetryDatagram to Redis field/value pairs.
pub fn datagram_to_fields(datagram: &TelemetryDatagram, server_ts: u64) -> Vec<(String, String)> {
    let mut fields = vec![
        ("ts".to_string(), datagram.ts.to_string()),
        ("server_ts".to_string(), server_ts.to_string()),
    ];

    if let Some(v) = datagram.sig {
        fields.push(("sig".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.temp {
        fields.push(("temp".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.fps {
        fields.push(("fps".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.kbps {
        fields.push(("kbps".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.cpu {
        fields.push(("cpu".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.mem {
        fields.push(("mem".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.uptime {
        fields.push(("uptime".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.lat {
        fields.push(("lat".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.lon {
        fields.push(("lon".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.alt {
        fields.push(("alt".to_string(), v.to_string()));
    }
    if let Some(v) = datagram.gps_fix {
        fields.push(("gps_fix".to_string(), v.to_string()));
    }

    fields
}

/// Parse Redis stream entry fields back into a TelemetryEntry.
pub fn fields_to_entry(fields: &[(String, String)]) -> Result<TelemetryEntry> {
    let mut entry = TelemetryEntry {
        ts: 0,
        server_ts: 0,
        sig: None,
        temp: None,
        fps: None,
        kbps: None,
        cpu: None,
        mem: None,
        uptime: None,
        lat: None,
        lon: None,
        alt: None,
        gps_fix: None,
    };

    for (key, value) in fields {
        match key.as_str() {
            "ts" => entry.ts = value.parse()?,
            "server_ts" => entry.server_ts = value.parse()?,
            "sig" => entry.sig = Some(value.parse()?),
            "temp" => entry.temp = Some(value.parse()?),
            "fps" => entry.fps = Some(value.parse()?),
            "kbps" => entry.kbps = Some(value.parse()?),
            "cpu" => entry.cpu = Some(value.parse()?),
            "mem" => entry.mem = Some(value.parse()?),
            "uptime" => entry.uptime = Some(value.parse()?),
            "lat" => entry.lat = Some(value.parse()?),
            "lon" => entry.lon = Some(value.parse()?),
            "alt" => entry.alt = Some(value.parse()?),
            "gps_fix" => entry.gps_fix = Some(value.parse()?),
            _ => {} // Ignore unknown fields for forward compatibility
        }
    }

    Ok(entry)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn datagram_to_fields_full() {
        let d = TelemetryDatagram {
            ts: 1000,
            sig: Some(-62),
            temp: Some(42),
            fps: Some(30.0),
            kbps: Some(2000),
            cpu: Some(55),
            mem: Some(256),
            uptime: Some(3600),
            lat: Some(37.77),
            lon: Some(-122.42),
            alt: Some(100.0),
            gps_fix: Some(2),
        };
        let fields = datagram_to_fields(&d, 5000);
        // ts, server_ts, + 11 fields = 13 total
        assert_eq!(fields.len(), 13);
        assert!(fields.iter().any(|(k, _)| k == "ts"));
        assert!(fields.iter().any(|(k, _)| k == "server_ts"));
        assert!(fields.iter().any(|(k, _)| k == "sig"));
        assert!(fields.iter().any(|(k, _)| k == "gps_fix"));
    }

    #[test]
    fn datagram_to_fields_sparse() {
        let d = TelemetryDatagram {
            ts: 1000,
            cpu: Some(55),
            ..Default::default()
        };
        let fields = datagram_to_fields(&d, 5000);
        // ts, server_ts, cpu = 3
        assert_eq!(fields.len(), 3);
    }

    #[test]
    fn datagram_to_fields_gps() {
        let d = TelemetryDatagram {
            ts: 1000,
            lat: Some(51.5),
            lon: Some(-0.1),
            alt: Some(30.5),
            gps_fix: Some(2),
            ..Default::default()
        };
        let fields = datagram_to_fields(&d, 5000);
        assert!(fields.iter().any(|(k, _)| k == "lat"));
        assert!(fields.iter().any(|(k, _)| k == "lon"));
        assert!(fields.iter().any(|(k, _)| k == "alt"));
        assert!(fields.iter().any(|(k, _)| k == "gps_fix"));
    }

    #[test]
    fn datagram_to_fields_no_gps() {
        let d = TelemetryDatagram {
            ts: 1000,
            cpu: Some(55),
            ..Default::default()
        };
        let fields = datagram_to_fields(&d, 5000);
        assert!(!fields.iter().any(|(k, _)| k == "lat"));
        assert!(!fields.iter().any(|(k, _)| k == "gps_fix"));
    }

    #[test]
    fn fields_to_entry_roundtrip() {
        let d = TelemetryDatagram {
            ts: 1000,
            sig: Some(-62),
            temp: Some(42),
            fps: Some(30.0),
            kbps: Some(2000),
            cpu: Some(55),
            mem: Some(256),
            uptime: Some(3600),
            lat: Some(37.77),
            lon: Some(-122.42),
            alt: Some(100.0),
            gps_fix: Some(2),
        };
        let fields = datagram_to_fields(&d, 5000);
        let entry = fields_to_entry(&fields).unwrap();
        assert_eq!(entry.ts, 1000);
        assert_eq!(entry.server_ts, 5000);
        assert_eq!(entry.sig, Some(-62));
        assert_eq!(entry.cpu, Some(55));
        assert_eq!(entry.lat, Some(37.77));
        assert_eq!(entry.gps_fix, Some(2));
    }

    #[test]
    fn fields_to_entry_missing_optional() {
        let fields = vec![
            ("ts".to_string(), "1000".to_string()),
            ("server_ts".to_string(), "5000".to_string()),
        ];
        let entry = fields_to_entry(&fields).unwrap();
        assert_eq!(entry.ts, 1000);
        assert!(entry.cpu.is_none());
        assert!(entry.lat.is_none());
    }
}
