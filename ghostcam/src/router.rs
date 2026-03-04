use bytes::Bytes;
use crate::frame::StreamType;
use crate::group::GroupId;
use crate::telemetry::{SparseTelemetry, TelemetryData};
use serde::Serialize;
use std::collections::{HashMap, HashSet};
use tokio::sync::broadcast;
use tracing::{info, warn};

pub type DeviceId = String;
pub type SessionId = String;

#[derive(Debug, Clone)]
pub struct CameraFrame {
    pub device_id: DeviceId,
    pub stream_type: StreamType,
    pub timestamp_us: u64,
    pub payload: Bytes,
}

#[derive(Debug, Clone, Serialize)]
pub struct CameraState {
    pub device_id: DeviceId,
    pub group_id: GroupId,
    pub capabilities: Vec<String>,
    pub connected_at: u64,
}

#[derive(Debug)]
pub struct ViewerSession {
    pub session_id: SessionId,
    pub group_id: GroupId,
}

pub struct GroupRouter {
    pub cameras: HashMap<DeviceId, CameraState>,
    pub groups: HashMap<GroupId, HashSet<DeviceId>>,
    pub viewers: HashMap<SessionId, ViewerSession>,
    pub frame_tx: broadcast::Sender<CameraFrame>,
    /// Cached SPS NAL per camera
    pub sps_cache: HashMap<DeviceId, Bytes>,
    /// Cached PPS NAL per camera
    pub pps_cache: HashMap<DeviceId, Bytes>,
    /// Last known telemetry per camera
    pub telemetry: HashMap<DeviceId, TelemetryData>,
}

impl GroupRouter {
    pub fn new() -> Self {
        let (frame_tx, _) = broadcast::channel(4096);
        Self {
            cameras: HashMap::new(),
            groups: HashMap::new(),
            viewers: HashMap::new(),
            frame_tx,
            sps_cache: HashMap::new(),
            pps_cache: HashMap::new(),
            telemetry: HashMap::new(),
        }
    }

    pub fn register_camera(&mut self, device_id: DeviceId, group_id: GroupId, capabilities: Vec<String>) {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();

        let state = CameraState {
            device_id: device_id.clone(),
            group_id: group_id.clone(),
            capabilities,
            connected_at: now,
        };
        self.cameras.insert(device_id.clone(), state);
        self.groups
            .entry(group_id.clone())
            .or_default()
            .insert(device_id.clone());
        info!(device_id = %device_id, group_id = %group_id, "camera registered");
    }

    pub fn unregister_camera(&mut self, device_id: &str) {
        if let Some(camera) = self.cameras.remove(device_id) {
            if let Some(set) = self.groups.get_mut(&camera.group_id) {
                set.remove(device_id);
                if set.is_empty() {
                    self.groups.remove(&camera.group_id);
                }
            }
            self.sps_cache.remove(device_id);
            self.pps_cache.remove(device_id);
            self.telemetry.remove(device_id);
            info!(device_id = %device_id, "camera unregistered");
        }
    }

    /// Process a video frame — caches SPS/PPS NALs and broadcasts.
    pub fn on_video_frame(&mut self, device_id: &str, timestamp_us: u64, payload: Bytes) {
        // Check NAL type (first byte & 0x1F)
        if !payload.is_empty() {
            let nal_type = payload[0] & 0x1F;
            match nal_type {
                7 => {
                    // SPS
                    self.sps_cache
                        .insert(device_id.to_string(), payload.clone());
                }
                8 => {
                    // PPS
                    self.pps_cache
                        .insert(device_id.to_string(), payload.clone());
                }
                _ => {}
            }
        }

        let _ = self.frame_tx.send(CameraFrame {
            device_id: device_id.to_string(),
            stream_type: StreamType::Video,
            timestamp_us,
            payload,
        });
    }

    /// Process a telemetry frame — decode SparseTelemetry, merge into stored state, broadcast.
    pub fn on_telemetry_frame(&mut self, device_id: &str, timestamp_us: u64, payload: Bytes) {
        match SparseTelemetry::decode(&payload) {
            Ok(sparse) => {
                let state = self.telemetry.entry(device_id.to_string()).or_default();
                sparse.merge_into(state);

                let _ = self.frame_tx.send(CameraFrame {
                    device_id: device_id.to_string(),
                    stream_type: StreamType::Telemetry,
                    timestamp_us,
                    payload,
                });
            }
            Err(e) => {
                warn!(device_id = %device_id, error = %e, "failed to decode telemetry");
            }
        }
    }

    /// Broadcast an audio frame.
    pub fn on_audio_frame(&self, device_id: &str, timestamp_us: u64, payload: Bytes) {
        let _ = self.frame_tx.send(CameraFrame {
            device_id: device_id.to_string(),
            stream_type: StreamType::Audio,
            timestamp_us,
            payload,
        });
    }

    pub fn get_cameras_in_group(&self, group_id: &GroupId) -> Vec<&CameraState> {
        if group_id.0 == "__all__" {
            return self.cameras.values().collect();
        }
        self.groups
            .get(group_id)
            .map(|ids| {
                ids.iter()
                    .filter_map(|id| self.cameras.get(id))
                    .collect()
            })
            .unwrap_or_default()
    }

    pub fn get_sps_pps(&self, device_id: &str) -> (Option<&Bytes>, Option<&Bytes>) {
        (
            self.sps_cache.get(device_id),
            self.pps_cache.get(device_id),
        )
    }

    pub fn all_groups(&self) -> Vec<(GroupId, usize)> {
        self.groups
            .iter()
            .map(|(g, ids)| (g.clone(), ids.len()))
            .collect()
    }
}
