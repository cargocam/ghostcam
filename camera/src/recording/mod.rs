pub mod init;
pub mod manifest;
pub mod manifest_push;
pub mod muxer;
pub mod ring_buffer;
pub mod segment;
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
    /// The manifest has been updated.
    ManifestUpdated { manifest: String },
    /// Init segment has been generated.
    InitReady { data: Bytes },
}
