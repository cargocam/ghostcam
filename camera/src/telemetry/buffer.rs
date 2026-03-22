use std::path::{Path, PathBuf};

use anyhow::Result;
use ghostcam::config::TELEMETRY_BUFFER_CAP;
use ghostcam::telemetry::TelemetryDatagram;
use tokio::sync::RwLock;

/// On-disk buffer for telemetry datagrams generated while disconnected.
pub struct TelemetryBuffer {
    entries: RwLock<Vec<TelemetryDatagram>>,
    path: PathBuf,
    cap: usize,
}

impl TelemetryBuffer {
    /// Load the buffer from disk (or create empty if file doesn't exist).
    pub fn load(path: &Path) -> Result<Self> {
        let entries = if path.exists() {
            let data = std::fs::read(path)?;
            if data.is_empty() {
                Vec::new()
            } else {
                TelemetryDatagram::decode_batch(&data).unwrap_or_default()
            }
        } else {
            Vec::new()
        };

        tracing::debug!(count = entries.len(), "telemetry buffer loaded");

        Ok(Self {
            entries: RwLock::new(entries),
            path: path.to_path_buf(),
            cap: TELEMETRY_BUFFER_CAP,
        })
    }

    /// Append a datagram with run-length deduplication.
    ///
    /// If the new entry equals the last entry, and also equals the second-to-last,
    /// we just update the timestamp of the last entry (collapsing identical runs
    /// to first + last). Otherwise we push normally.
    pub async fn push(&self, datagram: TelemetryDatagram) {
        let mut entries = self.entries.write().await;

        let len = entries.len();
        if len >= 2 {
            let last = &entries[len - 1];
            let second_last = &entries[len - 2];
            if datagrams_equal_ignoring_ts(last, &datagram)
                && datagrams_equal_ignoring_ts(second_last, &datagram)
            {
                // Collapse: just update timestamp of last entry
                entries[len - 1].ts = datagram.ts;
                return;
            }
        }

        entries.push(datagram);

        // Evict oldest if over cap
        if entries.len() > self.cap {
            let overflow = entries.len() - self.cap;
            entries.drain(..overflow);
        }
    }

    /// Drain all entries for upload.
    pub async fn drain(&self) -> Vec<TelemetryDatagram> {
        let mut entries = self.entries.write().await;
        std::mem::take(&mut *entries)
    }

    /// Check if the buffer has entries.
    pub async fn is_empty(&self) -> bool {
        self.entries.read().await.is_empty()
    }

    /// Entry count.
    pub async fn len(&self) -> usize {
        self.entries.read().await.len()
    }

    /// Flush in-memory buffer to disk.
    pub async fn flush_to_disk(&self) -> Result<()> {
        let entries = self.entries.read().await;
        if entries.is_empty() {
            // Remove file if it exists
            let _ = tokio::fs::remove_file(&self.path).await;
            return Ok(());
        }
        let data = TelemetryDatagram::encode_batch(&entries);
        if let Some(parent) = self.path.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        tokio::fs::write(&self.path, &data).await?;
        Ok(())
    }

    /// Clear the on-disk buffer (after successful upload).
    pub async fn clear_disk(&self) -> Result<()> {
        let _ = tokio::fs::remove_file(&self.path).await;
        Ok(())
    }
}

