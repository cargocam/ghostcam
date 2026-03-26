use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, AtomicUsize};
use std::sync::Arc;

use anyhow::Result;
use bytes::Bytes;
use ghostcam::config::BROADCAST_CAPACITY;
use ghostcam::telemetry::TelemetryDatagram;
use ghostcam::types::{DeviceId, UserId};
use ghostcam::wire::alert::StreamKind;
use ghostcam::wire::command::Command;
use ghostcam::wire::framing;
use tokio::sync::{broadcast, mpsc, oneshot, Notify, RwLock};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use crate::frames::{AudioFrame, InboundStreamTag, VideoFrame};
use crate::redis::connection::RedisManager;
use crate::redis::telemetry::TelemetryBatcher;

/// State of a segment upload for request coalescing.
#[derive(Debug)]
pub enum SegmentState {
    /// Upload is in progress. Waiters will be notified on completion.
    Uploading {
        waiters: Vec<oneshot::Sender<Result<Bytes>>>,
    },
    /// Upload complete. Data buffered for a TTL period.
    Buffered { data: Bytes },
}

/// Represents a connected camera's server-side state.
pub struct IngestSlot {
    /// Camera identity
    pub device_id: DeviceId,
    pub user_id: UserId,

    /// Current camera capabilities (which streams are active)
    pub capabilities: Arc<RwLock<Vec<StreamKind>>>,

    /// Broadcast channels — always present, silent when no data
    pub video_tx: broadcast::Sender<VideoFrame>,
    pub audio_tx: broadcast::Sender<AudioFrame>,
    pub telemetry_tx: broadcast::Sender<TelemetryDatagram>,

    /// Pre-normalized manifest for browser serving (avoids per-request processing).
    pub manifest_normalized: Arc<RwLock<Option<String>>>,

    /// Latest init segment (pushed by camera)
    pub init_segment: Arc<RwLock<Option<Bytes>>>,
    /// Notified when init_segment is set (replaces polling in HLS handler).
    pub init_notify: Arc<Notify>,

    /// Send commands to the camera
    pub commands_tx: mpsc::Sender<Command>,

    /// Live subscriber demand counters
    pub video_subscribers: Arc<AtomicUsize>,
    pub audio_subscribers: Arc<AtomicUsize>,

    /// Monotonically increasing command sequence number
    seq: Arc<AtomicU64>,

    /// In-flight segment uploads for request coalescing
    pub segments: Arc<RwLock<HashMap<String, SegmentState>>>,

    /// Redis manager for telemetry persistence (None in tests without Redis)
    pub redis: Option<Arc<RedisManager>>,

    /// Telemetry batcher for batched Redis writes (None in tests without Redis).
    /// Stored here for lifetime management; the batcher Arc is passed to telemetry_reader.
    #[allow(dead_code)]
    pub telemetry_batcher: Option<Arc<TelemetryBatcher>>,

    /// Cancellation token for coordinated teardown
    cancel: CancellationToken,
}

