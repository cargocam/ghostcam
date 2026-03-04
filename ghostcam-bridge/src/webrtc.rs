use crate::data_channel::{self, DataChannelMessage};
use crate::router::{CameraFrame, DeviceId, SessionId};
use crate::rtp;
use crate::AppState;
use ghostcam_common::group::GroupId;
use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Instant;
use str0m::change::SdpOffer;
use str0m::channel::ChannelId;
use str0m::format::Codec;
use str0m::media::{Direction, Frequency, MediaKind, Mid};
use str0m::net::Protocol;
use str0m::{Candidate, Event, Input, Output, Rtc};
use tokio::net::UdpSocket;
use tokio::sync::{broadcast, mpsc, oneshot};
use tracing::{info, warn};

pub enum WebRtcCommand {
    CreateSession {
        sdp_offer: String,
        group_id: GroupId,
        reply: oneshot::Sender<Result<(SessionId, String), String>>,
    },
    DeleteSession {
        session_id: SessionId,
        reply: oneshot::Sender<Result<(), String>>,
    },
    TrickleIce {
        session_id: SessionId,
        candidate: String,
        reply: oneshot::Sender<Result<(), String>>,
    },
}

struct RtcSession {
    rtc: Rtc,
    group_id: GroupId,
    /// Maps device_id -> video Mid
    video_track_map: HashMap<DeviceId, Mid>,
    /// Maps device_id -> audio Mid
    audio_track_map: HashMap<DeviceId, Mid>,
    /// Data channel ID — set when the browser's channel opens
    data_channel_id: Option<ChannelId>,
    /// Camera list + track map to send when data channel opens
    pending_camera_list: Option<Vec<crate::data_channel::CameraInfo>>,
    pending_track_map: Option<Vec<crate::data_channel::TrackMapping>>,
    created_at: Instant,
}

pub struct WebRtcEngine {
    state: Arc<AppState>,
    socket: UdpSocket,
    sessions: HashMap<SessionId, RtcSession>,
    /// Reverse lookup: remote addr -> session_id
    addr_to_session: HashMap<SocketAddr, SessionId>,
    cmd_rx: mpsc::Receiver<WebRtcCommand>,
    frame_rx: broadcast::Receiver<CameraFrame>,
    buf: Vec<u8>,
    /// Accumulates non-VCL NALs (SPS/PPS/SEI) per device, flushed when VCL NAL arrives.
    /// Stored as Annex-B format: [00 00 00 01][NAL][00 00 00 01][NAL]...
    nal_accumulator: HashMap<DeviceId, Vec<u8>>,
}

impl WebRtcEngine {
    pub async fn new(
        state: Arc<AppState>,
        cmd_rx: mpsc::Receiver<WebRtcCommand>,
        frame_rx: broadcast::Receiver<CameraFrame>,
    ) -> Self {
        let socket = UdpSocket::bind("0.0.0.0:0").await.expect("bind UDP socket");
        let local_addr = socket.local_addr().unwrap();
        info!(addr = %local_addr, "WebRTC UDP socket bound");

        Self {
            state,
            socket,
            sessions: HashMap::new(),
            addr_to_session: HashMap::new(),
            cmd_rx,
            frame_rx,
            buf: vec![0u8; 2000],
            nal_accumulator: HashMap::new(),
        }
    }

    pub async fn run(&mut self) {
        let mut tick_interval = tokio::time::interval(std::time::Duration::from_millis(20));
        let mut telemetry_interval = tokio::time::interval(std::time::Duration::from_secs(2));

        loop {
            tokio::select! {
                Some(cmd) = self.cmd_rx.recv() => {
                    self.handle_command(cmd).await;
                }
                Ok((len, remote)) = self.socket.recv_from(&mut self.buf) => {
                    self.handle_udp_receive(remote, len).await;
                }
                Ok(frame) = self.frame_rx.recv() => {
                    self.handle_camera_frame(frame);
                }
                _ = tick_interval.tick() => {
                    self.poll_all_sessions().await;
                }
                _ = telemetry_interval.tick() => {
                    self.send_telemetry();
                }
            }
        }
    }

    async fn handle_command(&mut self, cmd: WebRtcCommand) {
        match cmd {
            WebRtcCommand::CreateSession {
                sdp_offer,
                group_id,
                reply,
            } => {
                let result = self.create_session(&sdp_offer, &group_id).await;
                let _ = reply.send(result);
            }
            WebRtcCommand::DeleteSession { session_id, reply } => {
                let result = self.delete_session(&session_id);
                let _ = reply.send(result);
            }
            WebRtcCommand::TrickleIce {
                session_id,
                candidate,
                reply,
            } => {
                let result = self.trickle_ice(&session_id, &candidate);
                let _ = reply.send(result);
            }
        }
    }

