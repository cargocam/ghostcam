pub mod video;
pub mod audio;
pub mod telemetry;

use bytes::Bytes;

/// Messages produced by capture modules, consumed by the QUIC send loop.
#[derive(Debug, Clone)]
pub enum CaptureMessage {
    /// A single H.264 NAL unit (without Annex-B start codes).
    VideoNal { nal_data: Bytes, nal_type: u8 },
    /// An Opus-encoded audio frame.
    Audio { opus_data: Bytes },
    /// MessagePack-encoded SparseTelemetry.
    Telemetry { msgpack_data: Bytes },
}
