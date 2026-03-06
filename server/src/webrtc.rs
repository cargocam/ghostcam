use ghostcam::data_channel::{self, CameraInfo, DataChannelMessage, TrackMapping};
use ghostcam::router::{CameraEvent, CameraFrame, DeviceId, SessionId};
use ghostcam::rtp;
use ghostcam::telemetry::{SparseTelemetry, TelemetryData};
use crate::AppState;
use ghostcam::group::GroupId;
use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, Instant};
use str0m::change::{SdpAnswer, SdpOffer, SdpPendingOffer};
use str0m::channel::ChannelId;
use str0m::format::Codec;
use str0m::media::{Direction, Frequency, MediaKind, Mid};
use str0m::net::Protocol;
use str0m::{Candidate, Event, Input, Output, Rtc};
use tokio::net::UdpSocket;
use tokio::sync::{broadcast, mpsc, oneshot};
use tracing::{debug, info, warn};

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
    /// SDP offer awaiting viewer's answer (only one at a time per str0m)
    pending_offer: Option<SdpPendingOffer>,
    pending_offer_timestamp: Option<Instant>,
    /// Track additions waiting for accept_answer before applying to track maps
    pending_track_additions: Vec<(DeviceId, Mid, Mid)>,
    /// Camera events queued while a renegotiation is in-flight
    queued_camera_events: Vec<CameraEvent>,
}

