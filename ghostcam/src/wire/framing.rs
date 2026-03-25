use crate::config::MAX_FRAME_SIZE;
use serde::{de::DeserializeOwned, Serialize};
use thiserror::Error;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

#[derive(Debug, Error)]
pub enum FramingError {
    #[error("frame too large: {size} bytes (max {MAX_FRAME_SIZE})")]
    FrameTooLarge { size: u32 },

    #[error("truncated length prefix")]
    TruncatedLength,

    #[error("truncated payload: expected {expected} bytes, got {got}")]
    TruncatedPayload { expected: u32, got: usize },

    #[error("io error: {0}")]
    Io(#[from] std::io::Error),

    #[error("json error: {0}")]
    Json(#[from] serde_json::Error),
}

/// Write a length-prefixed frame to an async writer.
pub async fn write_frame<W>(writer: &mut W, data: &[u8]) -> Result<(), FramingError>
where
    W: AsyncWrite + Unpin,
{
    let len = data.len() as u32;
    if len > MAX_FRAME_SIZE {
        return Err(FramingError::FrameTooLarge { size: len });
    }
    writer.write_all(&len.to_be_bytes()).await?;
    writer.write_all(data).await?;
    Ok(())
}

/// Read a length-prefixed frame from an async reader.
/// Returns `Ok(None)` on clean EOF (no bytes available).
/// Returns `Err` on truncated reads or oversized frames.
pub async fn read_frame<R>(reader: &mut R) -> Result<Option<Vec<u8>>, FramingError>
where
    R: AsyncRead + Unpin,
{
    // Try reading the 4-byte length prefix. EOF here means clean stream close.
    let mut len_buf = [0u8; 4];
    let mut total = 0;
    while total < 4 {
        match reader.read(&mut len_buf[total..]).await? {
            0 if total == 0 => return Ok(None),             // Clean EOF
            0 => return Err(FramingError::TruncatedLength), // Partial length
            n => total += n,
        }
    }

    let len = u32::from_be_bytes(len_buf);
    if len > MAX_FRAME_SIZE {
        return Err(FramingError::FrameTooLarge { size: len });
    }

    let mut buf = vec![0u8; len as usize];
    let mut read = 0;
    while read < len as usize {
        match reader.read(&mut buf[read..]).await? {
            0 => {
                return Err(FramingError::TruncatedPayload {
                    expected: len,
                    got: read,
                });
            }
            n => read += n,
        }
    }
    Ok(Some(buf))
}

/// Serialize a value as JSON and write as a length-prefixed frame.
pub async fn write_json<T, W>(writer: &mut W, value: &T) -> Result<(), FramingError>
where
    T: Serialize,
    W: AsyncWrite + Unpin,
{
    let data = serde_json::to_vec(value)?;
    write_frame(writer, &data).await
}

/// Read a length-prefixed frame and deserialize from JSON.
/// Returns `Ok(None)` on clean EOF.
pub async fn read_json<T, R>(reader: &mut R) -> Result<Option<T>, FramingError>
where
    T: DeserializeOwned,
    R: AsyncRead + Unpin,
{
    match read_frame(reader).await? {
        Some(data) => Ok(Some(serde_json::from_slice(&data)?)),
        None => Ok(None),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::wire::alert::{Alert, StreamKind};
    use crate::wire::command::Command;

    #[tokio::test]
    async fn write_read_roundtrip() {
        let data = b"hello, world!";
        let mut buf = Vec::new();
        write_frame(&mut buf, data).await.unwrap();

        let mut cursor = &buf[..];
        let result = read_frame(&mut cursor).await.unwrap();
        assert_eq!(result.unwrap(), data);
    }

    #[tokio::test]
    async fn write_read_empty_frame() {
        let mut buf = Vec::new();
        write_frame(&mut buf, &[]).await.unwrap();

        let mut cursor = &buf[..];
        let result = read_frame(&mut cursor).await.unwrap();
        assert_eq!(result.unwrap(), Vec::<u8>::new());
    }

    #[tokio::test]
    async fn write_read_max_size_frame() {
        let data = vec![0xABu8; MAX_FRAME_SIZE as usize];
        let mut buf = Vec::new();
        write_frame(&mut buf, &data).await.unwrap();

        let mut cursor = &buf[..];
        let result = read_frame(&mut cursor).await.unwrap();
        assert_eq!(result.unwrap().len(), MAX_FRAME_SIZE as usize);
    }

    #[tokio::test]
    async fn read_oversized_frame_rejected() {
        let len = (MAX_FRAME_SIZE + 1).to_be_bytes();
        let mut cursor = &len[..];
        let result = read_frame(&mut cursor).await;
        assert!(matches!(result, Err(FramingError::FrameTooLarge { .. })));
    }

    #[tokio::test]
    async fn read_eof_returns_none() {
        let mut cursor: &[u8] = &[];
        let result = read_frame(&mut cursor).await.unwrap();
        assert!(result.is_none());
    }

    #[tokio::test]
    async fn read_truncated_length_returns_error() {
        // Only 2 bytes of a 4-byte length prefix
        let mut cursor: &[u8] = &[0, 0];
        let result = read_frame(&mut cursor).await;
        assert!(matches!(result, Err(FramingError::TruncatedLength)));
    }

    #[tokio::test]
    async fn read_truncated_payload_returns_error() {
        // Length says 100 bytes, but only 4 bytes of payload follow
        let mut buf = Vec::new();
        buf.extend_from_slice(&100u32.to_be_bytes());
        buf.extend_from_slice(&[1, 2, 3, 4]);

        let mut cursor = &buf[..];
        let result = read_frame(&mut cursor).await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn write_read_multiple_frames() {
        let frames = [b"first".as_slice(), b"second", b"third"];
        let mut buf = Vec::new();
        for frame in &frames {
            write_frame(&mut buf, frame).await.unwrap();
        }

        let mut cursor = &buf[..];
        for expected in &frames {
            let result = read_frame(&mut cursor).await.unwrap().unwrap();
            assert_eq!(result, *expected);
        }
        // EOF after all frames
        assert!(read_frame(&mut cursor).await.unwrap().is_none());
    }

    #[tokio::test]
    async fn write_read_json_roundtrip() {
        let alert = Alert::Handshake {
            protocol_version: 1,
            fw_version: "0.1.0".into(),
            streams: vec![StreamKind::Video, StreamKind::Audio],
        };

        let mut buf = Vec::new();
        write_json(&mut buf, &alert).await.unwrap();

        let mut cursor = &buf[..];
        let back: Alert = read_json(&mut cursor).await.unwrap().unwrap();
        assert_eq!(alert, back);
    }

    #[tokio::test]
    async fn write_read_json_command() {
        let cmd = Command::StartVideo { seq: 42 };

        let mut buf = Vec::new();
        write_json(&mut buf, &cmd).await.unwrap();

        let mut cursor = &buf[..];
        let back: Command = read_json(&mut cursor).await.unwrap().unwrap();
        assert_eq!(cmd, back);
    }
}
