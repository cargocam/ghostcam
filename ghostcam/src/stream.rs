use anyhow::Result;
use crate::frame::{Frame, StreamType};
use quinn::SendStream;

/// Send a single video frame over a unidirectional QUIC stream.
/// Each frame gets its own stream (stream-per-frame for simplicity in MVP).
pub async fn send_video_frame(
    mut stream: SendStream,
    timestamp_us: u64,
    nal_data: &[u8],
) -> Result<()> {
    let frame = Frame {
        stream_type: StreamType::Video,
        timestamp_us,
        payload: bytes::Bytes::copy_from_slice(nal_data),
    };
    let encoded = frame.encode();
    stream.write_all(&encoded).await?;
    stream.finish()?;
    Ok(())
}

/// Send a single audio frame over a unidirectional QUIC stream.
pub async fn send_audio_frame(
    mut stream: SendStream,
    timestamp_us: u64,
    opus_data: &[u8],
) -> Result<()> {
    let frame = Frame {
        stream_type: StreamType::Audio,
        timestamp_us,
        payload: bytes::Bytes::copy_from_slice(opus_data),
    };
    let encoded = frame.encode();
    stream.write_all(&encoded).await?;
    stream.finish()?;
    Ok(())
}

/// Send a single telemetry frame over a unidirectional QUIC stream.
pub async fn send_telemetry_frame(
    mut stream: SendStream,
    timestamp_us: u64,
    msgpack_data: &[u8],
) -> Result<()> {
    let frame = Frame {
        stream_type: StreamType::Telemetry,
        timestamp_us,
        payload: bytes::Bytes::copy_from_slice(msgpack_data),
    };
    let encoded = frame.encode();
    stream.write_all(&encoded).await?;
    stream.finish()?;
    Ok(())
}

/// Opus silence frame: a valid 3-byte Opus packet encoding silence.
/// Code 0, mono, 20ms frame, all zeros = silence.
pub const OPUS_SILENCE: &[u8] = &[0xF8, 0xFF, 0xFE];
