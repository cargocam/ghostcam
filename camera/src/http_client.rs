use std::path::Path;
use std::time::Duration;

use anyhow::{Context, Result};
use ghostcam::api_types::{
    CameraCommand, PresignRequest, PresignResponse, ProvisionRequest, ProvisionResponse,
    TelemetryPollRequest, TelemetryPollResponse, UploadedSegment,
};
use ghostcam::telemetry::TelemetryDatagram;
use reqwest::Client;

/// HTTP client for camera→server communication.
pub struct CameraHttpClient {
    client: Client,
    server_url: String,
    api_key: String,
    device_id: String,
}

impl CameraHttpClient {
    pub fn new(server_url: &str, api_key: &str, device_id: &str) -> Self {
        let client = Client::builder()
            .timeout(Duration::from_secs(30))
            .connect_timeout(Duration::from_secs(10))
            .build()
            .expect("failed to build HTTP client");

        Self {
            client,
            server_url: server_url.trim_end_matches('/').to_string(),
            api_key: api_key.to_string(),
            device_id: device_id.to_string(),
        }
    }

    /// POST /api/v1/cameras/:id/telemetry
    /// Returns pending commands from server.
    pub async fn post_telemetry(
        &self,
        telemetry: TelemetryDatagram,
    ) -> Result<Vec<CameraCommand>> {
        let body = TelemetryPollRequest {
            telemetry,
            fw_version: Some(env!("CARGO_PKG_VERSION").to_string()),
        };

        let resp = self
            .client
            .post(format!(
                "{}/api/v1/cameras/{}/telemetry",
                self.server_url, self.device_id
            ))
            .bearer_auth(&self.api_key)
            .json(&body)
            .send()
            .await
            .context("telemetry POST failed")?;

        if !resp.status().is_success() {
            anyhow::bail!("telemetry POST returned {}", resp.status());
        }

        let poll_resp: TelemetryPollResponse = resp.json().await?;
        Ok(poll_resp.commands)
    }

    /// POST /api/v1/cameras/:id/presign
    /// Request presigned PUT URLs and confirm uploaded segments.
    pub async fn request_presigned_urls(
        &self,
        count: u32,
        uploaded: Vec<UploadedSegment>,
    ) -> Result<PresignResponse> {
        let body = PresignRequest { count, uploaded };

        let resp = self
            .client
            .post(format!(
                "{}/api/v1/cameras/{}/presign",
                self.server_url, self.device_id
            ))
            .bearer_auth(&self.api_key)
            .json(&body)
            .send()
            .await
            .context("presign POST failed")?;

        if !resp.status().is_success() {
            anyhow::bail!("presign POST returned {}", resp.status());
        }

        resp.json().await.context("presign response parse failed")
    }

    /// Upload a segment file to S3/Tigris via presigned PUT URL.
    pub async fn upload_file(&self, presigned_url: &str, data: Vec<u8>) -> Result<()> {
        let resp = self
            .client
            .put(presigned_url)
            .header("content-type", "video/mp4")
            .body(data)
            .send()
            .await
            .context("S3 PUT failed")?;

        if !resp.status().is_success() {
            anyhow::bail!("S3 PUT returned {}", resp.status());
        }

        Ok(())
    }

    pub fn device_id(&self) -> &str {
        &self.device_id
    }

    pub fn server_url(&self) -> &str {
        &self.server_url
    }
}

/// POST /api/v1/cameras/provision (no auth required)
/// Called during initial setup before the camera has an API key.
pub async fn provision(
    server_url: &str,
    token: &str,
    device_serial: &str,
) -> Result<ProvisionResponse> {
    let client = Client::builder()
        .timeout(Duration::from_secs(30))
        .connect_timeout(Duration::from_secs(10))
        .build()?;

    let body = ProvisionRequest {
        token: token.to_string(),
        device_serial: device_serial.to_string(),
        fw_version: Some(env!("CARGO_PKG_VERSION").to_string()),
    };

    let url = format!(
        "{}/api/v1/cameras/provision",
        server_url.trim_end_matches('/')
    );

    let resp = client
        .post(&url)
        .json(&body)
        .send()
        .await
        .context("provision POST failed")?;

    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        anyhow::bail!("provisioning failed: {status} — {body}");
    }

    resp.json().await.context("provision response parse failed")
}

/// Load persisted credentials from disk.
pub fn load_credentials(data_dir: &Path) -> Option<(String, String, String)> {
    let api_key_path = data_dir.join("api_key");
    let device_id_path = data_dir.join("device_id");
    let server_url_path = data_dir.join("server_url");

    let api_key = std::fs::read_to_string(&api_key_path).ok()?;
    let device_id = std::fs::read_to_string(&device_id_path).ok()?;
    let server_url = std::fs::read_to_string(&server_url_path).ok()?;

    let api_key = api_key.trim().to_string();
    let device_id = device_id.trim().to_string();
    let server_url = server_url.trim().to_string();

    if api_key.is_empty() || device_id.is_empty() || server_url.is_empty() {
        return None;
    }

    Some((api_key, device_id, server_url))
}

/// Persist credentials to disk.
pub fn save_credentials(
    data_dir: &Path,
    api_key: &str,
    device_id: &str,
    server_url: &str,
) -> Result<()> {
    std::fs::write(data_dir.join("api_key"), api_key)?;
    std::fs::write(data_dir.join("device_id"), device_id)?;
    std::fs::write(data_dir.join("server_url"), server_url)?;
    Ok(())
}