pub struct WebRtcEngine {
    state: Arc<AppState>,
    socket: UdpSocket,
    sessions: HashMap<SessionId, RtcSession>,
    /// Reverse lookup: remote addr -> session_id
    addr_to_session: HashMap<SocketAddr, SessionId>,
    cmd_rx: mpsc::Receiver<WebRtcCommand>,
    frame_rx: broadcast::Receiver<CameraFrame>,
    camera_event_rx: broadcast::Receiver<CameraEvent>,
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
        camera_event_rx: broadcast::Receiver<CameraEvent>,
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
            camera_event_rx,
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
                Ok(event) = self.camera_event_rx.recv() => {
                    self.handle_camera_event(event);
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
            pending_offer: None,
            pending_offer_timestamp: None,
            pending_track_additions: Vec::new(),
            queued_camera_events: Vec::new(),
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

    fn handle_camera_event(&mut self, event: CameraEvent) {
        let group_id = match &event {
            CameraEvent::Joined { group_id, .. } => group_id.clone(),
            CameraEvent::Left { group_id, .. } => group_id.clone(),
        };

        // Find sessions watching this group (or __all__)
        let session_ids: Vec<SessionId> = self.sessions.iter()
            .filter(|(_, s)| s.group_id == group_id || s.group_id.0 == "__all__")
            .map(|(sid, _)| sid.clone())
            .collect();

        for session_id in session_ids {
            match &event {
                CameraEvent::Joined { device_id, capabilities, .. } => {
                    self.add_camera_to_session(&session_id, device_id, &group_id, capabilities);
                }
                CameraEvent::Left { device_id, .. } => {
                    self.remove_camera_from_session(&session_id, device_id);
                }
            }
        }

        // Clean up NAL accumulator on camera leave
        if let CameraEvent::Left { device_id, .. } = &event {
            self.nal_accumulator.remove(device_id);
        }
    }

    fn add_camera_to_session(
        &mut self,
        session_id: &str,
        device_id: &str,
        group_id: &GroupId,
        capabilities: &[String],
    ) {
        let session = match self.sessions.get_mut(session_id) {
            Some(s) => s,
            None => return,
        };

        // If renegotiation in-flight, queue this event
        if session.pending_offer.is_some() {
            session.queued_camera_events.push(CameraEvent::Joined {
                device_id: device_id.to_string(),
                group_id: group_id.clone(),
                capabilities: capabilities.to_vec(),
            });
            debug!(session_id = %session_id, device_id = %device_id, "queued camera join (renegotiation in-flight)");
            return;
        }

        // Skip if camera already has tracks
        if session.video_track_map.contains_key(device_id) {
            return;
        }

        // If data channel not yet open, queue the event
        let ch_id = match session.data_channel_id {
            Some(id) => id,
            None => {
                session.queued_camera_events.push(CameraEvent::Joined {
                    device_id: device_id.to_string(),
                    group_id: group_id.clone(),
                    capabilities: capabilities.to_vec(),
                });
                debug!(session_id = %session_id, device_id = %device_id, "queued camera join (data channel not open)");
                return;
            }
        };

        // Send camera_join notification
        let camera_info = CameraInfo {
            device_id: device_id.to_string(),
            group_id: group_id.clone(),
            capabilities: capabilities.to_vec(),
        };
        let join_msg = DataChannelMessage::CameraJoin { camera: camera_info };
        if let Ok(json) = serde_json::to_string(&join_msg) {
            if let Some(mut ch) = session.rtc.channel(ch_id) {
                let _ = ch.write(false, json.as_bytes());
            }
        }

        // Add media tracks via SDP API
        let mut sdp_api = session.rtc.sdp_api();
        let video_mid = sdp_api.add_media(
            MediaKind::Video,
            Direction::SendOnly,
            Some(device_id.to_string()),
            None,
        );
        let audio_mid = sdp_api.add_media(
            MediaKind::Audio,
            Direction::SendOnly,
            Some(device_id.to_string()),
            None,
        );

        if let Some((offer, pending)) = sdp_api.apply() {
            let sdp_offer_str = offer.to_sdp_string();

            // Store pending state
            session.pending_offer = Some(pending);
            session.pending_offer_timestamp = Some(Instant::now());
            session.pending_track_additions.push((device_id.to_string(), video_mid, audio_mid));

            // Send renegotiate message
            let reneg_msg = DataChannelMessage::Renegotiate { sdp_offer: sdp_offer_str };
            if let Ok(json) = serde_json::to_string(&reneg_msg) {
                if let Some(mut ch) = session.rtc.channel(ch_id) {
                    let _ = ch.write(false, json.as_bytes());
                }
            }

            info!(
                session_id = %session_id,
                device_id = %device_id,
                video_mid = ?video_mid,
                audio_mid = ?audio_mid,
                "renegotiation started: adding camera"
            );
        } else {
            // No negotiation needed (shouldn't happen for media addition)
            session.video_track_map.insert(device_id.to_string(), video_mid);
            session.audio_track_map.insert(device_id.to_string(), audio_mid);
            Self::send_track_map_on_session(session);
        }
    }

    fn remove_camera_from_session(
        &mut self,
        session_id: &str,
        device_id: &str,
    ) {
        let session = match self.sessions.get_mut(session_id) {
            Some(s) => s,
            None => return,
        };

        // If renegotiation in-flight, queue this event
        if session.pending_offer.is_some() {
            // We need the group_id — grab from the session
            session.queued_camera_events.push(CameraEvent::Left {
                device_id: device_id.to_string(),
                group_id: session.group_id.clone(),
            });
            debug!(session_id = %session_id, device_id = %device_id, "queued camera leave (renegotiation in-flight)");
            return;
        }

        let video_mid = session.video_track_map.remove(device_id);
        let audio_mid = session.audio_track_map.remove(device_id);

        // Nothing to do if camera didn't have tracks
        if video_mid.is_none() && audio_mid.is_none() {
            return;
        }

        let ch_id = match session.data_channel_id {
            Some(id) => id,
            None => return,
        };

        // Send camera_leave notification
        let leave_msg = DataChannelMessage::CameraLeave { device_id: device_id.to_string() };
        if let Ok(json) = serde_json::to_string(&leave_msg) {
            if let Some(mut ch) = session.rtc.channel(ch_id) {
                let _ = ch.write(false, json.as_bytes());
            }
        }

        // Set direction to Inactive
        let mut sdp_api = session.rtc.sdp_api();
        if let Some(mid) = video_mid {
            sdp_api.set_direction(mid, Direction::Inactive);
        }
        if let Some(mid) = audio_mid {
            sdp_api.set_direction(mid, Direction::Inactive);
        }

        if let Some((offer, pending)) = sdp_api.apply() {
            let sdp_offer_str = offer.to_sdp_string();

            session.pending_offer = Some(pending);
            session.pending_offer_timestamp = Some(Instant::now());

            let reneg_msg = DataChannelMessage::Renegotiate { sdp_offer: sdp_offer_str };
            if let Ok(json) = serde_json::to_string(&reneg_msg) {
                if let Some(mut ch) = session.rtc.channel(ch_id) {
                    let _ = ch.write(false, json.as_bytes());
                }
            }

            info!(
                session_id = %session_id,
                device_id = %device_id,
                "renegotiation started: removing camera"
            );
        } else {
            // Direction change didn't need renegotiation — just send updated track map
            Self::send_track_map_on_session(session);
        }

        // Clean up telemetry state for this camera
        session.telemetry_state.remove(device_id);
    }

    /// Handle an SDP answer received from the viewer via data channel.
    fn handle_sdp_answer(session: &mut RtcSession, sdp_answer_str: &str, session_id: &str) {
        let pending = match session.pending_offer.take() {
            Some(p) => p,
            None => {
                warn!(session_id = %session_id, "received SDP answer but no pending offer");
                return;
            }
        };
        session.pending_offer_timestamp = None;

        let answer = match SdpAnswer::from_sdp_string(sdp_answer_str) {
            Ok(a) => a,
            Err(e) => {
                warn!(session_id = %session_id, error = %e, "failed to parse SDP answer");
                return;
            }
        };

        if let Err(e) = session.rtc.sdp_api().accept_answer(pending, answer) {
            warn!(session_id = %session_id, error = %e, "failed to accept SDP answer");
            session.pending_track_additions.clear();
            return;
        }

        // Apply pending track additions to track maps
        for (device_id, video_mid, audio_mid) in session.pending_track_additions.drain(..) {
            session.video_track_map.insert(device_id.clone(), video_mid);
            session.audio_track_map.insert(device_id, audio_mid);
        }

        // Send updated track map
        Self::send_track_map_on_session(session);

        info!(session_id = %session_id, "renegotiation completed");
    }

    /// Build and send a track_map message from the session's current track maps.
    fn send_track_map_on_session(session: &mut RtcSession) {
        let ch_id = match session.data_channel_id {
            Some(id) => id,
            None => return,
        };

        let mut tracks = Vec::new();
        for (device_id, mid) in &session.video_track_map {
            tracks.push(TrackMapping {
                mid: format!("{mid}"),
                device_id: device_id.clone(),
                kind: "video".into(),
            });
        }
        for (device_id, mid) in &session.audio_track_map {
            tracks.push(TrackMapping {
                mid: format!("{mid}"),
                device_id: device_id.clone(),
                kind: "audio".into(),
            });
        }

        let msg = DataChannelMessage::TrackMap { tracks };
        if let Ok(json) = serde_json::to_string(&msg) {
            if let Some(mut ch) = session.rtc.channel(ch_id) {
                let _ = ch.write(false, json.as_bytes());
            }
        }
    }

    async fn poll_all_sessions(&mut self) {
        let now = Instant::now();
        let mut to_remove: Vec<String> = Vec::new();
        // Collect SDP answers and queued events to process after the borrow loop
        let mut sdp_answers: Vec<(String, String)> = Vec::new();
        let mut sessions_with_queued: Vec<String> = Vec::new();

        for (session_id, session) in &mut self.sessions {
            // Check for renegotiation timeout
            if let Some(ts) = session.pending_offer_timestamp {
                if now.duration_since(ts) > Duration::from_secs(10) {
                    warn!(session_id = %session_id, "renegotiation timed out");
                    session.pending_offer = None;
                    session.pending_offer_timestamp = None;
                    session.pending_track_additions.clear();
                    if !session.queued_camera_events.is_empty() {
                        sessions_with_queued.push(session_id.clone());
                    }
                }
            }

            let _ = session.rtc.handle_input(Input::Timeout(now));

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
                                // Process any queued camera events now that the channel is open
                                if !session.queued_camera_events.is_empty() {
                                    sessions_with_queued.push(session_id.clone());
                                }
                            }
                            Event::ChannelData(data) => {
                                // Parse incoming data channel messages (viewer→server)
                                if !data.binary {
                                    if let Ok(text) = std::str::from_utf8(&data.data) {
                                        if let Ok(msg) = serde_json::from_str::<DataChannelMessage>(text) {
                                            if let DataChannelMessage::SdpAnswer { sdp_answer } = msg {
                                                sdp_answers.push((session_id.clone(), sdp_answer));
                                            }
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

        // Process SDP answers outside the poll loop
        for (session_id, sdp_answer) in sdp_answers {
            if let Some(session) = self.sessions.get_mut(&session_id) {
                Self::handle_sdp_answer(session, &sdp_answer, &session_id);
                // After completing renegotiation, check for queued events
                if !session.queued_camera_events.is_empty() {
                    sessions_with_queued.push(session_id);
                }
            }
        }

        // Process queued camera events
        for session_id in sessions_with_queued {
            if let Some(session) = self.sessions.get_mut(&session_id) {
                if session.pending_offer.is_some() {
                    continue; // Still in-flight, will process next tick
                }
                let events: Vec<CameraEvent> = session.queued_camera_events.drain(..).collect();
                // Re-dispatch events — the first one that needs renegotiation will proceed,
                // the rest will re-queue since pending_offer will be set
                for event in events {
                    match event {
                        CameraEvent::Joined { device_id, group_id, capabilities } => {
                            self.add_camera_to_session(&session_id, &device_id, &group_id, &capabilities);
                        }
                        CameraEvent::Left { device_id, .. } => {
                            self.remove_camera_from_session(&session_id, &device_id);
                        }
                    }
                }
            }
        }

        for sid in to_remove {
            self.delete_session(&sid).ok();
        }
    }

}
