use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// Commands sent from bridge to camera over the QUIC control stream.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CameraCommand {
    // Stream control
    StartVideo,
    StopVideo,
    StartAudio,
    StopAudio,
    StartTelemetry,
    StopTelemetry,

    // Hot configuration (sparse update, only non-None fields changed)
    Configure {
        #[serde(skip_serializing_if = "Option::is_none")]
        width: Option<u32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        height: Option<u32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        fps: Option<u32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        bitrate: Option<u32>,
        #[serde(skip_serializing_if = "Option::is_none")]
        keyframe_interval: Option<u32>,
    },

    ForceKeyframe,

    /// Reassign camera to a new group.
    ReassignGroup { group_id: String },

    /// Extensible command for PTZ, drone, GPIO, etc.
    /// Unknown commands should be warned and ignored.
    Custom {
        name: String,
        #[serde(default)]
        params: HashMap<String, serde_json::Value>,
    },
}

/// Optional response from camera to bridge after processing a command.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CommandResponse {
    Ack { command: String },
    Error { command: String, message: String },
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_stream_control() {
        for cmd in [
            CameraCommand::StartVideo,
            CameraCommand::StopVideo,
            CameraCommand::StartAudio,
            CameraCommand::StopAudio,
            CameraCommand::StartTelemetry,
            CameraCommand::StopTelemetry,
            CameraCommand::ForceKeyframe,
        ] {
            let json = serde_json::to_string(&cmd).unwrap();
            let parsed: CameraCommand = serde_json::from_str(&json).unwrap();
            assert_eq!(format!("{:?}", cmd), format!("{:?}", parsed));
        }
    }

    #[test]
    fn roundtrip_configure() {
        let cmd = CameraCommand::Configure {
            width: Some(1920),
            height: None,
            fps: Some(60),
            bitrate: None,
            keyframe_interval: Some(30),
        };
        let json = serde_json::to_string(&cmd).unwrap();
        assert!(!json.contains("height")); // None fields skipped
        let parsed: CameraCommand = serde_json::from_str(&json).unwrap();
        match parsed {
            CameraCommand::Configure {
                width,
                height,
                fps,
                bitrate,
                keyframe_interval,
            } => {
                assert_eq!(width, Some(1920));
                assert_eq!(height, None);
                assert_eq!(fps, Some(60));
                assert_eq!(bitrate, None);
                assert_eq!(keyframe_interval, Some(30));
            }
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn roundtrip_reassign_group() {
        let cmd = CameraCommand::ReassignGroup {
            group_id: "perimeter".into(),
        };
        let json = serde_json::to_string(&cmd).unwrap();
        let parsed: CameraCommand = serde_json::from_str(&json).unwrap();
        match parsed {
            CameraCommand::ReassignGroup { group_id } => assert_eq!(group_id, "perimeter"),
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn roundtrip_custom() {
        let mut params = HashMap::new();
        params.insert("pan".into(), serde_json::json!(45.0));
        params.insert("tilt".into(), serde_json::json!(-10));
        params.insert("zoom".into(), serde_json::json!(2));
        let cmd = CameraCommand::Custom {
            name: "ptz".into(),
            params,
        };
        let json = serde_json::to_string(&cmd).unwrap();
        let parsed: CameraCommand = serde_json::from_str(&json).unwrap();
        match parsed {
            CameraCommand::Custom { name, params } => {
                assert_eq!(name, "ptz");
                assert_eq!(params.len(), 3);
                assert_eq!(params["pan"], serde_json::json!(45.0));
            }
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn unknown_type_errors() {
        let json = r#"{"type":"unknown_future_command"}"#;
        let result = serde_json::from_str::<CameraCommand>(json);
        assert!(result.is_err());
    }

    #[test]
    fn roundtrip_response_ack() {
        let resp = CommandResponse::Ack {
            command: "stop_video".into(),
        };
        let json = serde_json::to_string(&resp).unwrap();
        let parsed: CommandResponse = serde_json::from_str(&json).unwrap();
        match parsed {
            CommandResponse::Ack { command } => assert_eq!(command, "stop_video"),
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn roundtrip_response_error() {
        let resp = CommandResponse::Error {
            command: "configure".into(),
            message: "not supported".into(),
        };
        let json = serde_json::to_string(&resp).unwrap();
        let parsed: CommandResponse = serde_json::from_str(&json).unwrap();
        match parsed {
            CommandResponse::Error { command, message } => {
                assert_eq!(command, "configure");
                assert_eq!(message, "not supported");
            }
            _ => panic!("wrong variant"),
        }
    }
}
