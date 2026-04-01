//! Shared request/response types for the camera↔server HTTP API.

use serde::{Deserialize, Serialize};

use crate::telemetry::TelemetryDatagram;

/// Camera provisioning request (camera → server).
/// Sent after scanning a provisioning QR code.
#[derive(Debug, Serialize, Deserialize)]
pub struct ProvisionRequest {
    /// One-time provisioning token from QR code.
    pub token: String,
    /// Pi serial number (from /proc/cpuinfo).
    pub device_serial: String,
    /// Camera firmware version.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fw_version: Option<String>,
}

/// Camera provisioning response (server → camera).
#[derive(Debug, Serialize, Deserialize)]
pub struct ProvisionResponse {
    /// Permanent API key for all future requests.
    pub api_key: String,
    /// Server-assigned device ID.
    pub device_id: String,
}

/// Telemetry poll request (camera → server, every 10s).
#[derive(Debug, Serialize, Deserialize)]
pub struct TelemetryPollRequest {
    pub telemetry: TelemetryDatagram,
    /// Camera firmware version.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fw_version: Option<String>,
}

/// Telemetry poll response (server → camera).
/// Contains any pending commands for the camera.
#[derive(Debug, Serialize, Deserialize)]
pub struct TelemetryPollResponse {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub commands: Vec<CameraCommand>,
}

/// Commands the server can send to a camera (embedded in telemetry poll response).
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CameraCommand {
    /// Reboot the camera (e.g. firmware update).
    Reboot,
    /// Change recording mode.
    SetRecordingMode { mode: RecordingMode },
    /// Change resolution tier.
    SetResolution { resolution: String },
    /// Configure WiFi network.
    NetworkConfig { ssid: String, psk: String },
    /// Remove a WiFi network.
    RemoveNetwork { ssid: String },
    /// Unregister this camera (factory reset).
    Unregister,
}

/// Recording mode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecordingMode {
    Constant,
    Motion,
}

impl std::fmt::Display for RecordingMode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Constant => f.write_str("constant"),
            Self::Motion => f.write_str("motion"),
        }
    }
}

/// Presigned URL batch request (camera → server).
/// Camera requests URLs for upcoming segments and confirms previously uploaded ones.
#[derive(Debug, Serialize, Deserialize)]
pub struct PresignRequest {
    /// Number of presigned PUT URLs to generate.
    pub count: u32,
    /// Segment IDs that have been successfully uploaded since last request.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub uploaded: Vec<UploadedSegment>,
}

/// Confirmation of a successfully uploaded segment.
#[derive(Debug, Serialize, Deserialize)]
pub struct UploadedSegment {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
}

/// A presigned URL for uploading a segment to S3/Tigris.
#[derive(Debug, Serialize, Deserialize)]
pub struct PresignedUrl {
    /// Segment ID to use (server-assigned).
    pub segment_id: String,
    /// S3 key where the segment should be uploaded.
    pub s3_key: String,
    /// Presigned PUT URL.
    pub put_url: String,
    /// URL expiration (Unix seconds).
    pub expires_at: u64,
}

/// Presigned URL batch response (server → camera).
#[derive(Debug, Serialize, Deserialize)]
pub struct PresignResponse {
    pub urls: Vec<PresignedUrl>,
    /// Presigned PUT URL for the init segment (if not yet uploaded).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub init_url: Option<PresignedUrl>,
}

/// QR code payload for camera provisioning.
#[derive(Debug, Serialize, Deserialize)]
pub struct QrPayload {
    /// Server HTTPS URL.
    pub s: String,
    /// One-time provisioning token.
    pub t: String,
    /// WiFi SSID (optional).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub w: Option<String>,
    /// WiFi password (optional).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub p: Option<String>,
}
