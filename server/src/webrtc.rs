use ghostcam::data_channel::{self, CameraInfo, DataChannelMessage, TrackMapping};
use ghostcam::router::{CameraFrame, DeviceId, SessionId};
use ghostcam::rtp;
use ghostcam::telemetry::{SparseTelemetry, TelemetryData};
use crate::AppState;
use ghostcam::group::GroupId;
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
    pending_camera_list: Option<Vec<CameraInfo>>,
    pending_track_map: Option<Vec<TrackMapping>>,
    created_at: Instant,
    /// Per-camera telemetry state for this session
    telemetry_state: HashMap<DeviceId, TelemetryData>,
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
            .map(|c| CameraInfo {
                device_id: c.device_id.clone(),
                group_id: c.group_id.clone(),
                capabilities: c.capabilities.clone(),
            })
            .collect();

        let camera_device_ids: Vec<String> = cameras.iter().map(|c| c.device_id.clone()).collect();
        drop(router);

        // Use SDP API to add media tracks and accept offer.
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

        let answer = sdp_api
            .accept_offer(sdp_offer)
            .map_err(|e| format!("accept offer failed: {e}"))?;

        let sdp_answer = answer.to_sdp_string();

        // Parse the SDP answer to discover actual Mids assigned by str0m.
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
            track_mappings.push(TrackMapping {
                mid: format!("{mid}"),
                device_id: device_id.clone(),
                kind: "video".into(),
            });
        }
        for (device_id, mid) in &audio_track_map {
            track_mappings.push(TrackMapping {
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
            telemetry_state: HashMap::new(),
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
        use ghostcam::frame::StreamType;

        let now = Instant::now();

        match frame.stream_type {
            StreamType::Video => {
                let nal_type = if frame.payload.is_empty() { 0 } else { frame.payload[0] & 0x1F };
                let is_vcl = nal_type == 1 || nal_type == 5; // slice or IDR

                let acc = self.nal_accumulator
                    .entry(frame.device_id.clone())
                    .or_default();

                if !is_vcl {
                    // Buffer non-VCL NAL with Annex-B start code
                    acc.extend_from_slice(&[0, 0, 0, 1]);
                    acc.extend_from_slice(&frame.payload);
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
            StreamType::Telemetry => {
                // Decode sparse telemetry, merge into session state, send to viewer
                let sparse = match SparseTelemetry::decode(&frame.payload) {
                    Ok(s) => s,
                    Err(e) => {
                        warn!(device_id = %frame.device_id, error = %e, "failed to decode telemetry");
                        return;
                    }
                };

                for (_session_id, session) in &mut self.sessions {
                    // Only forward telemetry for cameras this session is watching
                    if !session.video_track_map.contains_key(&frame.device_id) {
                        continue;
                    }

                    let state = session
                        .telemetry_state
                        .entry(frame.device_id.clone())
                        .or_default();
                    sparse.merge_into(state);

                    if let Some(ch_id) = session.data_channel_id {
                        let msg = DataChannelMessage::Telemetry {
                            device_id: frame.device_id.clone(),
                            cpu_percent: state.cpu_percent as f64,
                            temp_celsius: state.temp_celsius.unwrap_or(0.0) as f64,
                            memory_mb: state.memory_mb as f64,
                            uptime_secs: state.uptime_secs,
                            gps: state.gps.as_ref().map(|g| data_channel::GpsData {
                                latitude: g.latitude,
                                longitude: g.longitude,
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

}
