use bytes::Bytes;
use serde::{Deserialize, Serialize};

/// Full telemetry state for a camera.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct TelemetryData {
    pub cpu_percent: f32,
    pub temp_celsius: Option<f32>,
    pub memory_mb: f32,
    pub uptime_secs: u64,
    pub load_average: [f32; 3],
    pub network_tx_bytes: u64,
    pub network_rx_bytes: u64,
    pub gps: Option<GpsData>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GpsData {
    pub latitude: f64,
    pub longitude: f64,
    pub altitude: Option<f64>,
    pub speed: Option<f64>,
    pub fix_mode: u8,
}

/// Sparse telemetry for wire efficiency. Only changed fields are Some.
/// Short serde field names reduce MessagePack size.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SparseTelemetry {
    /// true = full heartbeat (all fields populated)
    #[serde(rename = "f", default, skip_serializing_if = "is_false")]
    pub full: bool,
    #[serde(rename = "c", default, skip_serializing_if = "Option::is_none")]
    pub cpu_percent: Option<f32>,
    #[serde(rename = "t", default, skip_serializing_if = "Option::is_none")]
    pub temp_celsius: Option<f32>,
    #[serde(rename = "m", default, skip_serializing_if = "Option::is_none")]
    pub memory_mb: Option<f32>,
    #[serde(rename = "u", default, skip_serializing_if = "Option::is_none")]
    pub uptime_secs: Option<u64>,
    #[serde(rename = "l", default, skip_serializing_if = "Option::is_none")]
    pub load_average: Option<[f32; 3]>,
    #[serde(rename = "tx", default, skip_serializing_if = "Option::is_none")]
    pub network_tx_bytes: Option<u64>,
    #[serde(rename = "rx", default, skip_serializing_if = "Option::is_none")]
    pub network_rx_bytes: Option<u64>,
    #[serde(rename = "g", default, skip_serializing_if = "Option::is_none")]
    pub gps: Option<GpsData>,
}

fn is_false(v: &bool) -> bool {
    !v
}

/// Thresholds for sparse diff — only send if change exceeds threshold.
pub struct TelemetryThresholds {
    pub cpu: f32,
    pub temp: f32,
    pub memory: f32,
    pub network: u64,
    pub lat_lon: f64,
}

impl Default for TelemetryThresholds {
    fn default() -> Self {
        Self {
            cpu: 5.0,
            temp: 1.0,
            memory: 5.0,
            network: 10_000,
            lat_lon: 0.0001,
        }
    }
}

impl SparseTelemetry {
    /// Create a sparse telemetry with all fields from a full state (heartbeat).
    pub fn from_full(data: &TelemetryData) -> Self {
        Self {
            full: true,
            cpu_percent: Some(data.cpu_percent),
            temp_celsius: data.temp_celsius,
            memory_mb: Some(data.memory_mb),
            uptime_secs: Some(data.uptime_secs),
            load_average: Some(data.load_average),
            network_tx_bytes: Some(data.network_tx_bytes),
            network_rx_bytes: Some(data.network_rx_bytes),
            gps: data.gps.clone(),
        }
    }

    /// Compute a diff between `prev` and `curr`, returning only changed fields.
    pub fn diff(prev: &TelemetryData, curr: &TelemetryData, thresholds: &TelemetryThresholds) -> Self {
        let mut s = SparseTelemetry::default();
        let mut any = false;

        if (curr.cpu_percent - prev.cpu_percent).abs() > thresholds.cpu {
            s.cpu_percent = Some(curr.cpu_percent);
            any = true;
        }
        match (curr.temp_celsius, prev.temp_celsius) {
            (Some(c), Some(p)) if (c - p).abs() > thresholds.temp => {
                s.temp_celsius = Some(c);
                any = true;
            }
            (Some(c), None) => {
                s.temp_celsius = Some(c);
                any = true;
            }
            _ => {}
        }
        if (curr.memory_mb - prev.memory_mb).abs() > thresholds.memory {
            s.memory_mb = Some(curr.memory_mb);
            any = true;
        }
        if curr.uptime_secs != prev.uptime_secs {
            s.uptime_secs = Some(curr.uptime_secs);
            any = true;
        }
        if curr.load_average != prev.load_average {
            s.load_average = Some(curr.load_average);
            any = true;
        }
        if curr.network_tx_bytes.abs_diff(prev.network_tx_bytes) > thresholds.network
            || curr.network_rx_bytes.abs_diff(prev.network_rx_bytes) > thresholds.network
        {
            s.network_tx_bytes = Some(curr.network_tx_bytes);
            s.network_rx_bytes = Some(curr.network_rx_bytes);
            any = true;
        }
        match (&curr.gps, &prev.gps) {
            (Some(c), Some(p)) => {
                if (c.latitude - p.latitude).abs() > thresholds.lat_lon
                    || (c.longitude - p.longitude).abs() > thresholds.lat_lon
                {
                    s.gps = Some(c.clone());
                    any = true;
                }
            }
            (Some(c), None) => {
                s.gps = Some(c.clone());
                any = true;
            }
            (None, Some(_)) => {
                // GPS lost — we still mark change via uptime
                any = true;
            }
            _ => {}
        }

        // Always include uptime so receiver knows the camera is alive
        if !any {
            s.uptime_secs = Some(curr.uptime_secs);
        }

        s
    }

