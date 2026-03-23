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
use tokio::sync::{broadcast, mpsc};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use super::data_channel::ClientMessage;
use super::rtp::NalAccumulator;
use super::udp::{SharedWebRtcSocket, UdpPacket};
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
    /// Shared WebRTC UDP socket (all sessions share one port)
    shared_socket: Arc<SharedWebRtcSocket>,
    /// Incoming packets routed to this session by the dispatch loop
    udp_rx: mpsc::UnboundedReceiver<UdpPacket>,
    /// Local ICE ufrag (used to unregister from the shared socket on drop)
    local_ufrag: String,
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
        shared_socket: Arc<SharedWebRtcSocket>,
        public_ip: std::net::IpAddr,
        cancel: CancellationToken,
    ) -> Result<(Self, String)> {
        // 1. Build candidate address: use GHOSTCAM_PUBLIC_IP with the shared socket's port.
        //    shared_socket.local_addr.ip() is 0.0.0.0 (unspecified); str0m rejects that.
        let candidate_addr = SocketAddr::new(public_ip, shared_socket.local_addr.port());

        // 2. Create str0m Rtc with ICE-lite, H.264 + Opus only.
        let mut rtc = Rtc::builder()
            .set_ice_lite(true)
            .clear_codecs()
            .enable_h264(true)
            .enable_opus(true)
            .build();

        // 3. Add local candidate using the shared socket's address.
        let candidate = Candidate::host(candidate_addr, Protocol::Udp)?;
        rtc.add_local_candidate(candidate);

        // 4. Accept the browser's SDP offer — this creates media entries from the offer's
        //    m-lines, including codec negotiation. We must NOT add_media() before accept_offer()
        //    because that creates media without codecs, leaving writer() with no payload params.
        let offer = SdpOffer::from_sdp_string(sdp_offer)?;
        let answer: SdpAnswer = rtc.sdp_api().accept_offer(offer)?;
        let answer_str = answer.to_sdp_string();

        // 5. Extract the local ICE ufrag from the SDP answer so we can register
        //    with the shared socket.
        let local_ufrag = parse_local_ufrag(&answer_str)
            .ok_or_else(|| anyhow::anyhow!("no ice-ufrag in SDP answer"))?;

        // 6. Register with the shared socket — get a receiver for incoming packets.
        let udp_rx = shared_socket.register(local_ufrag.clone()).await;

        // 7. Find the video and audio mids from the answer SDP.
        let (video_mid, audio_mid, telemetry_channel) =
            Self::parse_mids_from_sdp(&answer_str, &mut rtc)?;

        // 8. Subscribe to IngestSlot broadcast channels.
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
            shared_socket,
            udp_rx,
            local_ufrag,
            local_candidate_addr: candidate_addr,
            nal_acc: NalAccumulator::new(),
            rtp_start: std::time::Instant::now(),
            cancel,
        };

        Ok((handle, answer_str))
    }

    /// Run the WebRTC event loop. Blocks until session ends.
    pub async fn run(mut self) -> Result<()> {
        // Start in live mode — increment subscriber counts.
        let _ = update_subscriber_demand(&self.slot, None, ClientMode::Live).await;

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
                        let _ = self.shared_socket.send_to(&transmit.contents, dest).await;
                        // Register the destination as a fast-path source for future
                        // packets arriving from that address.
                        self.shared_socket
                            .connect(dest, self.local_ufrag.clone())
                            .await;
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

                packet = self.udp_rx.recv() => {
                    match packet {
                        Some((src, data)) => {
                            let receive = Receive {
                                proto: Protocol::Udp,
                                source: src,
                                destination: self.local_candidate_addr,
                                contents: data.as_slice().try_into().unwrap(),
                            };
                            if let Err(e) = self.rtc.handle_input(Input::Receive(
                                std::time::Instant::now(),
                                receive,
                            )) {
                                tracing::debug!(session = %self.session_id, "receive error: {e}");
                            }
                        }
                        None => {
                            // Shared socket dispatch loop exited — shut down.
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

        // Unregister from the shared socket.
        self.shared_socket.unregister(&self.local_ufrag).await;

        // Decrement subscriber counts on exit.
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
        match self.rtc.channel(self.telemetry_channel) {
            Some(mut channel) => {
                if let Err(e) = channel.write(false, &data) {
                    tracing::debug!(session = %self.session_id, "telemetry channel write error: {e}");
                } else {
                    tracing::trace!(session = %self.session_id, bytes = data.len(), "telemetry sent");
                }
            }
            None => {
                tracing::debug!(session = %self.session_id, "telemetry channel not yet open, dropping datagram");
            }
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

        // Pre-negotiated data channel (stream ID 1) — browser uses `negotiated: true, id: 1`
        // so no DATA_CHANNEL_OPEN/ACK exchange is needed. str0m marks it Open immediately
        // after SCTP is established, avoiding the DCEP round trip.
        let telemetry_channel = rtc
            .direct_api()
            .create_data_channel(str0m::channel::ChannelConfig {
                label: "telemetry".to_string(),
                negotiated: Some(1),
                ..Default::default()
            });

        Ok((video_mid, audio_mid, telemetry_channel))
    }

    async fn handle_event(&mut self, event: Event) {
        match event {
            Event::Connected => {
                tracing::info!(session = %self.session_id, "webrtc connected (dtls established)");
            }
            Event::ChannelOpen(id, label) => {
                tracing::info!(session = %self.session_id, ?id, label = %label, "data channel open");
            }
            Event::ChannelClose(id) => {
                tracing::debug!(session = %self.session_id, ?id, "data channel closed");
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

/// Extract the local ICE ufrag from an SDP string (`a=ice-ufrag:<value>`).
fn parse_local_ufrag(sdp: &str) -> Option<String> {
    for line in sdp.split("\r\n") {
        if let Some(ufrag) = line.strip_prefix("a=ice-ufrag:") {
            return Some(ufrag.trim().to_owned());
        }
    }
    None
}
