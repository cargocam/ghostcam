use crate::group::GroupId;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CameraInfo {
    pub device_id: String,
    pub group_id: GroupId,
    pub capabilities: Vec<String>,
}

/// Messages sent over WebRTC data channel.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum DataChannelMessage {
    /// Full camera list (sent on session creation)
    Cameras {
        cameras: Vec<CameraInfo>,
    },
    /// A camera joined the group
    CameraJoin {
        camera: CameraInfo,
    },
    /// A camera left the group
    CameraLeave {
        device_id: String,
    },
    /// Periodic telemetry data
    Telemetry {
        device_id: String,
        cpu_percent: f64,
        temp_celsius: f64,
        memory_mb: f64,
        uptime_secs: u64,
        #[serde(skip_serializing_if = "Option::is_none")]
        gps: Option<GpsData>,
    },
    /// Request renegotiation (new SDP offer for track changes)
    Renegotiate {
        sdp_offer: String,
    },
    /// Maps SDP mid values to device IDs so the viewer can associate tracks with cameras
    TrackMap {
        tracks: Vec<TrackMapping>,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GpsData {
    pub latitude: f64,
    pub longitude: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TrackMapping {
    pub mid: String,
    pub device_id: String,
    pub kind: String,
}