impl IngestSlot {
    /// Create a new slot and spawn all read/write loop tasks.
    /// Returns the slot (wrapped in Arc) and a JoinHandle for the supervisor task.
    ///
    /// The slot accepts the alerts stream (already established during handshake)
    /// and the commands stream. Video and audio streams are accepted dynamically
    /// by the stream acceptor task, since quinn only creates QUIC streams at the
    /// transport layer when data is written to them.
    pub async fn spawn(
        device_id: DeviceId,
        user_id: UserId,
        connection: quinn::Connection,
        alerts_stream: quinn::RecvStream,
        commands_stream: quinn::SendStream,
        redis: Option<Arc<RedisManager>>,
        telemetry_batcher: Option<Arc<TelemetryBatcher>>,
    ) -> (Arc<Self>, JoinHandle<()>) {
        let (video_tx, _) = broadcast::channel(BROADCAST_CAPACITY);
        let (audio_tx, _) = broadcast::channel(BROADCAST_CAPACITY);
        let (telemetry_tx, _) = broadcast::channel(BROADCAST_CAPACITY);
        let (commands_tx, commands_rx) = mpsc::channel(64);

        let cancel = CancellationToken::new();

        let slot = Arc::new(Self {
            device_id: device_id.clone(),
            user_id,
            capabilities: Arc::new(RwLock::new(Vec::new())),
            video_tx: video_tx.clone(),
            audio_tx: audio_tx.clone(),
            telemetry_tx: telemetry_tx.clone(),
            manifest_normalized: Arc::new(RwLock::new(None)),
            init_segment: Arc::new(RwLock::new(None)),
            init_notify: Arc::new(Notify::new()),
            commands_tx,
            video_subscribers: Arc::new(AtomicUsize::new(0)),
            audio_subscribers: Arc::new(AtomicUsize::new(0)),
            seq: Arc::new(AtomicU64::new(1)),
            segments: Arc::new(RwLock::new(HashMap::new())),
            redis: redis.clone(),
            telemetry_batcher: telemetry_batcher.clone(),
            cancel: cancel.clone(),
        });

        let slot_clone = slot.clone();
        let conn = connection.clone();
        let supervisor = tokio::spawn(async move {
            let cancel = cancel.clone();

            // Spawn read/write tasks
            let alert_task = tokio::spawn(Self::alert_reader(
                slot_clone.clone(),
                alerts_stream,
                cancel.clone(),
            ));
            let telemetry_task = tokio::spawn(Self::telemetry_reader(
                slot_clone.device_id.clone(),
                telemetry_tx,
                conn.clone(),
                telemetry_batcher,
                cancel.clone(),
            ));
            let command_task = tokio::spawn(Self::command_writer(
                commands_rx,
                commands_stream,
                cancel.clone(),
            ));
            // Stream acceptor handles Video, Audio, and upload streams dynamically
            let stream_task = tokio::spawn(Self::stream_acceptor(
                slot_clone.clone(),
                video_tx,
                audio_tx,
                conn.clone(),
                cancel.clone(),
            ));

            // Wait for any task to complete or cancellation
            tokio::select! {
                _ = cancel.cancelled() => {}
                r = alert_task => { tracing::debug!(device_id = %slot_clone.device_id, "alert reader finished: {:?}", r); }
                r = telemetry_task => { tracing::debug!(device_id = %slot_clone.device_id, "telemetry reader finished: {:?}", r); }
                r = command_task => { tracing::debug!(device_id = %slot_clone.device_id, "command writer finished: {:?}", r); }
                r = stream_task => { tracing::debug!(device_id = %slot_clone.device_id, "stream acceptor finished: {:?}", r); }
            }

            // Cancel all remaining tasks
            cancel.cancel();
            conn.close(0u32.into(), b"slot shutdown");
        });

        (slot, supervisor)
    }

    /// Allocate the next command sequence number.
    pub fn next_seq(&self) -> u64 {
        self.seq.fetch_add(1, std::sync::atomic::Ordering::SeqCst)
    }

    /// Send a command to the camera.
    pub async fn send_command(&self, command: Command) -> Result<()> {
        self.commands_tx
            .send(command)
            .await
            .map_err(|_| anyhow::anyhow!("command channel closed"))?;
        Ok(())
    }

    /// Shut down the slot: cancel all tasks, close the QUIC connection.
    pub fn shutdown(&self) {
        self.cancel.cancel();
    }

    /// Returns true if this slot has been shut down.
    #[allow(dead_code)]
    pub fn is_shutdown(&self) -> bool {
        self.cancel.is_cancelled()
    }

    // --- Internal task implementations ---

