use std::path::{Path, PathBuf};

use anyhow::Result;
use tokio::sync::mpsc;

use super::SegmentEvent;

/// Info about a segment on disk.
#[derive(Debug, Clone)]
pub struct SegmentInfo {
    pub segment_id: String,
    pub start_ts: u64,
    pub end_ts: u64,
    pub path: PathBuf,
}

/// Manages segment files on disk, enforcing storage limits.
pub struct RingBuffer {
    segments: Vec<SegmentInfo>,
}

impl RingBuffer {
    /// Scan the segment directory and rebuild state from files on disk.
    pub async fn scan(dir: &Path, _event_tx: mpsc::Sender<SegmentEvent>) -> Result<Self> {
        let mut segments = Vec::new();

        if dir.exists() {
            let mut entries = tokio::fs::read_dir(dir).await?;
            while let Some(entry) = entries.next_entry().await? {
                let path = entry.path();
                if path.extension().is_some_and(|e| e == "m4s") {
                    let file_name = path
                        .file_stem()
                        .and_then(|s| s.to_str())
                        .unwrap_or("")
                        .to_string();
                    let _meta = entry.metadata().await?;
                    // Parse start_ts from segment_id (format: device_id:timestamp)
                    let start_ts = file_name
                        .rsplit(':')
                        .next()
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(0);
                    segments.push(SegmentInfo {
                        segment_id: file_name,
                        start_ts,
                        end_ts: start_ts + 10_000, // Estimate
                        path,
                    });
                }
            }
        } else {
            tokio::fs::create_dir_all(dir).await?;
        }

        // Sort by start timestamp
        segments.sort_by_key(|s| s.start_ts);

        Ok(Self { segments })
    }

    /// Register a newly finalized segment.
    pub fn register(&mut self, info: SegmentInfo) {
        self.segments.push(info);
    }

    /// Get all segments.
    pub fn segments(&self) -> &[SegmentInfo] {
        &self.segments
    }

    /// Look up a segment by ID. Accepts IDs with or without `.m4s` extension.
    pub fn get_segment_path(&self, segment_id: &str) -> Option<&Path> {
        let id = segment_id.strip_suffix(".m4s").unwrap_or(segment_id);
        self.segments
            .iter()
            .find(|s| s.segment_id == id)
            .map(|s| s.path.as_path())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn scan_empty_dir() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        assert!(rb.segments().is_empty());
    }

    #[tokio::test]
    async fn scan_existing_segments() {
        let dir = tempfile::tempdir().unwrap();
        for i in 0..3 {
            let path = dir.path().join(format!("cam:{}000.m4s", i));
            tokio::fs::write(&path, vec![0u8; 100]).await.unwrap();
        }
        let (tx, _rx) = mpsc::channel(16);
        let rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        assert_eq!(rb.segments().len(), 3);
    }

    #[tokio::test]
    async fn scan_ignores_non_m4s() {
        let dir = tempfile::tempdir().unwrap();
        tokio::fs::write(dir.path().join("cam:0.m4s"), b"seg")
            .await
            .unwrap();
        tokio::fs::write(dir.path().join("notes.txt"), b"txt")
            .await
            .unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        assert_eq!(rb.segments().len(), 1);
    }

    #[tokio::test]
    async fn register_adds_segment() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let mut rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        rb.register(SegmentInfo {
            segment_id: "s1".to_string(),
            start_ts: 0,
            end_ts: 10000,
            path: dir.path().join("s1.m4s"),
        });
        assert_eq!(rb.segments().len(), 1);
    }

    #[tokio::test]
    async fn get_segment_path_found() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let mut rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        let path = dir.path().join("s1.m4s");
        rb.register(SegmentInfo {
            segment_id: "s1".to_string(),
            start_ts: 0,
            end_ts: 10000,
            path: path.clone(),
        });
        assert_eq!(rb.get_segment_path("s1"), Some(path.as_path()));
    }

    #[tokio::test]
    async fn get_segment_path_not_found() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, _rx) = mpsc::channel(16);
        let rb = RingBuffer::scan(dir.path(), tx).await.unwrap();
        assert!(rb.get_segment_path("nonexistent").is_none());
    }
}
