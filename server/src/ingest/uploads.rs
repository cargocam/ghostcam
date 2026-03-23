use std::sync::Arc;

use anyhow::Result;
use bytes::Bytes;
use ghostcam::config::MAX_FRAME_SIZE;
use ghostcam::telemetry::TelemetryDatagram;

use super::alerts::complete_segment_upload;
use super::slot::IngestSlot;
use crate::frames::InboundStreamTag;

/// Handle an inbound unidirectional stream from the camera.
/// Reads the 1-byte type tag and dispatches to the appropriate handler.
pub async fn handle_upload_stream(
    slot: &Arc<IngestSlot>,
    mut stream: quinn::RecvStream,
) -> Result<()> {
    let mut tag = [0u8; 1];
    stream.read_exact(&mut tag).await?;
    let tag = InboundStreamTag::try_from(tag[0])?;
    handle_upload_stream_tagged(slot, tag, stream).await
}

/// Handle an upload stream where the tag has already been read.
pub async fn handle_upload_stream_tagged(
    slot: &Arc<IngestSlot>,
    tag: InboundStreamTag,
    stream: quinn::RecvStream,
) -> Result<()> {
    match tag {
        InboundStreamTag::Segment => handle_segment_upload(slot, stream).await,
        InboundStreamTag::Init => handle_init_upload(slot, stream).await,
        InboundStreamTag::Manifest => handle_manifest_push(slot, stream).await,
        InboundStreamTag::TelemetryBuffer => handle_telemetry_buffer(slot, stream).await,
        other => {
            tracing::warn!(
                device_id = %slot.device_id,
                tag = ?other,
                "unexpected stream tag in upload handler"
            );
            Ok(())
        }
    }
}

/// Read a segment upload stream. The stream format is:
/// [segment_id_len: u16 BE] [segment_id: UTF-8] [segment_data: rest of stream]
async fn handle_segment_upload(
    slot: &Arc<IngestSlot>,
    mut stream: quinn::RecvStream,
) -> Result<()> {
    // Read segment ID (length-prefixed u16)
    let mut id_len_buf = [0u8; 2];
    stream.read_exact(&mut id_len_buf).await?;
    let id_len = u16::from_be_bytes(id_len_buf) as usize;

    let mut id_buf = vec![0u8; id_len];
    stream.read_exact(&mut id_buf).await?;
    let segment_id = String::from_utf8(id_buf)?;

    // Read the rest as segment data
    let data = stream.read_to_end(MAX_FRAME_SIZE as usize).await?;

    tracing::info!(
        device_id = %slot.device_id,
        segment_id,
        size = data.len(),
        "segment upload received"
    );

    complete_segment_upload(slot, &segment_id, Bytes::from(data)).await;
    Ok(())
}

/// Read the init segment upload and store it.
async fn handle_init_upload(slot: &Arc<IngestSlot>, mut stream: quinn::RecvStream) -> Result<()> {
    let data = stream.read_to_end(MAX_FRAME_SIZE as usize).await?;

    tracing::info!(
        device_id = %slot.device_id,
        size = data.len(),
        "init segment upload received"
    );

    let _ = slot.init_segment.send(Some(Bytes::from(data)));
    Ok(())
}

/// Read the full stream and replace the slot's in-memory manifest.
async fn handle_manifest_push(slot: &Arc<IngestSlot>, mut stream: quinn::RecvStream) -> Result<()> {
    let data = stream.read_to_end(MAX_FRAME_SIZE as usize).await?;
    let manifest = String::from_utf8(data)?;

    tracing::info!(
        device_id = %slot.device_id,
        size = manifest.len(),
        "manifest push received"
    );

    *slot.manifest.write().await = Some(manifest.clone());

    if let Some(ref redis) = slot.redis {
        crate::redis::manifest::store_manifest(redis, &slot.device_id, &manifest).await;
    }

    Ok(())
}

/// Read the full telemetry buffer, decode as MessagePack array.
async fn handle_telemetry_buffer(
    slot: &Arc<IngestSlot>,
    mut stream: quinn::RecvStream,
) -> Result<()> {
    let data = stream.read_to_end(MAX_FRAME_SIZE as usize).await?;
    let entries: Vec<TelemetryDatagram> = rmp_serde::from_slice(&data)?;

    tracing::info!(
        device_id = %slot.device_id,
        count = entries.len(),
        "telemetry buffer upload received"
    );

    if let Some(ref redis) = slot.redis {
        crate::redis::telemetry::write_telemetry_batch(redis, &slot.device_id, &entries).await;
    }

    Ok(())
}
