use std::collections::VecDeque;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Result;
use ghostcam::api_types::{PresignedUrl, UploadedSegment};
use tokio::sync::{mpsc, Mutex};
use tokio_util::sync::CancellationToken;

use crate::http_client::CameraHttpClient;
use crate::NewSegment;

/// Info about a segment waiting to be uploaded.
#[derive(Debug, Clone)]
pub struct PendingSegment {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub size_bytes: u64,
    pub path: PathBuf,
}

/// FIFO upload queue. Segments are added by the watcher and consumed by the upload loop.
pub struct UploadQueue {
    queue: Mutex<VecDeque<PendingSegment>>,
    /// Maximum number of segments to buffer before evicting oldest.
    max_segments: usize,
    segment_dir: PathBuf,
}

impl UploadQueue {
    pub fn new(segment_dir: PathBuf, max_segments: usize) -> Self {
        Self {
            queue: Mutex::new(VecDeque::new()),
            max_segments,
            segment_dir,
        }
    }

    /// Add a segment to the upload queue. Evicts oldest if over capacity.
    pub async fn enqueue(&self, segment: PendingSegment) {
        let mut q = self.queue.lock().await;
        if q.len() >= self.max_segments {
            if let Some(evicted) = q.pop_front() {
                tracing::warn!(
                    segment_id = %evicted.segment_id,
                    "evicting oldest segment from upload queue"
                );
                // Delete the evicted segment file
                let _ = tokio::fs::remove_file(&evicted.path).await;
            }
        }
        q.push_back(segment);
    }

    /// Take the next segment to upload (FIFO).
    pub async fn dequeue(&self) -> Option<PendingSegment> {
        self.queue.lock().await.pop_front()
    }

    /// Number of segments in the queue.
    #[allow(dead_code)]
    pub async fn len(&self) -> usize {
        self.queue.lock().await.len()
    }
}

/// Run the upload loop. Watches for new segments from the directory watcher,
/// uploads them to S3 via presigned URLs, confirms uploads to server, deletes local files.
pub async fn run_upload_loop(
    client: Arc<CameraHttpClient>,
    queue: Arc<UploadQueue>,
    mut segment_rx: mpsc::Receiver<NewSegment>,
    cancel: CancellationToken,
) {
    // Presigned URLs we have available
    let mut available_urls: VecDeque<PresignedUrl> = VecDeque::new();
    // Segments we've successfully uploaded (pending confirmation)
    let mut uploaded_confirmations: Vec<UploadedSegment> = Vec::new();

    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            seg = segment_rx.recv() => {
                match seg {
                    Some(new_seg) => {
                        // Convert NewSegment to PendingSegment
                        let pending = PendingSegment {
                            segment_id: new_seg.filename.clone(),
                            start_ts: new_seg.start_ts,
                            end_ts: new_seg.end_ts,
                            size_bytes: new_seg.size_bytes,
                            path: new_seg.path,
                        };

                        queue.enqueue(pending).await;

                        // Process the upload queue
                        if let Err(e) = drain_upload_queue(
                            &client,
                            &queue,
                            &mut available_urls,
                            &mut uploaded_confirmations,
                        ).await {
                            tracing::warn!("upload queue drain failed: {e}");
                        }
                    }
                    None => break,
                }
            }
        }
    }
}

/// Try to upload all queued segments.
async fn drain_upload_queue(
    client: &CameraHttpClient,
    queue: &UploadQueue,
    available_urls: &mut VecDeque<PresignedUrl>,
    uploaded_confirmations: &mut Vec<UploadedSegment>,
) -> Result<()> {
    loop {
        let segment = match queue.dequeue().await {
            Some(s) => s,
            None => break,
        };

        // Get a presigned URL (request more if we're out)
        if available_urls.is_empty() {
            replenish_urls(client, available_urls, uploaded_confirmations).await?;
        }

        let presigned = match available_urls.pop_front() {
            Some(url) => url,
            None => {
                tracing::warn!("no presigned URLs available, re-queuing segment");
                queue.enqueue(segment).await;
                break;
            }
        };

        // Read segment data from disk
        let data = match tokio::fs::read(&segment.path).await {
            Ok(d) => d,
            Err(e) => {
                tracing::warn!(
                    segment_id = %segment.segment_id,
                    "failed to read segment file: {e}"
                );
                continue;
            }
        };

        // Upload to S3
        match client.upload_file(&presigned.put_url, data).await {
            Ok(()) => {
                tracing::debug!(
                    segment_id = %segment.segment_id,
                    "segment uploaded to S3"
                );
                // Queue confirmation for next presign request
                uploaded_confirmations.push(UploadedSegment {
                    segment_id: presigned.segment_id,
                    start_ts: segment.start_ts,
                    end_ts: segment.end_ts,
                    size_bytes: segment.size_bytes,
                });
                // Delete local file
                let _ = tokio::fs::remove_file(&segment.path).await;
            }
            Err(e) => {
                tracing::warn!(
                    segment_id = %segment.segment_id,
                    "S3 upload failed: {e}"
                );
                // Re-queue for retry
                queue.enqueue(segment).await;
                break;
            }
        }
    }
    Ok(())
}

/// Request a fresh batch of presigned URLs from the server.
/// Also confirms any pending uploaded segments.
async fn replenish_urls(
    client: &CameraHttpClient,
    available_urls: &mut VecDeque<PresignedUrl>,
    uploaded_confirmations: &mut Vec<UploadedSegment>,
) -> Result<()> {
    let confirmations = std::mem::take(uploaded_confirmations);
    let resp = client
        .request_presigned_urls(10, confirmations)
        .await?;

    for url in resp.urls {
        available_urls.push_back(url);
    }

    // Handle init URL if provided (not needed for MPEG-TS, but keep for compat)
    if let Some(init_url) = resp.init_url {
        tracing::debug!("received init presigned URL from server (ignoring for MPEG-TS)");
        let _ = init_url;
    }

    Ok(())
}
