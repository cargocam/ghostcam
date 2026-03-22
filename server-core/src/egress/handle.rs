use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use ghostcam::telemetry::TelemetryDatagram;
use ghostcam::types::DeviceId;
use str0m::change::{SdpAnswer, SdpOffer};
use str0m::channel::ChannelId;
use str0m::media::{Frequency, MediaTime, Mid};
use str0m::net::{Protocol, Receive};
use str0m::{Candidate, Event, Input, Output, Rtc};
use tokio::net::UdpSocket;
use tokio::sync::broadcast;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use super::data_channel::ClientMessage;
use super::rtp::NalAccumulator;
use crate::frames::{AudioFrame, VideoFrame};
use crate::ingest::demand::{update_subscriber_demand, ClientMode};
use crate::ingest::slot::IngestSlot;

/// One EgressHandle per observer×camera pair.
pub struct EgressHandle {
    /// Session identifier
    pub session_id: String,
    /// Which camera this handle watches
    pub device_id: DeviceId,
    /// The str0m Rtc instance
    rtc: Rtc,
    /// Video RTP track mid
    video_mid: Mid,
    /// Audio RTP track mid
    audio_mid: Mid,
    /// Telemetry data channel
    telemetry_channel: ChannelId,
    /// Broadcast receivers from the IngestSlot
    video_rx: broadcast::Receiver<VideoFrame>,
    audio_rx: broadcast::Receiver<AudioFrame>,
    telemetry_rx: broadcast::Receiver<TelemetryDatagram>,
    /// Reference to the slot for sending commands
    slot: Arc<IngestSlot>,
    /// Current client mode
    client_mode: ClientMode,
    /// UDP socket for WebRTC
    udp_socket: UdpSocket,
    /// The advertised local candidate address (for Receive destination matching)
    local_candidate_addr: SocketAddr,
    /// NAL accumulator for building access units
    nal_acc: NalAccumulator,
    /// RTP base time (for monotonic timestamps)
    rtp_start: std::time::Instant,
    /// Cancellation
    cancel: CancellationToken,
}

impl EgressHandle {
    /// Create a new egress handle from an SDP offer.
    ///
    /// Returns (handle, sdp_answer_string).
    pub async fn create(
        session_id: String,
        slot: &Arc<IngestSlot>,
        sdp_offer: &str,
        public_addr: SocketAddr,
        cancel: CancellationToken,
    ) -> Result<(Self, String)> {
        // 1. Bind UDP socket
        let udp_socket = UdpSocket::bind("0.0.0.0:0").await?;
        let local_addr = udp_socket.local_addr()?;

        // 2. Create str0m Rtc with ICE-lite, H.264 + Opus only
        let mut rtc = Rtc::builder()
            .set_ice_lite(true)
            .clear_codecs()
            .enable_h264(true)
            .enable_opus(true)
            .build();

        // 3. Add local candidate
        let candidate_addr = SocketAddr::new(public_addr.ip(), local_addr.port());
        let candidate = Candidate::host(candidate_addr, Protocol::Udp)?;
        rtc.add_local_candidate(candidate);

        // 4. Accept the browser's SDP offer — this creates media entries from the offer's
        //    m-lines, including codec negotiation. We must NOT add_media() before accept_offer()
        //    because that creates media without codecs, leaving writer() with no payload params.
        let offer = SdpOffer::from_sdp_string(sdp_offer)?;
        let answer: SdpAnswer = rtc.sdp_api().accept_offer(offer)?;
        let answer_str = answer.to_sdp_string();

        // 5. Find the video and audio mids from the answer SDP
        let (video_mid, audio_mid, telemetry_channel) =
            Self::parse_mids_from_sdp(&answer_str, &mut rtc)?;

        // 8. Subscribe to IngestSlot broadcast channels
        let video_rx = slot.video_tx.subscribe();
        let audio_rx = slot.audio_tx.subscribe();
        let telemetry_rx = slot.telemetry_tx.subscribe();

        let handle = Self {
            session_id,
            device_id: slot.device_id.clone(),
            rtc,
            video_mid,
            audio_mid,
            telemetry_channel,
            video_rx,
            audio_rx,
            telemetry_rx,
            slot: slot.clone(),
            client_mode: ClientMode::Live,
            udp_socket,
            local_candidate_addr: candidate_addr,
            nal_acc: NalAccumulator::new(),
            rtp_start: std::time::Instant::now(),
            cancel,
        };

        Ok((handle, answer_str))
    }

