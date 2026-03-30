use std::path::PathBuf;
use std::sync::atomic::AtomicBool;
use std::sync::Arc;

use anyhow::{Context, Result};
use bytes::Bytes;
use ghostcam::wire::alert::{Alert, StreamKind};
use ghostcam::wire::frames::InboundStreamTag;
use ghostcam::wire::framing;
use tokio::sync::{broadcast, mpsc, Mutex, RwLock};
use tokio_util::sync::CancellationToken;

use crate::capture::CaptureMessage;
use crate::commands::CommandSignal;
use crate::recording::ring_buffer::RingBuffer;
use crate::recording::uploads::UploadCommand;
use crate::recording::SegmentEvent;
use crate::telemetry::buffer::TelemetryBuffer;

/// A single QUIC connection session.
pub struct Session {
    connection: quinn::Connection,
    /// Alerts send stream (shared for ack writes from command handler)
    alerts_tx: Arc<Mutex<quinn::SendStream>>,
    /// Commands receive stream
    commands_rx: quinn::RecvStream,
    /// Video streaming enabled
    video_enabled: Arc<AtomicBool>,
    /// Audio streaming enabled
    audio_enabled: Arc<AtomicBool>,
    /// Cancellation
    cancel: CancellationToken,
    /// Data directory for cert storage etc
    data_dir: PathBuf,
    /// Directory for fMP4 recording segments
    segment_dir: PathBuf,
    /// Device fingerprint (used as stable identity for segments)
    device_fingerprint: String,
}

impl Session {
    /// Establish a session:
    /// 1. Open bidirectional control stream (Alerts send + Commands recv)
    /// 2. Send Alerts stream tag
    /// 3. Send handshake alert
    /// 4. Upload telemetry buffer if non-empty
    pub async fn establish(
        connection: quinn::Connection,
        telemetry_buffer: &TelemetryBuffer,
        cancel: CancellationToken,
        data_dir: PathBuf,
        segment_dir: PathBuf,
        device_fingerprint: String,
    ) -> Result<Self> {
        // 1. Open bidirectional stream for control
        let (mut alerts_send, commands_recv) = connection
            .open_bi()
            .await
            .map_err(|e| anyhow::anyhow!("failed to open control stream: {e}"))?;

        // 2. Write stream tag
        alerts_send
            .write_all(&[InboundStreamTag::Alerts as u8])
            .await?;

        // 3. Send handshake alert
        let handshake = Alert::Handshake {
            protocol_version: ghostcam::config::PROTOCOL_VERSION,
            fw_version: env!("CARGO_PKG_VERSION").to_string(),
            streams: vec![StreamKind::Video, StreamKind::Audio, StreamKind::Telemetry],
        };
        framing::write_json(&mut alerts_send, &handshake)
            .await
            .map_err(|e| anyhow::anyhow!("handshake write error: {e}"))?;

        tracing::info!("handshake sent");

        // 4. Upload telemetry buffer if non-empty
        if !telemetry_buffer.is_empty().await {
            if let Err(e) = upload_telemetry_buffer(&connection, telemetry_buffer).await {
                tracing::warn!("telemetry buffer upload failed: {e}");
            }
        }

        Ok(Self {
            connection,
            alerts_tx: Arc::new(Mutex::new(alerts_send)),
            commands_rx: commands_recv,
            video_enabled: Arc::new(AtomicBool::new(false)),
            audio_enabled: Arc::new(AtomicBool::new(false)),
            cancel,
            data_dir,
            segment_dir,
            device_fingerprint,
        })
    }

