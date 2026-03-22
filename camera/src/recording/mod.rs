pub mod init;
pub mod manifest;
pub mod manifest_push;
pub mod muxer;
pub mod recovery;
pub mod ring_buffer;
pub mod segment;
pub mod storage;
pub mod uploads;

use bytes::Bytes;

/// Events emitted by the recording pipeline.
#[derive(Debug, Clone)]
pub enum SegmentEvent {
    /// A segment has been finalized and written to disk.
    Finalized {
        segment_id: String,
        start_ts: u64,
        end_ts: u64,
        size_bytes: u64,
    },
    /// A segment has been evicted from the ring buffer.
    Evicted { segment_id: String },
    /// The manifest has been updated.
    ManifestUpdated { manifest: String },
    /// Init segment has been generated.
    InitReady { data: Bytes },
    /// Storage full — recording paused.
    StorageFull { free_bytes: u64 },
    /// Storage resumed.
    StorageResumed { free_bytes: u64 },
}
