use serde::{Deserialize, Serialize};

/// MessagePack-encoded telemetry datagram sent via QUIC datagrams.
/// All fields except `ts` are optional — only changed values are sent.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct TelemetryDatagram {
    /// Unix milliseconds, camera clock.
    pub ts: u64,
    /// WiFi signal strength (dBm).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sig: Option<i8>,
    /// SoC temperature (°C).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temp: Option<u32>,
    /// Capture frame rate.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fps: Option<f32>,
    /// Video bitrate (kbps).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kbps: Option<u32>,
    /// CPU usage (%).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cpu: Option<u32>,
    /// Memory usage (MB).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem: Option<u32>,
    /// Uptime (seconds).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uptime: Option<u32>,
    /// GPS latitude.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lat: Option<f64>,
    /// GPS longitude.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lon: Option<f64>,
    /// GPS altitude (metres).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alt: Option<f32>,
    /// GPS fix quality: 0=none, 1=2D, 2=3D.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub gps_fix: Option<u8>,
}

impl TelemetryDatagram {
    /// Encode to MessagePack bytes (named/map format for optional field support).
    pub fn encode(&self) -> Vec<u8> {
        rmp_serde::to_vec_named(self).expect("telemetry serialization cannot fail")
    }

    /// Decode from MessagePack bytes.
    pub fn decode(data: &[u8]) -> Result<Self, rmp_serde::decode::Error> {
        rmp_serde::from_slice(data)
    }

    /// Encode a batch of datagrams to MessagePack.
    pub fn encode_batch(batch: &[TelemetryDatagram]) -> Vec<u8> {
        rmp_serde::to_vec_named(batch).expect("telemetry batch serialization cannot fail")
    }

    /// Decode a batch of datagrams from MessagePack.
    pub fn decode_batch(data: &[u8]) -> Result<Vec<TelemetryDatagram>, rmp_serde::decode::Error> {
        rmp_serde::from_slice(data)
    }
}

/// Threshold configuration for telemetry change detection.
pub struct TelemetryThresholds {
    pub sig: i8,
    pub temp: u32,
    pub fps: f32,
    pub kbps: u32,
    pub cpu: u32,
    pub mem: u32,
    pub lat_lon: f64,
    pub alt: f32,
}

impl Default for TelemetryThresholds {
    fn default() -> Self {
        Self {
            sig: 5,
            temp: 1,
            fps: 2.0,
            kbps: 500,
            cpu: 5,
            mem: 5,
            lat_lon: 0.0001,
            alt: 10.0,
        }
    }
}

