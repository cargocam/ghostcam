use bytes::Bytes;
use crate::command::CameraCommand;
use crate::frame::StreamType;
use crate::group::GroupId;
use crate::telemetry::{SparseTelemetry, TelemetryData};
use serde::Serialize;
use std::collections::{HashMap, HashSet};
use tokio::sync::{broadcast, mpsc};
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
    /// Per-camera command senders. Receiver lives in the QUIC handler task.
    pub command_txs: HashMap<DeviceId, mpsc::Sender<CameraCommand>>,
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
            command_txs: HashMap::new(),
        }
    }

    pub fn register_camera(
        &mut self,
        device_id: DeviceId,
        group_id: GroupId,
        capabilities: Vec<String>,
        command_tx: mpsc::Sender<CameraCommand>,
    ) {
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
        self.command_txs.insert(device_id.clone(), command_tx);
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
            self.command_txs.remove(device_id);
            info!(device_id = %device_id, "camera unregistered");
        }
    }

    /// Reassign a camera to a new group. Returns the old group_id.
    pub fn reassign_camera(
        &mut self,
        device_id: &str,
        new_group_id: GroupId,
    ) -> Result<GroupId, String> {
        let camera = self
            .cameras
            .get_mut(device_id)
            .ok_or_else(|| format!("camera {device_id} not found"))?;

        let old_group_id = camera.group_id.clone();

        // Remove from old group
        if let Some(set) = self.groups.get_mut(&old_group_id) {
            set.remove(device_id);
            if set.is_empty() {
                self.groups.remove(&old_group_id);
            }
        }

        // Add to new group
        camera.group_id = new_group_id.clone();
        self.groups
            .entry(new_group_id.clone())
            .or_default()
            .insert(device_id.to_string());

        info!(
            device_id = %device_id,
            old_group = %old_group_id,
            new_group = %new_group_id,
            "camera reassigned"
        );
        Ok(old_group_id)
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

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::sync::mpsc;

    fn dummy_command_tx() -> mpsc::Sender<CameraCommand> {
        let (tx, _rx) = mpsc::channel(1);
        tx
    }

    #[test]
    fn reassign_camera_moves_between_groups() {
        let mut router = GroupRouter::new();
        router.register_camera(
            "cam-01".into(),
            GroupId::new("alpha"),
            vec!["h264".into()],
            dummy_command_tx(),
        );

        let old = router
            .reassign_camera("cam-01", GroupId::new("beta"))
            .unwrap();
        assert_eq!(old, GroupId::new("alpha"));

        // Camera is now in beta
        assert_eq!(router.cameras["cam-01"].group_id, GroupId::new("beta"));
        assert!(router.groups.get(&GroupId::new("beta")).unwrap().contains("cam-01"));

        // Alpha group is cleaned up (was the only camera)
        assert!(!router.groups.contains_key(&GroupId::new("alpha")));
    }

    #[test]
    fn reassign_camera_nonexistent_errors() {
        let mut router = GroupRouter::new();
        let result = router.reassign_camera("no-such-cam", GroupId::new("beta"));
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("not found"));
    }

    #[test]
    fn reassign_camera_preserves_other_cameras_in_source_group() {
        let mut router = GroupRouter::new();
        router.register_camera(
            "cam-01".into(),
            GroupId::new("alpha"),
            vec![],
            dummy_command_tx(),
        );
        router.register_camera(
            "cam-02".into(),
            GroupId::new("alpha"),
            vec![],
            dummy_command_tx(),
        );

        router
            .reassign_camera("cam-01", GroupId::new("beta"))
            .unwrap();

        // Alpha still exists with cam-02
        assert!(router.groups.get(&GroupId::new("alpha")).unwrap().contains("cam-02"));
        assert_eq!(router.groups.get(&GroupId::new("alpha")).unwrap().len(), 1);
    }

    #[test]
    fn unregister_removes_command_tx() {
        let mut router = GroupRouter::new();
        router.register_camera(
            "cam-01".into(),
            GroupId::new("default"),
            vec![],
            dummy_command_tx(),
        );
        assert!(router.command_txs.contains_key("cam-01"));

        router.unregister_camera("cam-01");
        assert!(!router.command_txs.contains_key("cam-01"));
    }
}
