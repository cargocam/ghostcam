use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use bytes::Bytes;
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use super::init::generate_init_segment;
use super::manifest::generate_manifest;
use super::ring_buffer::{RingBuffer, SegmentInfo};
use super::segment::SegmentWriter;
use super::SegmentEvent;

/// The fMP4 muxer: consumes frames and writes segments to disk.
pub struct Muxer {
    segment_dir: PathBuf,
    device_id: String,
    segment_duration: Duration,
    event_tx: mpsc::Sender<SegmentEvent>,
    ring_buffer: Arc<RwLock<RingBuffer>>,
    init_generated: bool,
}

impl Muxer {
    pub fn new(
        segment_dir: PathBuf,
        device_id: String,
        event_tx: mpsc::Sender<SegmentEvent>,
        ring_buffer: Arc<RwLock<RingBuffer>>,
    ) -> Self {
        Self {
            segment_dir,
            device_id,
            segment_duration: Duration::from_secs(
                ghostcam::config::SEGMENT_DURATION_SECS,
            ),
            event_tx,
            ring_buffer,
            init_generated: false,
        }
    }

    /// Run the muxer loop.
    pub async fn run(
        &mut self,
        mut video_rx: broadcast::Receiver<Bytes>,
        mut audio_rx: broadcast::Receiver<Bytes>,
        cancel: CancellationToken,
    ) -> Result<()> {
        let mut current_segment: Option<SegmentWriter> = None;
        let mut segment_start = std::time::Instant::now();
        let mut sps_buf: Option<Vec<u8>> = None;
        let mut pps_buf: Option<Vec<u8>> = None;

        loop {
            tokio::select! {
                _ = cancel.cancelled() => {
                    // Finalize current segment if any
                    if let Some(sw) = current_segment.take() {
                        let meta = sw.finalize().await?;
                        self.ring_buffer.write().await.register(SegmentInfo {
                            segment_id: meta.segment_id.clone(),
                            start_ts: meta.start_ts,
                            end_ts: meta.end_ts,
                            size_bytes: meta.size_bytes,
                            path: meta.path,
                        });
                        if self.event_tx.try_send(SegmentEvent::Finalized {
                            segment_id: meta.segment_id,
                            start_ts: meta.start_ts,
                            end_ts: meta.end_ts,
                            size_bytes: meta.size_bytes,
                        }).is_err() {
                            tracing::warn!("segment event channel full, dropping finalized event");
                        }
                    }
                    return Ok(());
                }

                result = video_rx.recv() => {
                    let nal = match result {
                        Ok(data) => data,
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!("muxer video lagged by {n} frames — recording may have gaps");
                            continue;
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    };

                    // Parse NAL type
                    if nal.is_empty() { continue; }
                    let nal_type = nal[0] & 0x1F;

                    // Cache SPS/PPS
                    match nal_type {
                        7 => { sps_buf = Some(nal.to_vec()); continue; }
                        8 => { pps_buf = Some(nal.to_vec()); continue; }
                        _ => {}
                    }

                    // Generate init segment on first IDR
                    if !self.init_generated && nal_type == 5 {
                        if let (Some(sps), Some(pps)) = (&sps_buf, &pps_buf) {
                            match generate_init_segment(sps, pps) {
                                Ok(init_data) => {
                                    let _ = self.event_tx.try_send(SegmentEvent::InitReady {
                                        data: init_data,
                                    });
                                    self.init_generated = true;
                                }
                                Err(e) => {
                                    tracing::warn!("failed to generate init segment: {e}");
                                }
                            }
                        }
                    }

                    // Check if we need a new segment (IDR + duration exceeded)
                    let is_idr = nal_type == 5;
                    if is_idr && current_segment.is_some() {
                        if segment_start.elapsed() >= self.segment_duration {
                            // Finalize current segment
                            if let Some(sw) = current_segment.take() {
                                let meta = sw.finalize().await?;
                                self.ring_buffer.write().await.register(SegmentInfo {
                                    segment_id: meta.segment_id.clone(),
                                    start_ts: meta.start_ts,
                                    end_ts: meta.end_ts,
                                    size_bytes: meta.size_bytes,
                                    path: meta.path,
                                });
                                let _ = self.event_tx.send(SegmentEvent::Finalized {
                                    segment_id: meta.segment_id,
                                    start_ts: meta.start_ts,
                                    end_ts: meta.end_ts,
                                    size_bytes: meta.size_bytes,
                                }).await;

                                // Update and emit manifest
                                let manifest = generate_manifest(self.ring_buffer.read().await.segments());
                                let _ = self.event_tx.try_send(SegmentEvent::ManifestUpdated {
                                    manifest,
                                });
                            }
                        }
                    }

                    // Start new segment if needed
                    if current_segment.is_none() && self.init_generated {
                        let now_ms = std::time::SystemTime::now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .unwrap()
                            .as_millis() as u64;
                        let seg_id = format!("{}:{}", self.device_id, now_ms);
                        current_segment = Some(
                            SegmentWriter::create(&self.segment_dir, &seg_id, now_ms).await?
                        );
                        segment_start = std::time::Instant::now();
                    }

                    // Write NAL to current segment
                    if let Some(ref mut sw) = current_segment {
                        let ts = segment_start.elapsed();
                        sw.write_video(&nal, ts).await?;
                    }
                }

                result = audio_rx.recv() => {
                    let frame = match result {
                        Ok(data) => data,
                        Err(broadcast::error::RecvError::Lagged(_)) => continue,
                        Err(broadcast::error::RecvError::Closed) => break,
                    };

                    if let Some(ref mut sw) = current_segment {
                        let ts = segment_start.elapsed();
                        sw.write_audio(&frame, ts).await?;
                    }
                }
            }
        }

        Ok(())
    }
}
