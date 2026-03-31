use std::sync::atomic::Ordering;
use std::sync::Arc;

use bytes::Bytes;
use ghostcam::wire::alert::{Alert, UploadFailReason};
use ghostcam::wire::command::Command;

use super::slot::{IngestSlot, SegmentState};

/// Dispatch an alert from a camera to the appropriate handler.
pub async fn handle_alert(slot: &Arc<IngestSlot>, alert: Alert) {
    match alert {
        Alert::Handshake { .. } => {
            tracing::debug!(device_id = %slot.device_id, "ignoring duplicate handshake");
        }
        Alert::CapabilityUpdate { streams } => {
            *slot.capabilities.write().await = streams;
            tracing::info!(device_id = %slot.device_id, "capability update");
        }
        Alert::RecordingSegment {
            segment_id,
            start_ts,
            end_ts,
            size_bytes,
            ..
        } => {
            tracing::info!(
                device_id = %slot.device_id,
                segment_id,
                start_ts,
                end_ts,
                size_bytes,
                "recording segment"
            );

            // Auto-cache: if viewers are watching, proactively fetch the segment
            // so it's ready for instant playback when the browser requests it.
            let viewers = slot.video_subscribers.load(Ordering::SeqCst);
            if viewers > 0 {
                let mut segments = slot.segments.write().await;
                if !segments.contains_key(&segment_id) {
                    segments.insert(
                        segment_id.clone(),
                        SegmentState::Uploading { waiters: vec![] },
                    );
                    drop(segments);
                    let _ = slot
                        .send_command(Command::UploadSegment {
                            seq: slot.next_seq(),
                            segment_id: segment_id.clone(),
                        })
                        .await;
                    tracing::debug!(
                        device_id = %slot.device_id,
                        segment_id,
                        viewers,
                        "auto-caching segment for active viewers"
                    );
                }
            }
        }
        Alert::SegmentEvicted { segment_id } => {
            tracing::info!(device_id = %slot.device_id, segment_id, "segment evicted");
        }
        Alert::SegmentUploaded { segment_id, .. } => {
            handle_segment_uploaded(slot, &segment_id).await;
        }
        Alert::SegmentUploadFailed {
            segment_id, reason, ..
        } => {
            handle_segment_upload_failed(slot, &segment_id, reason).await;
        }
        Alert::Ack { cmd, seq } => {
            tracing::info!(device_id = %slot.device_id, cmd, seq, "ack received");
        }
        other => {
            tracing::info!(device_id = %slot.device_id, ?other, "alert received");
        }
    }
}

/// Log the segment uploaded alert. The actual data delivery and waiter notification
/// happens in `complete_segment_upload` when the upload stream data arrives.
async fn handle_segment_uploaded(slot: &Arc<IngestSlot>, segment_id: &str) {
    tracing::info!(device_id = %slot.device_id, segment_id, "segment uploaded");
}

/// Notify waiters of upload failure and remove the segment entry.
async fn handle_segment_upload_failed(
    slot: &Arc<IngestSlot>,
    segment_id: &str,
    reason: UploadFailReason,
) {
    let mut segments = slot.segments.write().await;
    if let Some(SegmentState::Uploading { waiters }) = segments.remove(segment_id) {
        for waiter in waiters {
            let _ = waiter.send(Err(anyhow::anyhow!("segment upload failed: {:?}", reason)));
        }
    }
    tracing::info!(device_id = %slot.device_id, segment_id, ?reason, "segment upload failed");
}