    async fn alert_reader(
        slot: Arc<IngestSlot>,
        mut stream: quinn::RecvStream,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                result = framing::read_json::<ghostcam::wire::alert::Alert, _>(&mut stream) => {
                    match result {
                        Ok(Some(alert)) => {
                            if let Err(e) = alert.validate() {
                                tracing::warn!(device_id = %slot.device_id, "alert validation failed: {e}");
                                continue;
                            }
                            super::alerts::handle_alert(&slot, alert).await;
                        }
                        Ok(None) => break, // Clean EOF
                        Err(e) => {
                            tracing::warn!(device_id = %slot.device_id, "alert read error: {e}");
                            break;
                        }
                    }
                }
            }
        }
    }

    async fn video_reader(
        tx: broadcast::Sender<VideoFrame>,
        mut stream: quinn::RecvStream,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                result = framing::read_frame(&mut stream) => {
                    match result {
                        Ok(Some(data)) => {
                            // Bytes::from(Vec<u8>) is zero-copy: it takes ownership of the
                            // Vec's heap allocation without copying the buffer contents.
                            let _ = tx.send(VideoFrame { data: Bytes::from(data) });
                        }
                        Ok(None) => break,
                        Err(e) => {
                            tracing::warn!("video read error: {e}");
                            break;
                        }
                    }
                }
            }
        }
    }

    async fn audio_reader(
        tx: broadcast::Sender<AudioFrame>,
        mut stream: quinn::RecvStream,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                result = framing::read_frame(&mut stream) => {
                    match result {
                        Ok(Some(data)) => {
                            // Bytes::from(Vec<u8>) is zero-copy: see video_reader comment.
                            let _ = tx.send(AudioFrame { data: Bytes::from(data) });
                        }
                        Ok(None) => break,
                        Err(e) => {
                            tracing::warn!("audio read error: {e}");
                            break;
                        }
                    }
                }
            }
        }
    }

    async fn telemetry_reader(
        device_id: DeviceId,
        tx: broadcast::Sender<TelemetryDatagram>,
        connection: quinn::Connection,
        batcher: Option<Arc<TelemetryBatcher>>,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                result = connection.read_datagram() => {
                    match result {
                        Ok(data) => {
                            match rmp_serde::from_slice::<TelemetryDatagram>(&data) {
                                Ok(datagram) => {
                                    // Broadcast to viewers immediately (low latency).
                                    // Enqueue to batcher for batched Redis persistence
                                    // (avoids per-datagram tokio::spawn + Redis round-trip).
                                    if let Some(ref batcher) = batcher {
                                        batcher.send(device_id.clone(), datagram.clone());
                                    }
                                    let _ = tx.send(datagram);
                                }
                                Err(e) => {
                                    tracing::warn!("telemetry decode error: {e}");
                                }
                            }
                        }
                        Err(e) => {
                            tracing::debug!("telemetry datagram read ended: {e}");
                            break;
                        }
                    }
                }
            }
        }
    }

    async fn command_writer(
        mut rx: mpsc::Receiver<Command>,
        mut stream: quinn::SendStream,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                cmd = rx.recv() => {
                    match cmd {
                        Some(command) => {
                            if let Err(e) = framing::write_json(&mut stream, &command).await {
                                tracing::warn!("command write error: {e}");
                                break;
                            }
                        }
                        None => break, // Channel closed
                    }
                }
            }
        }
    }

    /// Accept all inbound unidirectional streams from the camera.
    /// Reads the 1-byte stream tag and dispatches:
    /// - Video/Audio: spawn a persistent reader task
    /// - Segment/Init/Manifest/TelemetryBuffer: handle as one-shot upload
    async fn stream_acceptor(
        slot: Arc<IngestSlot>,
        video_tx: broadcast::Sender<VideoFrame>,
        audio_tx: broadcast::Sender<AudioFrame>,
        connection: quinn::Connection,
        cancel: CancellationToken,
    ) {
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                result = connection.accept_uni() => {
                    match result {
                        Ok(mut stream) => {
                            // Read the 1-byte stream tag
                            let mut tag_buf = [0u8; 1];
                            if let Err(e) = stream.read_exact(&mut tag_buf).await {
                                tracing::warn!(device_id = %slot.device_id, "stream tag read error: {e}");
                                continue;
                            }

                            let tag = match InboundStreamTag::try_from(tag_buf[0]) {
                                Ok(t) => t,
                                Err(e) => {
                                    tracing::warn!(device_id = %slot.device_id, "unknown stream tag: {e}");
                                    continue;
                                }
                            };

                            match tag {
                                InboundStreamTag::Video => {
                                    let tx = video_tx.clone();
                                    let c = cancel.clone();
                                    tracing::debug!(device_id = %slot.device_id, "video stream connected");
                                    tokio::spawn(Self::video_reader(tx, stream, c));
                                }
                                InboundStreamTag::Audio => {
                                    let tx = audio_tx.clone();
                                    let c = cancel.clone();
                                    tracing::debug!(device_id = %slot.device_id, "audio stream connected");
                                    tokio::spawn(Self::audio_reader(tx, stream, c));
                                }
                                InboundStreamTag::Alerts => {
                                    // Additional alerts streams shouldn't happen, but handle gracefully
                                    tracing::warn!(device_id = %slot.device_id, "unexpected additional alerts stream");
                                }
                                _ => {
                                    // Upload streams (Segment, Init, Manifest, TelemetryBuffer)
                                    let slot = slot.clone();
                                    tokio::spawn(async move {
                                        if let Err(e) = super::uploads::handle_upload_stream_tagged(&slot, tag, stream).await {
                                            tracing::warn!(device_id = %slot.device_id, "upload stream error: {e}");
                                        }
                                    });
                                }
                            }
                        }
                        Err(e) => {
                            tracing::debug!("stream accept ended: {e}");
                            break;
                        }
                    }
                }
            }
        }
    }
}