/// Compare two datagrams ignoring the timestamp field.
fn datagrams_equal_ignoring_ts(a: &TelemetryDatagram, b: &TelemetryDatagram) -> bool {
    a.cpu == b.cpu
        && a.mem == b.mem
        && a.temp == b.temp
        && a.uptime == b.uptime
        && a.sig == b.sig
        && a.fps == b.fps
        && a.kbps == b.kbps
        && a.lat == b.lat
        && a.lon == b.lon
        && a.alt == b.alt
        && a.gps_fix == b.gps_fix
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_datagram(ts: u64, cpu: u32) -> TelemetryDatagram {
        TelemetryDatagram {
            ts,
            cpu: Some(cpu),
            ..Default::default()
        }
    }

    #[tokio::test]
    async fn push_and_drain() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("buf.bin")).unwrap();

        for i in 0..5 {
            buf.push(make_datagram(i, i as u32 * 10)).await;
        }

        let entries = buf.drain().await;
        assert_eq!(entries.len(), 5);
        assert!(buf.is_empty().await);
    }

    #[tokio::test]
    async fn push_dedup_three_identical() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("buf.bin")).unwrap();

        buf.push(make_datagram(1, 50)).await;
        buf.push(make_datagram(2, 50)).await;
        buf.push(make_datagram(3, 50)).await;

        let entries = buf.drain().await;
        // First push: [A(t=1)] (len=1)
        // Second push: [A(t=1), A(t=2)] (len=2, no dedup — second_last doesn't exist for first push)
        // Third push: last=A(t=2), second_last=A(t=1), new=A(t=3) — all equal → update last ts
        // Result: [A(t=1), A(t=3)]
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[0].ts, 1);
        assert_eq!(entries[1].ts, 3);
    }

    #[tokio::test]
    async fn push_dedup_five_identical() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("buf.bin")).unwrap();

        for i in 1..=5 {
            buf.push(make_datagram(i, 50)).await;
        }

        let entries = buf.drain().await;
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[0].ts, 1);
        assert_eq!(entries[1].ts, 5);
    }

    #[tokio::test]
    async fn push_dedup_change_resets() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("buf.bin")).unwrap();

        // A, A, A, B, B, B
        buf.push(make_datagram(1, 50)).await;
        buf.push(make_datagram(2, 50)).await;
        buf.push(make_datagram(3, 50)).await;
        buf.push(make_datagram(4, 80)).await;
        buf.push(make_datagram(5, 80)).await;
        buf.push(make_datagram(6, 80)).await;

        let entries = buf.drain().await;
        // A(1), A(3), B(4), B(6)
        assert_eq!(entries.len(), 4);
        assert_eq!(entries[0].ts, 1);
        assert_eq!(entries[0].cpu, Some(50));
        assert_eq!(entries[1].ts, 3);
        assert_eq!(entries[1].cpu, Some(50));
        assert_eq!(entries[2].ts, 4);
        assert_eq!(entries[2].cpu, Some(80));
        assert_eq!(entries[3].ts, 6);
        assert_eq!(entries[3].cpu, Some(80));
    }

    #[tokio::test]
    async fn push_respects_cap() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("buf.bin");
        let buf = TelemetryBuffer {
            entries: RwLock::new(Vec::new()),
            path,
            cap: 10,
        };

        for i in 0..15 {
            // Use different cpu values to avoid dedup
            buf.push(make_datagram(i, i as u32)).await;
        }

        assert_eq!(buf.len().await, 10);
    }

    #[tokio::test]
    async fn load_empty() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("nonexistent.bin")).unwrap();
        assert!(buf.is_empty().await);
    }

    #[tokio::test]
    async fn flush_and_reload() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("buf.bin");

        {
            let buf = TelemetryBuffer::load(&path).unwrap();
            buf.push(make_datagram(1, 50)).await;
            buf.push(make_datagram(2, 60)).await;
            buf.flush_to_disk().await.unwrap();
        }

        let buf = TelemetryBuffer::load(&path).unwrap();
        assert_eq!(buf.len().await, 2);
    }

    #[tokio::test]
    async fn drain_clears() {
        let dir = tempfile::tempdir().unwrap();
        let buf = TelemetryBuffer::load(&dir.path().join("buf.bin")).unwrap();
        buf.push(make_datagram(1, 50)).await;
        let _ = buf.drain().await;
        assert!(buf.is_empty().await);
    }

    #[tokio::test]
    async fn clear_disk() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("buf.bin");
        let buf = TelemetryBuffer::load(&path).unwrap();
        buf.push(make_datagram(1, 50)).await;
        buf.flush_to_disk().await.unwrap();
        assert!(path.exists());
        buf.clear_disk().await.unwrap();
        assert!(!path.exists());
    }
}