/// Transition a segment to Buffered state with its data and notify any waiters.
pub async fn complete_segment_upload(slot: &Arc<IngestSlot>, segment_id: &str, data: Bytes) {
    let mut segments = slot.segments.write().await;
    let waiters = if let Some(SegmentState::Uploading { waiters }) = segments.remove(segment_id) {
        waiters
    } else {
        Vec::new()
    };

    // Notify waiters
    for waiter in waiters {
        let _ = waiter.send(Ok(data.clone()));
    }

    // Store as buffered
    segments.insert(
        segment_id.to_string(),
        SegmentState::Buffered {
            data,
            buffered_at: std::time::Instant::now(),
        },
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ingest::slot::{test_slot, test_slot_with_commands};
    use ghostcam::wire::alert::StreamKind;

    #[tokio::test]
    async fn capability_update_updates_slot() {
        let slot = test_slot("cam-1", "user-1");
        handle_alert(
            &slot,
            Alert::CapabilityUpdate {
                streams: vec![StreamKind::Video, StreamKind::Audio],
            },
        )
        .await;
        let caps = slot.capabilities.read().await;
        assert_eq!(*caps, vec![StreamKind::Video, StreamKind::Audio]);
    }

    #[tokio::test]
    async fn segment_upload_failed_errors_waiters() {
        let slot = test_slot("cam-1", "user-1");
        let (tx, rx) = tokio::sync::oneshot::channel();

        {
            let mut segments = slot.segments.write().await;
            segments.insert(
                "seg-1".to_string(),
                SegmentState::Uploading { waiters: vec![tx] },
            );
        }

        handle_alert(
            &slot,
            Alert::SegmentUploadFailed {
                seq: 1,
                segment_id: "seg-1".to_string(),
                reason: UploadFailReason::Evicted,
            },
        )
        .await;

        let result = rx.await.unwrap();
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn complete_segment_upload_notifies_waiters() {
        let slot = test_slot("cam-1", "user-1");
        let (tx, rx) = tokio::sync::oneshot::channel();

        {
            let mut segments = slot.segments.write().await;
            segments.insert(
                "seg-1".to_string(),
                SegmentState::Uploading { waiters: vec![tx] },
            );
        }

        complete_segment_upload(&slot, "seg-1", Bytes::from_static(b"segment data")).await;

        let result = rx.await.unwrap().unwrap();
        assert_eq!(result, Bytes::from_static(b"segment data"));

        // Verify it's now buffered
        let segments = slot.segments.read().await;
        assert!(matches!(
            segments.get("seg-1"),
            Some(SegmentState::Buffered { .. })
        ));
    }

    #[tokio::test]
    async fn duplicate_handshake_ignored() {
        let slot = test_slot("cam-1", "user-1");
        // Should not panic or error
        handle_alert(
            &slot,
            Alert::Handshake {
                protocol_version: 1,
                fw_version: "0.1.0".to_string(),
                streams: vec![StreamKind::Video],
            },
        )
        .await;
    }

    #[tokio::test]
    async fn unknown_alert_logged() {
        let slot = test_slot("cam-1", "user-1");
        // StorageFull is handled by the catch-all arm
        handle_alert(&slot, Alert::StorageFull { free_bytes: 0 }).await;
    }

    #[tokio::test]
    async fn recording_segment_auto_caches_when_viewers_present() {
        let (slot, mut cmd_rx) = test_slot_with_commands("cam-1", "user-1");

        // Simulate an active viewer
        slot.video_subscribers
            .store(1, std::sync::atomic::Ordering::SeqCst);

        handle_alert(
            &slot,
            Alert::RecordingSegment {
                device_id: "cam-1".to_string(),
                segment_id: "seg-42".to_string(),
                start_ts: 1000,
                end_ts: 2000,
                size_bytes: 5000,
            },
        )
        .await;

        // Should have sent an UploadSegment command
        let cmd = cmd_rx.try_recv().unwrap();
        assert!(matches!(cmd, Command::UploadSegment { segment_id, .. } if segment_id == "seg-42"));

        // Segment should be in Uploading state
        let segments = slot.segments.read().await;
        assert!(matches!(
            segments.get("seg-42"),
            Some(SegmentState::Uploading { .. })
        ));
    }

    #[tokio::test]
    async fn recording_segment_skips_cache_when_no_viewers() {
        let (slot, mut cmd_rx) = test_slot_with_commands("cam-1", "user-1");

        // No viewers — video_subscribers is 0 by default

        handle_alert(
            &slot,
            Alert::RecordingSegment {
                device_id: "cam-1".to_string(),
                segment_id: "seg-42".to_string(),
                start_ts: 1000,
                end_ts: 2000,
                size_bytes: 5000,
            },
        )
        .await;

        // No command should have been sent
        assert!(cmd_rx.try_recv().is_err());

        // No segment state entry
        let segments = slot.segments.read().await;
        assert!(segments.get("seg-42").is_none());
    }
}