/// Create a dummy IngestSlot for testing purposes (no QUIC connection).
#[cfg(test)]
pub fn test_slot(device_id: &str, user_id: &str) -> Arc<IngestSlot> {
    let (video_tx, _) = broadcast::channel(16);
    let (audio_tx, _) = broadcast::channel(16);
    let (telemetry_tx, _) = broadcast::channel(16);
    let (commands_tx, _commands_rx) = mpsc::channel(64);

    Arc::new(IngestSlot {
        device_id: DeviceId(device_id.to_string()),
        user_id: UserId(user_id.to_string()),
        capabilities: Arc::new(RwLock::new(Vec::new())),
        video_tx,
        audio_tx,
        telemetry_tx,
        manifest_normalized: Arc::new(RwLock::new(None)),
        init_segment: Arc::new(RwLock::new(None)),
        init_notify: Arc::new(Notify::new()),
        commands_tx,
        video_subscribers: Arc::new(AtomicUsize::new(0)),
        audio_subscribers: Arc::new(AtomicUsize::new(0)),
        seq: Arc::new(AtomicU64::new(1)),
        segments: Arc::new(RwLock::new(HashMap::new())),
        redis: None,
        telemetry_batcher: None,
        cancel: CancellationToken::new(),
    })
}

/// Create a dummy IngestSlot with access to the command receiver for testing.
#[cfg(test)]
pub fn test_slot_with_commands(
    device_id: &str,
    user_id: &str,
) -> (Arc<IngestSlot>, mpsc::Receiver<Command>) {
    let (video_tx, _) = broadcast::channel(16);
    let (audio_tx, _) = broadcast::channel(16);
    let (telemetry_tx, _) = broadcast::channel(16);
    let (commands_tx, commands_rx) = mpsc::channel(64);

    let slot = Arc::new(IngestSlot {
        device_id: DeviceId(device_id.to_string()),
        user_id: UserId(user_id.to_string()),
        capabilities: Arc::new(RwLock::new(Vec::new())),
        video_tx,
        audio_tx,
        telemetry_tx,
        manifest_normalized: Arc::new(RwLock::new(None)),
        init_segment: Arc::new(RwLock::new(None)),
        init_notify: Arc::new(Notify::new()),
        commands_tx,
        video_subscribers: Arc::new(AtomicUsize::new(0)),
        audio_subscribers: Arc::new(AtomicUsize::new(0)),
        seq: Arc::new(AtomicU64::new(1)),
        segments: Arc::new(RwLock::new(HashMap::new())),
        redis: None,
        telemetry_batcher: None,
        cancel: CancellationToken::new(),
    });

    (slot, commands_rx)
}
