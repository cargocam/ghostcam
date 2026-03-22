use anyhow::Result;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use super::ring_buffer::RingBuffer;
use super::SegmentEvent;

pub enum StorageAction {
    /// Space recovered, continue recording.
    Recovered,
    /// Recording paused, polling for space.
    Paused,
}

/// Handle a storage-full condition.
///
/// 1. Attempt emergency eviction (oldest 5 segments)
/// 2. Check if space recovered
/// 3. If not: emit StorageFull event, return Paused
pub async fn handle_storage_full(
    ring_buffer: &mut RingBuffer,
    event_tx: &mpsc::Sender<SegmentEvent>,
) -> Result<StorageAction> {
    // Try emergency eviction
    let evicted = ring_buffer.emergency_evict(5).await?;
    tracing::warn!(count = evicted.len(), "emergency segment eviction");

    // Check space
    let available = ring_buffer.available_space().await.unwrap_or(0);
    if available > 1_000_000 {
        // > 1MB free, recovered
        return Ok(StorageAction::Recovered);
    }

    // Still full
    let _ = event_tx
        .send(SegmentEvent::StorageFull {
            free_bytes: available,
        })
        .await;

    Ok(StorageAction::Paused)
}

/// Poll free space while recording is paused.
pub async fn poll_for_space(
    ring_buffer: &RingBuffer,
    event_tx: &mpsc::Sender<SegmentEvent>,
    cancel: CancellationToken,
) -> Result<()> {
    let interval = std::time::Duration::from_secs(60);

    loop {
        tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            _ = tokio::time::sleep(interval) => {}
        }

        let available = ring_buffer.available_space().await.unwrap_or(0);
        if available > 10_000_000 {
            // > 10MB free, resume
            let _ = event_tx
                .send(SegmentEvent::StorageResumed {
                    free_bytes: available,
                })
                .await;
            return Ok(());
        }
    }
}