    /// Wait for the initial DeviceStatus from the server after handshake.
    /// If Unclaimed, scans QR for a claim token, sends it, and waits for Active.
    /// Returns Ok(()) when the device is Active and ready to stream.
    pub async fn wait_for_active_status(&mut self) -> Result<()> {
        use ghostcam::wire::command::{Command, DeviceStatusKind};

        // Read the first command — should be DeviceStatus
        let cmd: Command = framing::read_json(&mut self.commands_rx)
            .await
            .map_err(|e| anyhow::anyhow!("failed to read DeviceStatus: {e}"))?
            .ok_or_else(|| anyhow::anyhow!("command stream closed before DeviceStatus"))?;

        match cmd {
            Command::DeviceStatus {
                status: DeviceStatusKind::Active,
                ..
            } => {
                tracing::info!("device status: active — ready to stream");
                return Ok(());
            }
            Command::DeviceStatus {
                status: DeviceStatusKind::Unclaimed,
                ..
            } => {
                tracing::info!("device status: unclaimed — entering QR claim mode");
            }
            Command::DeviceStatus {
                status: DeviceStatusKind::Suspended,
                ..
            } => {
                anyhow::bail!("device is suspended");
            }
            other => {
                anyhow::bail!("expected DeviceStatus, got: {:?}", other);
            }
        }

        // We're unclaimed — try to get a claim token from:
        // 1. A claim_token file in the data dir (Docker auto-claim)
        // 2. QR code scanning (real hardware)
        let token = self.get_claim_token().await?;

        // Send ClaimToken alert
        crate::enrollment::send_claim_token(&self.alerts_tx, &token).await?;

        // Wait for Active status
        let cmd: Command = framing::read_json(&mut self.commands_rx)
            .await
            .map_err(|e| anyhow::anyhow!("failed to read status after claim: {e}"))?
            .ok_or_else(|| anyhow::anyhow!("stream closed after claim"))?;

        match cmd {
            Command::DeviceStatus {
                status: DeviceStatusKind::Active,
                ..
            } => {
                tracing::info!("device claimed successfully — ready to stream");
                // Clean up claim token file if it existed
                let _ = tokio::fs::remove_file(self.data_dir.join("claim_token")).await;
                Ok(())
            }
            other => {
                anyhow::bail!("expected Active after claim, got: {:?}", other);
            }
        }
    }

    /// Try to get a claim token from a file (Docker) or QR scan (hardware).
    async fn get_claim_token(&self) -> Result<String> {
        // Check for a pre-provisioned claim token file (Docker auto-claim)
        let token_file = self.data_dir.join("claim_token");
        if let Ok(token) = tokio::fs::read_to_string(&token_file).await {
            let token = token.trim().to_string();
            if !token.is_empty() {
                tracing::info!("claim token loaded from file");
                return Ok(token);
            }
        }

        // No file — scan QR code (real hardware, Linux only)
        #[cfg(target_os = "linux")]
        {
            tracing::info!("scanning for claim QR code...");
            let qr_result = crate::qr_enrollment::scan_for_claim_token().await?;
            match qr_result {
                crate::qr_enrollment::QrResult::ClaimToken { token, server_addr } => {
                    if let Some(addr) = server_addr {
                        tracing::info!(server_addr = %addr, "QR code contains server address (will use on next connect)");
                    }
                    Ok(token)
                }
                crate::qr_enrollment::QrResult::Wifi {
                    claim_token,
                    server_addr,
                    ..
                } => {
                    if let Some(addr) = server_addr {
                        tracing::info!(server_addr = %addr, "QR code contains server address (will use on next connect)");
                    }
                    claim_token.ok_or_else(|| {
                        anyhow::anyhow!("WiFi QR scanned but no claim token — reconnect to retry")
                    })
                }
            }
        }

        #[cfg(not(target_os = "linux"))]
        {
            anyhow::bail!(
                "device is unclaimed — place a claim_token file in {} or scan QR on Linux",
                self.data_dir.display()
            );
        }
    }

