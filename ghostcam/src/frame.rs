use bytes::{Buf, BufMut, Bytes, BytesMut};
use std::io;

/// Stream type discriminator in frame header.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum StreamType {
    Video = 0,
    Audio = 1,
    Telemetry = 2,
}

impl TryFrom<u8> for StreamType {
    type Error = io::Error;
    fn try_from(v: u8) -> Result<Self, Self::Error> {
        match v {
            0 => Ok(Self::Video),
            1 => Ok(Self::Audio),
            2 => Ok(Self::Telemetry),
            _ => Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("unknown stream type: {v}"),
            )),
        }
    }
}

/// 13-byte wire header: stream_type(u8) + timestamp_us(u64 BE) + payload_len(u32 BE)
pub const HEADER_LEN: usize = 1 + 8 + 4;

#[derive(Debug, Clone)]
pub struct Frame {
    pub stream_type: StreamType,
    pub timestamp_us: u64,
    pub payload: Bytes,
}

impl Frame {
    /// Encode frame header + payload into a contiguous buffer.
    pub fn encode(&self) -> Bytes {
        let mut buf = BytesMut::with_capacity(HEADER_LEN + self.payload.len());
        buf.put_u8(self.stream_type as u8);
        buf.put_u64(self.timestamp_us);
        buf.put_u32(self.payload.len() as u32);
        buf.put(self.payload.clone());
        buf.freeze()
    }

    /// Decode header from a buffer (must contain at least HEADER_LEN bytes).
    /// Returns (stream_type, timestamp_us, payload_len).
    pub fn decode_header(buf: &[u8]) -> io::Result<(StreamType, u64, u32)> {
        if buf.len() < HEADER_LEN {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "frame header too short",
            ));
        }
        let mut cursor = &buf[..HEADER_LEN];
        let stream_type = StreamType::try_from(cursor.get_u8())?;
        let timestamp_us = cursor.get_u64();
        let payload_len = cursor.get_u32();
        Ok((stream_type, timestamp_us, payload_len))
    }

    /// Decode a complete frame from a buffer.
    pub fn decode(buf: &[u8]) -> io::Result<Self> {
        let (stream_type, timestamp_us, payload_len) = Self::decode_header(buf)?;
        let total = HEADER_LEN + payload_len as usize;
        if buf.len() < total {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                format!("need {total} bytes, have {}", buf.len()),
            ));
        }
        let payload = Bytes::copy_from_slice(&buf[HEADER_LEN..total]);
        Ok(Self {
            stream_type,
            timestamp_us,
            payload,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let frame = Frame {
            stream_type: StreamType::Video,
            timestamp_us: 1_000_000,
            payload: Bytes::from_static(b"hello"),
        };
        let encoded = frame.encode();
        assert_eq!(encoded.len(), HEADER_LEN + 5);
        let decoded = Frame::decode(&encoded).unwrap();
        assert_eq!(decoded.stream_type, StreamType::Video);
        assert_eq!(decoded.timestamp_us, 1_000_000);
        assert_eq!(decoded.payload, Bytes::from_static(b"hello"));
    }

    #[test]
    fn roundtrip_telemetry() {
        let frame = Frame {
            stream_type: StreamType::Telemetry,
            timestamp_us: 42_000,
            payload: Bytes::from_static(b"\x93\xa3cpu"),
        };
        let encoded = frame.encode();
        let decoded = Frame::decode(&encoded).unwrap();
        assert_eq!(decoded.stream_type, StreamType::Telemetry);
        assert_eq!(decoded.timestamp_us, 42_000);
        assert_eq!(decoded.payload, Bytes::from_static(b"\x93\xa3cpu"));
    }
}
