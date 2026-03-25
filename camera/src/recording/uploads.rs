use anyhow::Result;
use ghostcam::wire::alert::Alert;
use ghostcam::wire::frames::InboundStreamTag;
use ghostcam::wire::framing;
use tokio::sync::{mpsc, Mutex, RwLock};

use super::ring_buffer::RingBuffer;

/// Run the upload handler loop.
pub async fn run_upload_handler(
    connection: quinn::Connection,
    mut cmd_rx: mpsc::Receiver<UploadCommand>,
    ring_buffer: &RwLock<RingBuffer>,
    init_segment: &RwLock<Option<bytes::Bytes>>,
    alerts_tx: &Mutex<quinn::SendStream>,
    cancel: tokio_util::sync::CancellationToken,
) -> Result<()> {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => return Ok(()),
            cmd = cmd_rx.recv() => {
                let Some(cmd) = cmd else { return Ok(()) };
                match cmd {
                    UploadCommand::Segment { seq, segment_id } => {
                        handle_upload_segment(
                            &connection, ring_buffer, alerts_tx, seq, &segment_id,
                        ).await;
                    }
                    UploadCommand::Init { seq } => {
                        handle_upload_init(&connection, init_segment, seq).await;
                    }
                }
            }
        }
    }
}

/// Upload commands forwarded from the command handler.
#[derive(Debug)]
pub enum UploadCommand {
    Segment { seq: u64, segment_id: String },
    Init { seq: u64 },
}

async fn handle_upload_segment(
    connection: &quinn::Connection,
    ring_buffer: &RwLock<RingBuffer>,
    alerts_tx: &Mutex<quinn::SendStream>,
    seq: u64,
    segment_id: &str,
) {
    let path = {
        let rb = ring_buffer.read().await;
        rb.get_segment_path(segment_id).map(|p| p.to_path_buf())
    };

    let Some(path) = path else {
        // Segment not found (evicted)
        let alert = Alert::SegmentUploadFailed {
            seq,
            segment_id: segment_id.to_string(),
            reason: ghostcam::wire::alert::UploadFailReason::Evicted,
        };
        let mut stream = alerts_tx.lock().await;
        let _ = framing::write_json(&mut *stream, &alert).await;
        return;
    };

    match tokio::fs::read(&path).await {
        Ok(data) => {
            match connection.open_uni().await {
                Ok(mut stream) => {
                    // Wire format: [tag] [segment_id_len: u16 BE] [segment_id] [data]
                    let _ = stream.write_all(&[InboundStreamTag::Segment as u8]).await;
                    let id_bytes = segment_id.as_bytes();
                    let _ = stream
                        .write_all(&(id_bytes.len() as u16).to_be_bytes())
                        .await;
                    let _ = stream.write_all(id_bytes).await;
                    let _ = stream.write_all(&data).await;
                    let _ = stream.finish();

                    let alert = Alert::SegmentUploaded {
                        seq,
                        segment_id: segment_id.to_string(),
                    };
                    let mut a = alerts_tx.lock().await;
                    let _ = framing::write_json(&mut *a, &alert).await;
                }
                Err(e) => {
                    tracing::warn!("failed to open upload stream: {e}");
                }
            }
        }
        Err(e) => {
            tracing::warn!("failed to read segment file: {e}");
            let alert = Alert::SegmentUploadFailed {
                seq,
                segment_id: segment_id.to_string(),
                reason: ghostcam::wire::alert::UploadFailReason::IoError,
            };
            let mut stream = alerts_tx.lock().await;
            let _ = framing::write_json(&mut *stream, &alert).await;
        }
    }
}

async fn handle_upload_init(
    connection: &quinn::Connection,
    init_segment: &RwLock<Option<bytes::Bytes>>,
    _seq: u64,
) {
    let data = init_segment.read().await.clone();
    let Some(data) = data else {
        tracing::warn!("no init segment available for upload");
        return;
    };

    match connection.open_uni().await {
        Ok(mut stream) => {
            let _ = stream.write_all(&[InboundStreamTag::Init as u8]).await;
            let _ = stream.write_all(&data).await;
            let _ = stream.finish();
        }
        Err(e) => {
            tracing::warn!("failed to open init upload stream: {e}");
        }
    }
}