    /// Run the session. Spawns concurrent tasks and waits for any to fail.
    /// Returns Some(CommandSignal) if a signal was received from the command handler.
    pub async fn run(
        mut self,
        mut video_rx: mpsc::Receiver<CaptureMessage>,
        mut audio_rx: mpsc::Receiver<CaptureMessage>,
    ) -> Result<Option<CommandSignal>> {
        // Open Video unidirectional stream
        let mut video_stream = self.connection.open_uni().await?;
        video_stream
            .write_all(&[InboundStreamTag::Video as u8])
            .await?;

        // Open Audio unidirectional stream
        let mut audio_stream = self.connection.open_uni().await?;
        audio_stream
            .write_all(&[InboundStreamTag::Audio as u8])
            .await?;

        let cancel = self.cancel.clone();
        let (signal_tx, mut signal_rx) = mpsc::channel::<CommandSignal>(1);

        // --- Recording pipeline setup ---
        let segment_dir = self.segment_dir.clone();
        tokio::fs::create_dir_all(&segment_dir).await?;

        let (seg_event_tx, mut seg_event_rx) = mpsc::channel::<SegmentEvent>(64);
        let ring_buffer = RingBuffer::scan(&segment_dir, seg_event_tx.clone()).await?;
        let ring_buffer = Arc::new(RwLock::new(ring_buffer));
        let init_segment: Arc<RwLock<Option<Bytes>>> = Arc::new(RwLock::new(None));

        // Broadcast channels: video NALs and audio frames are sent to both
        // the QUIC stream writers and the local muxer.
        let (video_broadcast_tx, _) = broadcast::channel::<Bytes>(512);
        let (audio_broadcast_tx, _) = broadcast::channel::<Bytes>(512);

        let muxer_video_rx = video_broadcast_tx.subscribe();
        let muxer_audio_rx = audio_broadcast_tx.subscribe();

        // Upload command channel
        let (upload_tx, upload_rx) = mpsc::channel::<UploadCommand>(64);

        // Muxer task — writes fMP4 segments to disk (shares ring buffer with upload handler)
        let device_id = self.device_fingerprint.clone();
        let mut muxer = crate::recording::muxer::Muxer::new(
            segment_dir.clone(),
            device_id,
            seg_event_tx.clone(),
            ring_buffer.clone(),
        );
        let muxer_cancel = cancel.clone();
        let muxer_task = tokio::spawn(async move {
            if let Err(e) = muxer
                .run(muxer_video_rx, muxer_audio_rx, muxer_cancel)
                .await
            {
                tracing::warn!("muxer ended: {e}");
            }
        });

        // Segment event handler — pushes manifests and alerts to server
        let evt_conn = self.connection.clone();
        let evt_alerts = self.alerts_tx.clone();
        let evt_init = init_segment.clone();
        let evt_cancel = cancel.clone();
        let event_handler_task = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = evt_cancel.cancelled() => break,
                    event = seg_event_rx.recv() => {
                        let Some(event) = event else { break };
                        match event {
                            SegmentEvent::ManifestUpdated { manifest } => {
                                if let Err(e) = crate::recording::manifest_push::push_manifest(
                                    &evt_conn, &manifest,
                                ).await {
                                    tracing::warn!("manifest push failed: {e}");
                                }
                            }
                            SegmentEvent::InitReady { data } => {
                                *evt_init.write().await = Some(data);
                            }
                            SegmentEvent::Finalized { segment_id, start_ts, end_ts, size_bytes } => {
                                let alert = Alert::RecordingSegment {
                                    device_id: String::new(), // server fills from slot
                                    segment_id,
                                    start_ts,
                                    end_ts,
                                    size_bytes,
                                };
                                let mut stream = evt_alerts.lock().await;
                                let _ = framing::write_json(&mut *stream, &alert).await;
                            }
                        }
                    }
                }
            }
        });

        // Upload handler task — responds to server UploadSegment/UploadInit commands
        let upl_conn = self.connection.clone();
        let upl_rb = ring_buffer.clone();
        let upl_init = init_segment.clone();
        let upl_alerts = self.alerts_tx.clone();
        let upl_cancel = cancel.clone();
        let upload_task = tokio::spawn(async move {
            if let Err(e) = crate::recording::uploads::run_upload_handler(
                upl_conn,
                upload_rx,
                &upl_rb,
                &upl_init,
                &upl_alerts,
                upl_cancel,
            )
            .await
            {
                tracing::warn!("upload handler ended: {e}");
            }
        });

        // Command reader task — now with upload_tx connected
        let cmd_video = self.video_enabled.clone();
        let cmd_audio = self.audio_enabled.clone();
        let cmd_alerts = self.alerts_tx.clone();
        let cmd_cancel = cancel.clone();
        let cmd_data_dir = self.data_dir.clone();
        let command_task = tokio::spawn(async move {
            let result = crate::commands::run_command_reader(
                &mut self.commands_rx,
                cmd_video,
                cmd_audio,
                &cmd_alerts,
                Some(upload_tx),
                cmd_data_dir,
                signal_tx,
            )
            .await;
            cmd_cancel.cancel();
            result
        });

        // Video writer task — reads from mpsc, fans out to broadcast + QUIC
        let vid_enabled = self.video_enabled.clone();
        let vid_cancel = cancel.clone();
        let vid_broadcast = video_broadcast_tx;
        let video_task = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = vid_cancel.cancelled() => return Ok::<_, anyhow::Error>(()),
                    msg = video_rx.recv() => {
                        match msg {
                            Some(CaptureMessage::VideoNal(data)) => {
                                // Always send to muxer (local recording)
                                let _ = vid_broadcast.send(data.clone());
                                // Send to server only if enabled
                                if vid_enabled.load(std::sync::atomic::Ordering::SeqCst) {
                                    framing::write_frame(&mut video_stream, &data)
                                        .await
                                        .map_err(|e| anyhow::anyhow!("video write error: {e}"))?;
                                }
                            }
                            Some(_) => {}
                            None => return Ok(()),
                        }
                    }
                }
            }
        });

        // Audio writer task — reads from mpsc, fans out to broadcast + QUIC
        let aud_enabled = self.audio_enabled.clone();
        let aud_cancel = cancel.clone();
        let aud_broadcast = audio_broadcast_tx;
        let audio_task = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = aud_cancel.cancelled() => return Ok::<_, anyhow::Error>(()),
                    msg = audio_rx.recv() => {
                        match msg {
                            Some(CaptureMessage::AudioFrame(data)) => {
                                let _ = aud_broadcast.send(data.clone());
                                if aud_enabled.load(std::sync::atomic::Ordering::SeqCst) {
                                    framing::write_frame(&mut audio_stream, &data)
                                        .await
                                        .map_err(|e| anyhow::anyhow!("audio write error: {e}"))?;
                                }
                            }
                            Some(_) => {}
                            None => return Ok(()),
                        }
                    }
                }
            }
        });

        // Wait for any task to finish or a signal
        let mut signal = None;
        tokio::select! {
            r = command_task => {
                tracing::debug!("command reader ended: {:?}", r);
            }
            r = video_task => {
                tracing::debug!("video writer ended: {:?}", r);
            }
            r = audio_task => {
                tracing::debug!("audio writer ended: {:?}", r);
            }
            _ = muxer_task => {
                tracing::debug!("muxer ended");
            }
            _ = event_handler_task => {
                tracing::debug!("event handler ended");
            }
            _ = upload_task => {
                tracing::debug!("upload handler ended");
            }
            s = signal_rx.recv() => {
                signal = s;
                tracing::debug!("command signal received: {:?}", signal);
            }
            _ = cancel.cancelled() => {
                tracing::debug!("session cancelled");
            }
        }

        Ok(signal)
    }
}

/// Upload buffered telemetry to the server via a one-shot upload stream.
async fn upload_telemetry_buffer(
    connection: &quinn::Connection,
    buffer: &TelemetryBuffer,
) -> Result<()> {
    let entries = buffer.drain().await;
    if entries.is_empty() {
        return Ok(());
    }

    let encoded = ghostcam::telemetry::TelemetryDatagram::encode_batch(&entries)
        .context("encoding telemetry batch")?;

    let mut stream = connection.open_uni().await?;
    stream
        .write_all(&[InboundStreamTag::TelemetryBuffer as u8])
        .await?;
    stream.write_all(&encoded).await?;
    stream.finish()?;

    buffer.clear_disk().await?;
    tracing::info!(count = entries.len(), "uploaded telemetry buffer");
    Ok(())
}