    /// Merge sparse telemetry into a full state, updating only present fields.
    pub fn merge_into(&self, state: &mut TelemetryData) {
        if let Some(v) = self.cpu_percent {
            state.cpu_percent = v;
        }
        if let Some(v) = self.temp_celsius {
            state.temp_celsius = Some(v);
        }
        if let Some(v) = self.memory_mb {
            state.memory_mb = v;
        }
        if let Some(v) = self.uptime_secs {
            state.uptime_secs = v;
        }
        if let Some(v) = self.load_average {
            state.load_average = v;
        }
        if let Some(v) = self.network_tx_bytes {
            state.network_tx_bytes = v;
        }
        if let Some(v) = self.network_rx_bytes {
            state.network_rx_bytes = v;
        }
        if let Some(ref v) = self.gps {
            state.gps = Some(v.clone());
        }
    }

    /// Encode to MessagePack bytes.
    pub fn encode(&self) -> Result<Bytes, rmp_serde::encode::Error> {
        let data = rmp_serde::to_vec(self)?;
        Ok(Bytes::from(data))
    }

    /// Decode from MessagePack bytes.
    pub fn decode(data: &[u8]) -> Result<Self, rmp_serde::decode::Error> {
        rmp_serde::from_slice(data)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encode_decode_roundtrip() {
        let sparse = SparseTelemetry {
            full: true,
            cpu_percent: Some(42.5),
            temp_celsius: Some(55.0),
            memory_mb: Some(256.0),
            uptime_secs: Some(3600),
            load_average: Some([1.0, 0.5, 0.3]),
            network_tx_bytes: Some(1_000_000),
            network_rx_bytes: Some(2_000_000),
            gps: Some(GpsData {
                latitude: 37.7749,
                longitude: -122.4194,
                altitude: Some(10.0),
                speed: Some(0.5),
                fix_mode: 3,
            }),
        };
        let encoded = sparse.encode().unwrap();
        let decoded = SparseTelemetry::decode(&encoded).unwrap();
        assert!(decoded.full);
        assert_eq!(decoded.cpu_percent, Some(42.5));
        assert_eq!(decoded.uptime_secs, Some(3600));
        assert!(decoded.gps.is_some());
        let gps = decoded.gps.unwrap();
        assert!((gps.latitude - 37.7749).abs() < 0.0001);
    }

    #[test]
    fn diff_only_changed_fields() {
        let prev = TelemetryData {
            cpu_percent: 30.0,
            temp_celsius: Some(50.0),
            memory_mb: 256.0,
            uptime_secs: 100,
            ..Default::default()
        };
        let curr = TelemetryData {
            cpu_percent: 30.1, // below threshold
            temp_celsius: Some(50.0),
            memory_mb: 256.0,
            uptime_secs: 102,
            ..Default::default()
        };
        let thresholds = TelemetryThresholds::default();
        let diff = SparseTelemetry::diff(&prev, &curr, &thresholds);
        assert!(diff.cpu_percent.is_none()); // below 5% threshold
        assert!(diff.uptime_secs.is_some()); // always changes
    }

    #[test]
    fn merge_into_state() {
        let mut state = TelemetryData::default();
        let sparse = SparseTelemetry {
            cpu_percent: Some(75.0),
            memory_mb: Some(512.0),
            ..Default::default()
        };
        sparse.merge_into(&mut state);
        assert_eq!(state.cpu_percent, 75.0);
        assert_eq!(state.memory_mb, 512.0);
        assert_eq!(state.uptime_secs, 0); // unchanged
    }

    #[test]
    fn from_full_roundtrip() {
        let data = TelemetryData {
            cpu_percent: 50.0,
            temp_celsius: Some(60.0),
            memory_mb: 128.0,
            uptime_secs: 9999,
            load_average: [2.0, 1.5, 1.0],
            network_tx_bytes: 500_000,
            network_rx_bytes: 1_000_000,
            gps: None,
        };
        let sparse = SparseTelemetry::from_full(&data);
        assert!(sparse.full);
        let encoded = sparse.encode().unwrap();
        let decoded = SparseTelemetry::decode(&encoded).unwrap();
        let mut reconstructed = TelemetryData::default();
        decoded.merge_into(&mut reconstructed);
        assert_eq!(reconstructed.cpu_percent, 50.0);
        assert_eq!(reconstructed.memory_mb, 128.0);
        assert_eq!(reconstructed.uptime_secs, 9999);
    }
}