    /// Run the WebRTC event loop. Blocks until session ends.
    pub async fn run(mut self) -> Result<()> {
        // Start in live mode — increment subscriber counts
        let _ = update_subscriber_demand(&self.slot, None, ClientMode::Live).await;

        let mut buf = vec![0u8; 2000];
        let mut next_timeout = Instant::now() + Duration::from_secs(30);
        let mut polls_since_yield: u32 = 0;

        loop {
            // Process pending str0m output, yielding periodically to avoid starving
            // the tokio runtime when many frames arrive in bursts.
            loop {
                match self.rtc.poll_output() {
                    Ok(Output::Timeout(t)) => {
                        next_timeout = t.into();
                        break;
                    }
                    Ok(Output::Transmit(transmit)) => {
                        let dest = transmit.destination;
                        let _ = self.udp_socket.send_to(&transmit.contents, dest).await;
                        polls_since_yield += 1;
                        if polls_since_yield >= 64 {
                            polls_since_yield = 0;
                            tokio::task::yield_now().await;
                        }
                    }
                    Ok(Output::Event(event)) => {
                        self.handle_event(event).await;
                    }
                    Err(e) => {
                        tracing::warn!(session = %self.session_id, "rtc error: {e}");
                        break;
                    }
                }
            }
            polls_since_yield = 0;

            if !self.rtc.is_alive() {
                break;
            }

            tokio::select! {
                _ = self.cancel.cancelled() => break,

                _ = tokio::time::sleep_until(next_timeout) => {
                    if let Err(e) = self.rtc.handle_input(Input::Timeout(next_timeout.into())) {
                        tracing::warn!(session = %self.session_id, "timeout error: {e}");
                        break;
                    }
                }

                result = self.udp_socket.recv_from(&mut buf) => {
                    match result {
                        Ok((len, addr)) => {
                            let receive = Receive {
                                proto: Protocol::Udp,
                                source: addr,
                                destination: self.local_candidate_addr,
                                contents: buf[..len].try_into().unwrap(),
                            };
                            if let Err(e) = self.rtc.handle_input(Input::Receive(
                                std::time::Instant::now(),
                                receive,
                            )) {
                                tracing::debug!(session = %self.session_id, "receive error: {e}");
                            }
                        }
                        Err(e) => {
                            tracing::warn!(session = %self.session_id, "udp recv error: {e}");
                            break;
                        }
                    }
                }

                result = self.video_rx.recv() => {
                    match result {
                        Ok(frame) => {
                            tracing::trace!(session = %self.session_id, len = frame.data.len(), "video frame received");
                            self.send_video_frame(&frame);
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!(session = %self.session_id, device_id = %self.device_id, dropped = n, "video: viewer lagged, dropped {n} frames");
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }

                result = self.audio_rx.recv() => {
                    match result {
                        Ok(frame) => self.send_audio_frame(&frame),
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!(session = %self.session_id, device_id = %self.device_id, dropped = n, "audio: viewer lagged, dropped {n} frames");
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }

                result = self.telemetry_rx.recv() => {
                    match result {
                        Ok(datagram) => self.send_telemetry(&datagram),
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::warn!(session = %self.session_id, device_id = %self.device_id, dropped = n, "telemetry: viewer lagged, dropped {n} datagrams");
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }
            }
        }

        // Decrement subscriber counts on exit
        let _ = update_subscriber_demand(&self.slot, Some(self.client_mode), ClientMode::Map).await;

        Ok(())
    }

    fn send_video_frame(&mut self, frame: &VideoFrame) {
        if self.client_mode != ClientMode::Live {
            return;
        }

        // Parse NAL units from the frame data (Annex-B or raw NAL)
        let nals = super::rtp::parse_annex_b(&frame.data);
        if nals.is_empty() {
            // Treat as a single raw NAL
            if let Some(au) = self.nal_acc.push(&frame.data) {
                self.write_video_au(&au);
            }
        } else {
            for nal in nals {
                if let Some(au) = self.nal_acc.push(nal) {
                    self.write_video_au(&au);
                }
            }
        }
    }

    fn write_video_au(&mut self, au: &[u8]) {
        let now = std::time::Instant::now();
        let elapsed_us = self.rtp_start.elapsed().as_micros() as u64;
        let rtp_time = MediaTime::from_90khz((elapsed_us * 90 + 500) / 1000);

        let pt = self
            .rtc
            .writer(self.video_mid)
            .and_then(|w| w.payload_params().next().map(|pp| pp.pt()));
        if let Some(pt) = pt {
            if let Some(writer) = self.rtc.writer(self.video_mid) {
                match writer.write(pt, now, rtp_time, au.to_vec()) {
                    Ok(_) => {
                        tracing::trace!(session = %self.session_id, au_len = au.len(), "wrote video AU to str0m");
                    }
                    Err(e) => {
                        tracing::debug!(session = %self.session_id, "str0m write error: {e}");
                    }
                }
            } else {
                tracing::debug!(session = %self.session_id, "no writer for video mid (second call)");
            }
        } else {
            tracing::debug!(session = %self.session_id, "no payload params for video mid");
        }
    }

    fn send_audio_frame(&mut self, frame: &AudioFrame) {
        if self.client_mode != ClientMode::Live {
            return;
        }

        let now = std::time::Instant::now();
        let elapsed_us = self.rtp_start.elapsed().as_micros() as u64;
        let rtp_time = MediaTime::new(
            (elapsed_us * 48 + 500) / 1000,
            Frequency::FORTY_EIGHT_KHZ,
        );

        let pt = self
            .rtc
            .writer(self.audio_mid)
            .and_then(|w| w.payload_params().next().map(|pp| pp.pt()));
        if let Some(pt) = pt {
            if let Some(writer) = self.rtc.writer(self.audio_mid) {
                let _ = writer.write(pt, now, rtp_time, frame.data.to_vec());
            }
        }
    }

    fn send_telemetry(&mut self, datagram: &TelemetryDatagram) {
        let Ok(data) = serde_json::to_vec(datagram) else {
            return;
        };
        if let Some(mut channel) = self.rtc.channel(self.telemetry_channel) {
            let _ = channel.write(false, &data);
        }
    }

    /// Parse the SDP answer to find video/audio mids and the data channel ID.
    fn parse_mids_from_sdp(
        sdp: &str,
        rtc: &mut Rtc,
    ) -> Result<(Mid, Mid, ChannelId)> {
        let mut video_mid: Option<Mid> = None;
        let mut audio_mid: Option<Mid> = None;
        let mut current_kind: Option<&str> = None;

        for line in sdp.split("\r\n") {
            if line.starts_with("m=video") {
                current_kind = Some("video");
            } else if line.starts_with("m=audio") {
                current_kind = Some("audio");
            } else if line.starts_with("m=application") {
                current_kind = Some("application");
            } else if let Some(mid_str) = line.strip_prefix("a=mid:") {
                match current_kind {
                    Some("video") => video_mid = Some(Mid::from(mid_str)),
                    Some("audio") => audio_mid = Some(Mid::from(mid_str)),
                    _ => {}
                }
            }
        }

        let video_mid =
            video_mid.ok_or_else(|| anyhow::anyhow!("no video mid found in SDP answer"))?;
        let audio_mid =
            audio_mid.ok_or_else(|| anyhow::anyhow!("no audio mid found in SDP answer"))?;

        // Create data channel via direct API since accept_offer handles the SCTP m-line
        let telemetry_channel = rtc
            .direct_api()
            .create_data_channel(str0m::channel::ChannelConfig {
                label: "telemetry".to_string(),
                ..Default::default()
            });

        Ok((video_mid, audio_mid, telemetry_channel))
    }

    async fn handle_event(&mut self, event: Event) {
        match event {
            Event::Connected => {
                tracing::info!(session = %self.session_id, "webrtc connected");
            }
            Event::ChannelData(cd) => {
                // Parse as ClientMessage
                let data = cd.data;
                match serde_json::from_slice::<ClientMessage>(&data) {
                    Ok(ClientMessage::ClientMode { mode }) => {
                        let new_mode: ClientMode = mode.into();
                        let old = self.client_mode;
                        self.client_mode = new_mode;
                        let _ =
                            update_subscriber_demand(&self.slot, Some(old), new_mode).await;
                    }
                    Err(e) => {
                        tracing::debug!(
                            session = %self.session_id,
                            "unknown data channel message: {e}"
                        );
                    }
                }
            }
            Event::IceConnectionStateChange(state) => {
                tracing::debug!(session = %self.session_id, ?state, "ice state change");
            }
            _ => {}
        }
    }
}
