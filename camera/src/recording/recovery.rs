use std::path::Path;

use anyhow::Result;
use tokio::sync::mpsc;

use super::manifest::generate_manifest;
use super::ring_buffer::RingBuffer;
use super::SegmentEvent;

/// Scan the segment directory and recover state after a crash.
///
/// 1. Scan directory for .m4s files
/// 2. Delete any files that are empty or clearly corrupt
/// 3. Rebuild the ring buffer and manifest from surviving segments
pub async fn recover(
    segment_dir: &Path,
    _device_id: &str,
    event_tx: mpsc::Sender<SegmentEvent>,
) -> Result<(RingBuffer, String)> {
    // Ensure directory exists
    tokio::fs::create_dir_all(segment_dir).await?;

    // Clean up empty/corrupt files
    if segment_dir.exists() {
        let mut entries = tokio::fs::read_dir(segment_dir).await?;
        while let Some(entry) = entries.next_entry().await? {
            let path = entry.path();
            if path.extension().map_or(false, |e| e == "m4s") {
                let meta = entry.metadata().await?;
                if meta.len() == 0 {
                    tracing::warn!(path = %path.display(), "deleting empty segment file");
                    let _ = tokio::fs::remove_file(&path).await;
                }
            }
        }
    }

    let ring_buffer = RingBuffer::scan(segment_dir, event_tx).await?;
    let manifest = generate_manifest(ring_buffer.segments());

    tracing::info!(
        segments = ring_buffer.segments().len(),
        "recording recovery complete"
    );

    Ok((ring_buffer, manifest))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn recover_empty_dir() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let (rb, manifest) = recover(dir.path(), "cam-01", tx).await.unwrap();
        assert!(rb.segments().is_empty());
        assert!(manifest.contains("#EXTM3U"));
        assert!(!manifest.contains("#EXTINF"));
    }

    #[tokio::test]
    async fn recover_valid_segments() {
        let dir = tempfile::tempdir().unwrap();
        for i in 0..3 {
            let path = dir.path().join(format!("cam:{}000.m4s", i));
            tokio::fs::write(&path, vec![0u8; 100]).await.unwrap();
        }
        let (tx, _rx) = mpsc::channel(16);
        let (rb, manifest) = recover(dir.path(), "cam", tx).await.unwrap();
        assert_eq!(rb.segments().len(), 3);
        assert_eq!(manifest.matches("#EXTINF").count(), 3);
    }

    #[tokio::test]
    async fn recover_deletes_empty() {
        let dir = tempfile::tempdir().unwrap();
        // Valid segment
        tokio::fs::write(dir.path().join("cam:1000.m4s"), vec![0u8; 100])
            .await
            .unwrap();
        // Empty (corrupt) segment
        tokio::fs::write(dir.path().join("cam:2000.m4s"), b"")
            .await
            .unwrap();

        let (tx, _rx) = mpsc::channel(16);
        let (rb, _) = recover(dir.path(), "cam", tx).await.unwrap();
        assert_eq!(rb.segments().len(), 1);
    }
}
