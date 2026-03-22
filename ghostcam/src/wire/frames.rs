use bytes::Bytes;

/// A video frame read from the QUIC video stream.
#[derive(Debug, Clone)]
pub struct VideoFrame {
    pub data: Bytes,
}

/// An audio frame read from the QUIC audio stream.
#[derive(Debug, Clone)]
pub struct AudioFrame {
    pub data: Bytes,
}

/// First byte written by the camera on each inbound unidirectional stream.
/// This tag identifies the stream's purpose. Persistent streams (Alerts,
/// Video, Audio) stay open for the connection lifetime. Upload streams
/// (Segment, Init, Manifest, TelemetryBuffer) are one-shot.
#[repr(u8)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InboundStreamTag {
    /// Upload: media segment data
    Segment = 0x00,
    /// Upload: init segment data
    Init = 0x01,
    /// Upload: HLS manifest
    Manifest = 0x02,
    /// Upload: buffered telemetry array
    TelemetryBuffer = 0x03,

    /// Persistent: alerts / handshake channel
    Alerts = 0x10,
    /// Persistent: video frames (length-prefixed)
    Video = 0x11,
    /// Persistent: audio frames (length-prefixed)
    Audio = 0x12,
}

impl TryFrom<u8> for InboundStreamTag {
    type Error = anyhow::Error;

    fn try_from(value: u8) -> Result<Self, Self::Error> {
        match value {
            0x00 => Ok(Self::Segment),
            0x01 => Ok(Self::Init),
            0x02 => Ok(Self::Manifest),
            0x03 => Ok(Self::TelemetryBuffer),
            0x10 => Ok(Self::Alerts),
            0x11 => Ok(Self::Video),
            0x12 => Ok(Self::Audio),
            other => Err(anyhow::anyhow!(
                "unknown inbound stream tag: 0x{:02x}",
                other
            )),
        }
    }
}

/// Keep the old name as an alias for backward compat in upload code.
pub type UploadStreamType = InboundStreamTag;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn video_frame_clone() {
        let frame = VideoFrame {
            data: Bytes::from_static(b"nal data"),
        };
        let cloned = frame.clone();
        assert_eq!(frame.data, cloned.data);
    }

    #[test]
    fn audio_frame_clone() {
        let frame = AudioFrame {
            data: Bytes::from_static(b"opus data"),
        };
        let cloned = frame.clone();
        assert_eq!(frame.data, cloned.data);
    }

    #[test]
    fn stream_tag_roundtrip() {
        for (tag, expected) in [
            (0x00, InboundStreamTag::Segment),
            (0x01, InboundStreamTag::Init),
            (0x02, InboundStreamTag::Manifest),
            (0x03, InboundStreamTag::TelemetryBuffer),
            (0x10, InboundStreamTag::Alerts),
            (0x11, InboundStreamTag::Video),
            (0x12, InboundStreamTag::Audio),
        ] {
            let parsed = InboundStreamTag::try_from(tag).unwrap();
            assert_eq!(parsed, expected);
            assert_eq!(parsed as u8, tag);
        }
    }

    #[test]
    fn stream_tag_invalid() {
        assert!(InboundStreamTag::try_from(0xFF).is_err());
        assert!(InboundStreamTag::try_from(0x04).is_err());
    }
}