    async fn create_session(
        &mut self,
        sdp_offer_str: &str,
        group_id: &GroupId,
    ) -> Result<(SessionId, String), String> {
        let session_id = uuid::Uuid::new_v4().to_string();

        let sdp_offer =
            SdpOffer::from_sdp_string(sdp_offer_str).map_err(|e| format!("invalid SDP: {e}"))?;

        let mut rtc = Rtc::builder().set_ice_lite(true).build();

        // Add host candidate with public IP
        let local_addr = self.socket.local_addr().unwrap();
        let candidate_addr = SocketAddr::new(self.state.public_ip, local_addr.port());
        let candidate = Candidate::host(candidate_addr, Protocol::Udp)
            .map_err(|e| format!("candidate error: {e}"))?;
        rtc.add_local_candidate(candidate);

        // Get cameras in the group
        let router = self.state.router.read().await;
        let cameras = router.get_cameras_in_group(group_id);

        // Build camera list for data channel
        let camera_list: Vec<_> = cameras
            .iter()
            .map(|c| crate::data_channel::CameraInfo {
                device_id: c.device_id.clone(),
                group_id: c.group_id.clone(),
                capabilities: c.capabilities.clone(),
            })
            .collect();

        let camera_device_ids: Vec<String> = cameras.iter().map(|c| c.device_id.clone()).collect();
        drop(router);

        // Use SDP API to add media tracks and accept offer.
        // We add_media for each camera so str0m generates the correct sendonly direction
        // in the answer, but the Mids returned here are temporary — str0m will use the
        // browser's offer Mids (e.g. "0", "1") in the actual answer.
        let mut sdp_api = rtc.sdp_api();

        for device_id in &camera_device_ids {
            sdp_api.add_media(
                MediaKind::Video,
                Direction::SendOnly,
                Some(device_id.clone()),
                None,
            );
            sdp_api.add_media(
                MediaKind::Audio,
                Direction::SendOnly,
                Some(device_id.clone()),
                None,
            );
        }

        // Don't add a bridge-side data channel — the browser's createDataChannel('telemetry')
        // is already in the offer. We'll get the channel ID via Event::ChannelOpen.

        let answer = sdp_api
            .accept_offer(sdp_offer)
            .map_err(|e| format!("accept offer failed: {e}"))?;

        let sdp_answer = answer.to_sdp_string();

        // Parse the SDP answer to discover actual Mids assigned by str0m.
        // The add_media return values use str0m-internal Mids, but the SDP answer
        // uses the browser's offer Mids (e.g. "0", "1", "2").
        let mut video_mids: Vec<Mid> = Vec::new();
        let mut audio_mids: Vec<Mid> = Vec::new();
        let mut current_kind = "";
        for line in sdp_answer.lines() {
            if line.starts_with("m=video") {
                current_kind = "video";
            } else if line.starts_with("m=audio") {
                current_kind = "audio";
            } else if line.starts_with("m=application") {
                current_kind = "application";
            } else if let Some(mid_str) = line.strip_prefix("a=mid:") {
                let mid: Mid = mid_str.trim().into();
                match current_kind {
                    "video" => video_mids.push(mid),
                    "audio" => audio_mids.push(mid),
                    _ => {}
                }
            }
        }

        let mut video_track_map = HashMap::new();
        let mut audio_track_map = HashMap::new();
        for (i, device_id) in camera_device_ids.iter().enumerate() {
            if let Some(mid) = video_mids.get(i) {
                video_track_map.insert(device_id.clone(), mid.clone());
            }
            if let Some(mid) = audio_mids.get(i) {
                audio_track_map.insert(device_id.clone(), mid.clone());
            }
        }

        // Build track map for data channel (maps mid -> device_id)
        let mut track_mappings = Vec::new();
        for (device_id, mid) in &video_track_map {
            track_mappings.push(crate::data_channel::TrackMapping {
                mid: format!("{mid}"),
                device_id: device_id.clone(),
                kind: "video".into(),
            });
        }
        for (device_id, mid) in &audio_track_map {
            track_mappings.push(crate::data_channel::TrackMapping {
                mid: format!("{mid}"),
                device_id: device_id.clone(),
                kind: "audio".into(),
            });
        }

        info!(
            video_mids = ?video_track_map.values().collect::<Vec<_>>(),
            audio_mids = ?audio_track_map.values().collect::<Vec<_>>(),
            "track map from SDP answer"
        );

        let session = RtcSession {
            rtc,
            group_id: group_id.clone(),
            video_track_map,
            audio_track_map,
            data_channel_id: None,
            pending_camera_list: Some(camera_list),
            pending_track_map: Some(track_mappings),
            created_at: Instant::now(),
        };

        self.sessions.insert(session_id.clone(), session);
        info!(
            session_id = %session_id,
            group_id = %group_id,
            cameras = camera_device_ids.len(),
            "WebRTC session created"
        );

        Ok((session_id, sdp_answer))
    }

