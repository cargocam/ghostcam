use std::time::Duration;

use anyhow::{Context, Result};
use aws_sdk_s3::presigning::PresigningConfig;
use aws_sdk_s3::Client;

use crate::config::ServerConfig;

/// S3/Tigris client wrapper for presigned URL generation.
pub struct S3Client {
    client: Client,
    bucket: String,
    presign_ttl: Duration,
}

impl S3Client {
    /// Create a new S3 client configured for Tigris (or MinIO in dev).
    pub async fn new(config: &ServerConfig) -> Result<Self> {
        let mut s3_config = aws_config::defaults(aws_config::BehaviorVersion::latest())
            .region(aws_config::Region::new(config.s3_region.clone()));

        if let Some(ref endpoint) = config.s3_endpoint {
            s3_config = s3_config.endpoint_url(endpoint);
        }

        let aws_config = s3_config.load().await;
        // Use path-style addressing for MinIO compatibility.
        // Tigris also supports path-style, so this works everywhere.
        let client = aws_sdk_s3::Client::from_conf(
            aws_sdk_s3::config::Builder::from(&aws_config)
                .force_path_style(true)
                .build(),
        );

        Ok(Self {
            client,
            bucket: config.s3_bucket.clone(),
            presign_ttl: Duration::from_secs(config.s3_presign_ttl_secs),
        })
    }

    /// Generate a presigned PUT URL for uploading a segment.
    pub async fn presign_put(&self, key: &str) -> Result<String> {
        let presigning = PresigningConfig::builder()
            .expires_in(self.presign_ttl)
            .build()
            .context("building presign config")?;

        let request = self
            .client
            .put_object()
            .bucket(&self.bucket)
            .key(key)
            .presigned(presigning)
            .await
            .context("generating presigned PUT URL")?;

        Ok(request.uri().to_string())
    }

    /// Generate a presigned GET URL for downloading a segment (viewer playback).
    pub async fn presign_get(&self, key: &str) -> Result<String> {
        let presigning = PresigningConfig::builder()
            .expires_in(self.presign_ttl)
            .build()
            .context("building presign config")?;

        let request = self
            .client
            .get_object()
            .bucket(&self.bucket)
            .key(key)
            .presigned(presigning)
            .await
            .context("generating presigned GET URL")?;

        Ok(request.uri().to_string())
    }

    /// S3 key for a camera's init segment.
    pub fn init_key(device_id: &str) -> String {
        format!("{device_id}/init.mp4")
    }

    /// S3 key for a camera segment.
    pub fn segment_key(device_id: &str, segment_id: &str) -> String {
        format!("{device_id}/{segment_id}.ts")
    }

    /// Generate a batch of presigned PUT URLs for upcoming segments.
    pub async fn presign_put_batch(
        &self,
        device_id: &str,
        segment_ids: &[String],
    ) -> Result<Vec<(String, String, String)>> {
        let mut results = Vec::with_capacity(segment_ids.len());
        for seg_id in segment_ids {
            let key = Self::segment_key(device_id, seg_id);
            let url = self.presign_put(&key).await?;
            results.push((seg_id.clone(), key, url));
        }
        Ok(results)
    }

    /// Presign TTL in seconds (for communicating expiry to cameras).
    pub fn presign_ttl_secs(&self) -> u64 {
        self.presign_ttl.as_secs()
    }
}