/// Returns true if any field in `current` exceeds thresholds compared to `previous`.
/// Fields transitioning between Some and None always trigger.
/// Uptime is always considered changed if the value differs.
pub fn exceeds_threshold(
    previous: &TelemetryDatagram,
    current: &TelemetryDatagram,
    thresholds: &TelemetryThresholds,
) -> bool {
    macro_rules! check_u32 {
        ($field:ident, $threshold:expr) => {
            match (previous.$field, current.$field) {
                (Some(p), Some(c)) => {
                    if p.abs_diff(c) >= $threshold {
                        return true;
                    }
                }
                (None, Some(_)) | (Some(_), None) => return true,
                (None, None) => {}
            }
        };
    }

    macro_rules! check_float {
        ($field:ident, $threshold:expr) => {
            match (previous.$field, current.$field) {
                (Some(p), Some(c)) => {
                    if (p - c).abs() >= $threshold {
                        return true;
                    }
                }
                (None, Some(_)) | (Some(_), None) => return true,
                (None, None) => {}
            }
        };
    }

    // sig is i8 — use abs_diff which returns u8
    match (previous.sig, current.sig) {
        (Some(p), Some(c)) => {
            if p.abs_diff(c) >= thresholds.sig as u8 {
                return true;
            }
        }
        (None, Some(_)) | (Some(_), None) => return true,
        (None, None) => {}
    }

    check_u32!(temp, thresholds.temp);
    check_float!(fps, thresholds.fps);
    check_u32!(kbps, thresholds.kbps);
    check_u32!(cpu, thresholds.cpu);
    check_u32!(mem, thresholds.mem);
    check_float!(lat, thresholds.lat_lon);
    check_float!(lon, thresholds.lat_lon);
    check_float!(alt, thresholds.alt);

    // GPS fix: any change triggers
    match (previous.gps_fix, current.gps_fix) {
        (Some(p), Some(c)) if p != c => return true,
        (None, Some(_)) | (Some(_), None) => return true,
        _ => {}
    }

    // Uptime: any change triggers
    match (previous.uptime, current.uptime) {
        (Some(p), Some(c)) if p != c => return true,
        (None, Some(_)) | (Some(_), None) => return true,
        _ => {}
    }

    false
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base_datagram() -> TelemetryDatagram {
        TelemetryDatagram {
            ts: 1000,
            sig: None,
            temp: None,
            fps: None,
            kbps: None,
            cpu: Some(20),
            mem: Some(100),
            uptime: Some(3600),
            lat: Some(37.7749),
            lon: Some(-122.4194),
            alt: None,
            gps_fix: Some(2),
        }
    }

    #[test]
    fn datagram_msgpack_roundtrip() {
        let d = base_datagram();
        let encoded = d.encode();
        let decoded = TelemetryDatagram::decode(&encoded).unwrap();
        assert_eq!(d, decoded);
    }

    #[test]
    fn datagram_sparse_roundtrip() {
        let d = TelemetryDatagram {
            ts: 500,
            cpu: Some(50),
            ..TelemetryDatagram {
                ts: 0,
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
            }
        };
        let encoded = d.encode();
        let decoded = TelemetryDatagram::decode(&encoded).unwrap();
        assert_eq!(d, decoded);
        assert!(decoded.mem.is_none());
    }

    #[test]
    fn datagram_gps_fields() {
        let d = TelemetryDatagram {
            ts: 1,
            lat: Some(51.5074),
            lon: Some(-0.1278),
            alt: Some(30.5),
            gps_fix: Some(2),
            ..base_datagram()
        };
        let decoded = TelemetryDatagram::decode(&d.encode()).unwrap();
        assert_eq!(decoded.lat, Some(51.5074));
        assert_eq!(decoded.alt, Some(30.5));
    }

    #[test]
    fn datagram_no_gps() {
        let d = TelemetryDatagram {
            ts: 1,
            lat: None,
            lon: None,
            alt: None,
            gps_fix: None,
            ..base_datagram()
        };
        let decoded = TelemetryDatagram::decode(&d.encode()).unwrap();
        assert!(decoded.lat.is_none());
        assert!(decoded.gps_fix.is_none());
    }

    #[test]
    fn datagram_array_roundtrip() {
        let batch = vec![base_datagram(), base_datagram()];
        let encoded = TelemetryDatagram::encode_batch(&batch);
        let decoded = TelemetryDatagram::decode_batch(&encoded).unwrap();
        assert_eq!(batch, decoded);
    }

    #[test]
    fn datagram_empty_array() {
        let batch: Vec<TelemetryDatagram> = vec![];
        let encoded = TelemetryDatagram::encode_batch(&batch);
        let decoded = TelemetryDatagram::decode_batch(&encoded).unwrap();
        assert!(decoded.is_empty());
    }

    #[test]
    fn threshold_cpu_triggers() {
        let prev = base_datagram();
        let mut curr = prev.clone();
        curr.cpu = Some(26); // diff = 6 > threshold 5
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_cpu_within() {
        let prev = base_datagram();
        let mut curr = prev.clone();
        curr.cpu = Some(24); // diff = 4 < threshold 5
                             // uptime is same so it won't trigger from uptime
        curr.uptime = prev.uptime;
        assert!(!exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_temp_triggers() {
        let mut prev = base_datagram();
        prev.temp = Some(50);
        let mut curr = prev.clone();
        curr.temp = Some(52); // diff = 2 > threshold 1
        curr.uptime = prev.uptime;
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_gps_triggers() {
        let prev = base_datagram(); // lat = 37.7749
        let mut curr = prev.clone();
        curr.lat = Some(37.7751); // diff = 0.0002 > threshold 0.0001
        curr.uptime = prev.uptime;
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_gps_within() {
        let prev = base_datagram(); // lat = 37.7749
        let mut curr = prev.clone();
        curr.lat = Some(37.77495); // diff = 0.00005 < threshold
        curr.uptime = prev.uptime;
        assert!(!exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_multiple_fields_one_exceeds() {
        let prev = base_datagram();
        let mut curr = prev.clone();
        curr.cpu = Some(21); // within threshold
        curr.mem = Some(110); // diff = 10 > threshold 5
        curr.uptime = prev.uptime;
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_no_previous_gps() {
        let mut prev = base_datagram();
        prev.lat = None;
        prev.lon = None;
        let mut curr = prev.clone();
        curr.lat = Some(37.7749);
        curr.uptime = prev.uptime;
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_no_current_gps() {
        let prev = base_datagram(); // has lat/lon
        let mut curr = prev.clone();
        curr.lat = None;
        curr.uptime = prev.uptime;
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_uptime_any_change() {
        let prev = base_datagram(); // uptime = 3600
        let mut curr = prev.clone();
        curr.uptime = Some(3601);
        assert!(exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }

    #[test]
    fn threshold_nothing_changed() {
        let prev = base_datagram();
        let curr = prev.clone();
        assert!(!exceeds_threshold(
            &prev,
            &curr,
            &TelemetryThresholds::default()
        ));
    }
}