    fn delete_session(&mut self, session_id: &str) -> Result<(), String> {
        if self.sessions.remove(session_id).is_some() {
            self.addr_to_session.retain(|_, sid| sid != session_id);
            info!(session_id = %session_id, "session deleted");
            Ok(())
        } else {
            Err("session not found".into())
        }
    }

    fn trickle_ice(&mut self, session_id: &str, candidate: &str) -> Result<(), String> {
        if let Some(session) = self.sessions.get_mut(session_id) {
            let cand = Candidate::from_sdp_string(candidate)
                .map_err(|e| format!("invalid candidate: {e}"))?;
            session.rtc.add_remote_candidate(cand);
            Ok(())
        } else {
            Err("session not found".into())
        }
    }

    async fn handle_udp_receive(&mut self, remote: SocketAddr, len: usize) {
        let data = self.buf[..len].to_vec();
        // Use public_ip + socket port as destination so str0m matches against the ICE candidate
        let local_port = self.socket.local_addr().unwrap().port();
        let local = SocketAddr::new(self.state.public_ip, local_port);

        let contents = match (&data[..]).try_into() {
            Ok(c) => c,
            Err(_) => return, // Not a valid STUN/DTLS/RTP packet
        };

        // Try known session first
        if let Some(session_id) = self.addr_to_session.get(&remote).cloned() {
            if let Some(session) = self.sessions.get_mut(&session_id) {
                let input = Input::Receive(
                    Instant::now(),
                    str0m::net::Receive {
                        proto: Protocol::Udp,
                        source: remote,
                        destination: local,
                        contents,
                    },
                );
                let _ = session.rtc.handle_input(input);
                return;
            }
        }

        // Try all sessions for new peer connections
        let mut matched = None;
        for (sid, session) in &mut self.sessions {
            let contents = match (&data[..]).try_into() {
                Ok(c) => c,
                Err(_) => return,
            };
            let input = Input::Receive(
                Instant::now(),
                str0m::net::Receive {
                    proto: Protocol::Udp,
                    source: remote,
                    destination: local,
                    contents,
                },
            );
            if session.rtc.accepts(&input) {
                let _ = session.rtc.handle_input(input);
                matched = Some(sid.clone());
                break;
            }
        }
        if let Some(sid) = matched {
            self.addr_to_session.insert(remote, sid);
        }
    }

    fn handle_camera_frame(&mut self, frame: CameraFrame) {
        use ghostcam_common::frame::StreamType;
        use std::sync::atomic::{AtomicU64, Ordering};
        static FRAME_LOG_COUNTER: AtomicU64 = AtomicU64::new(0);

        let now = Instant::now();

        match frame.stream_type {
            StreamType::Video => {
                // Accumulate non-VCL NALs (SPS/PPS/SEI) and flush together with
                // the next VCL NAL (IDR/slice) as Annex-B format. str0m's H264Packetizer
                // expects Annex-B and handles STAP-A/FU-A creation.
                // This prevents SPS/PPS from being consumed by the packetizer before
                // ICE connects, ensuring they're always paired with a decodable frame.
                let nal_type = if frame.payload.is_empty() { 0 } else { frame.payload[0] & 0x1F };
                let is_vcl = nal_type == 1 || nal_type == 5; // slice or IDR

                let acc = self.nal_accumulator
                    .entry(frame.device_id.clone())
                    .or_default();

                if !is_vcl {
                    // Buffer non-VCL NAL with Annex-B start code
                    acc.extend_from_slice(&[0, 0, 0, 1]);
                    acc.extend_from_slice(&frame.payload);
                    // Don't write to sessions yet — wait for VCL NAL
                } else {
                    // Build combined payload: accumulated non-VCL NALs + this VCL NAL
                    acc.extend_from_slice(&[0, 0, 0, 1]);
                    acc.extend_from_slice(&frame.payload);
                    let combined = std::mem::take(acc);

                    let rtp_ts = rtp::h264_rtp_timestamp(frame.timestamp_us);

                    for (_session_id, session) in &mut self.sessions {
                        if let Some(&mid) = session.video_track_map.get(&frame.device_id) {
                            let pt = {
                                if let Some(writer) = session.rtc.writer(mid) {
                                    writer.payload_params()
                                        .find(|p| p.spec().codec == Codec::H264)
                                        .map(|p| p.pt())
                                } else {
                                    None
                                }
                            };

                            if let Some(pt) = pt {
                                let rtp_time =
                                    str0m::media::MediaTime::from_90khz(rtp_ts as u64);
                                if let Some(writer) = session.rtc.writer(mid) {
                                    let _ = writer.write(
                                        pt,
                                        now,
                                        rtp_time,
                                        combined.clone(),
                                    );
                                }
                            }
                        }
                    }
                }
            }
            StreamType::Audio => {
                let rtp_ts = rtp::opus_rtp_timestamp(frame.timestamp_us);

                for (_session_id, session) in &mut self.sessions {
                    if let Some(&mid) = session.audio_track_map.get(&frame.device_id) {
                        let pt = {
                            if let Some(writer) = session.rtc.writer(mid) {
                                writer.payload_params()
                                    .find(|p| p.spec().codec == Codec::Opus)
                                    .map(|p| p.pt())
                            } else {
                                None
                            }
                        };

                        if let Some(pt) = pt {
                            let rtp_time =
                                str0m::media::MediaTime::new(rtp_ts as u64, Frequency::FORTY_EIGHT_KHZ);
                            if let Some(writer) = session.rtc.writer(mid) {
                                let _ = writer.write(
                                    pt,
                                    now,
                                    rtp_time,
                                    frame.payload.to_vec(),
                                );
                            }
                        }
                    }
                }
            }
        }
    }

    async fn poll_all_sessions(&mut self) {
        let now = Instant::now();
        let mut to_remove: Vec<String> = Vec::new();

        for (session_id, session) in &mut self.sessions {
            session.rtc.handle_input(Input::Timeout(now));

            loop {
                match session.rtc.poll_output() {
                    Ok(output) => match output {
                        Output::Transmit(transmit) => {
                            let _ = self
                                .socket
                                .send_to(&transmit.contents, transmit.destination)
                                .await;
                        }
                        Output::Timeout(_t) => {
                            break;
                        }
                        Output::Event(event) => match &event {
                            Event::IceConnectionStateChange(state) => {
                                info!(session_id = %session_id, state = ?state, "ICE state change");
                                // Don't auto-remove on Disconnected — it's transient.
                                // Sessions are cleaned up via DELETE /session or explicit timeout.
                            }
                            Event::ChannelOpen(ch_id, label) => {
                                info!(session_id = %session_id, label = %label, "data channel open");
                                session.data_channel_id = Some(*ch_id);
                                // Send pending camera list now that the channel is open
                                if let Some(camera_list) = session.pending_camera_list.take() {
                                    let msg = DataChannelMessage::Cameras { cameras: camera_list };
                                    if let Ok(json) = serde_json::to_string(&msg) {
                                        if let Some(mut ch) = session.rtc.channel(*ch_id) {
                                            let _ = ch.write(false, json.as_bytes());
                                        }
                                    }
                                }
                                // Send track map so viewer can associate tracks with cameras
                                if let Some(tracks) = session.pending_track_map.take() {
                                    let msg = DataChannelMessage::TrackMap { tracks };
                                    if let Ok(json) = serde_json::to_string(&msg) {
                                        if let Some(mut ch) = session.rtc.channel(*ch_id) {
                                            let _ = ch.write(false, json.as_bytes());
                                        }
                                    }
                                }
                            }
                            _ => {}
                        },
                    },
                    Err(_) => break,
                }
            }
        }

        for sid in to_remove {
            self.delete_session(&sid).ok();
        }
    }

    fn send_telemetry(&mut self) {
        for (_session_id, session) in &mut self.sessions {
            if let Some(ch_id) = session.data_channel_id {
                for (i, (device_id, _)) in session.video_track_map.iter().enumerate() {
                    let elapsed = session.created_at.elapsed().as_secs();
                    let t = elapsed as f64;

                    // Each camera gets a unique GPS starting point offset and movement
                    let idx = i as f64;
                    let base_lat = 37.7749 + idx * 0.005;  // SF area, spaced apart
                    let base_lon = -122.4194 + idx * 0.005;
                    let lat = base_lat + (t * 0.02 + idx).sin() * 0.002;
                    let lon = base_lon + (t * 0.015 + idx * 1.5).cos() * 0.002;

                    let msg = DataChannelMessage::Telemetry {
                        device_id: device_id.clone(),
                        cpu_percent: 28.0 + (t * 0.1).sin() * 5.0,
                        temp_celsius: 44.0 + (t * 0.05).cos() * 3.0,
                        memory_mb: 128.0 + t * 0.5,
                        uptime_secs: elapsed,
                        gps: Some(data_channel::GpsData {
                            latitude: lat,
                            longitude: lon,
                        }),
                    };
                    if let Ok(json) = serde_json::to_string(&msg) {
                        if let Some(mut ch) = session.rtc.channel(ch_id) {
                            let _ = ch.write(false, json.as_bytes());
                        }
                    }
                }
            }
        }
    }
}
