package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	// h264ClockRate is the standard clock rate for H.264 in RTP.
	h264ClockRate = 90000

	// maxNALSize is the MTU-safe threshold. NALs larger than this are
	// sent as FU-A fragments.
	maxNALSize = 1200
)

// whepSession tracks one viewer's WebRTC peer connection.
type whepSession struct {
	id         string
	deviceID   string
	pc         *webrtc.PeerConnection
	track      *webrtc.TrackLocalStaticRTP
	viewerID   uint64
	viewerChan <-chan NALFrame
	done       chan struct{}
}

// WHEPManager tracks active viewer sessions for cleanup.
type WHEPManager struct {
	mu       sync.Mutex
	sessions map[string]*whepSession // sessionID → session
}

// NewWHEPManager creates a new manager.
func NewWHEPManager() *WHEPManager {
	return &WHEPManager{
		sessions: make(map[string]*whepSession),
	}
}

// WHEPOffer handles POST /api/v1/whep/{deviceID} — WHEP offer/answer.
// Accepts an SDP offer, creates a PeerConnection with a video track
// sourced from the camera's live session, and returns an SDP answer.
func (a *App) WHEPOffer(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}

	session := a.Live.GetSession(deviceID)
	if session == nil {
		http.Error(w, "camera not streaming", http.StatusNotFound)
		return
	}

	// Read the SDP offer.
	offerBytes, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "reading offer", http.StatusBadRequest)
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offerBytes),
	}

	// Build ICE configuration. Use the server's public IP as a host
	// candidate via ICE-lite behavior (no STUN/TURN needed since the
	// server has a public IP).
	iceServers := []webrtc.ICEServer{}

	se := webrtc.SettingEngine{}
	// Set the public IP as NAT 1:1 mapping so pion advertises it.
	if ip := a.Config.PublicIP; ip != "" {
		se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
	}
	// Restrict UDP port range for firewall-friendliness.
	se.SetEphemeralUDPPortRange(50000, 50200)
	// Enable ICE-lite: server doesn't generate connectivity checks.
	se.SetLite(true)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		slog.Error("whep: creating peer connection", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Create video track.
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: h264ClockRate,
		},
		"video",
		"ghostcam-"+deviceID,
	)
	if err != nil {
		pc.Close()
		slog.Error("whep: creating track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		slog.Error("whep: adding track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Set remote offer.
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		slog.Error("whep: setting remote description", "error", err)
		http.Error(w, "invalid SDP offer", http.StatusBadRequest)
		return
	}

	// Create answer.
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		slog.Error("whep: creating answer", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		slog.Error("whep: setting local description", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Wait for ICE gathering to complete (ICE-lite gathers instantly
	// since we have a known public IP).
	<-webrtc.GatheringCompletePromise(pc)

	// Subscribe to the camera's live session.
	viewerID, viewerChan := session.subscribe()

	// Notify camera to start streaming if this is the first viewer.
	if session.viewerCount() == 1 {
		sendStreamControl(r.Context(), session.cameraConn, "start_stream")
	}

	sessionID := uuid.New().String()
	ws := &whepSession{
		id:         sessionID,
		deviceID:   deviceID,
		pc:         pc,
		track:      track,
		viewerID:   viewerID,
		viewerChan: viewerChan,
		done:       make(chan struct{}),
	}

	a.WHEP.mu.Lock()
	a.WHEP.sessions[sessionID] = ws
	a.WHEP.mu.Unlock()

	// Clean up on ICE disconnect.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Debug("whep: connection state", "session", sessionID, "state", state.String())
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateDisconnected ||
			state == webrtc.PeerConnectionStateClosed {
			a.cleanupWHEPSession(sessionID)
		}
	})

	// Start forwarding NAL frames to the WebRTC track.
	go a.forwardToTrack(ws)

	// Return SDP answer.
	localDesc := pc.LocalDescription()
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/v1/whep/"+deviceID+"/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(localDesc.SDP))
}

// WHEPDelete handles DELETE /api/v1/whep/{deviceID}/{sessionID} — tears
// down a WHEP session explicitly.
func (a *App) WHEPDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	a.cleanupWHEPSession(sessionID)
	w.WriteHeader(http.StatusNoContent)
}

// cleanupWHEPSession tears down a viewer session and notifies the camera
// to stop streaming if no viewers remain.
func (a *App) cleanupWHEPSession(sessionID string) {
	a.WHEP.mu.Lock()
	ws, ok := a.WHEP.sessions[sessionID]
	if !ok {
		a.WHEP.mu.Unlock()
		return
	}
	delete(a.WHEP.sessions, sessionID)
	a.WHEP.mu.Unlock()

	// Signal the forwarder goroutine to stop.
	select {
	case <-ws.done:
	default:
		close(ws.done)
	}

	ws.pc.Close()

	// Unsubscribe from the live session.
	session := a.Live.GetSession(ws.deviceID)
	if session != nil {
		session.unsubscribe(ws.viewerID)
		// If no viewers remain, tell camera to stop streaming.
		if session.viewerCount() == 0 {
			sendStreamControl(context.Background(), session.cameraConn, "stop_stream")
		}
	}

	slog.Info("whep: session closed", "session", sessionID, "device_id", ws.deviceID)
}

// forwardToTrack reads NAL frames from the live session and writes them
// as RTP packets to the WebRTC track.
func (a *App) forwardToTrack(ws *whepSession) {
	var seqNum uint16
	var timestamp uint32
	var ssrc uint32 = 1 // arbitrary; pion will set the real SSRC
	payloadType := uint8(96) // dynamic payload type for H.264

	for {
		select {
		case <-ws.done:
			return
		case frame, ok := <-ws.viewerChan:
			if !ok {
				return
			}

			// Advance timestamp by one frame duration (~3000 ticks at 30fps).
			timestamp += h264ClockRate / 30

			// Package NAL unit as RTP. For NALs that fit in one packet,
			// send as a single NAL unit packet. For larger NALs, use
			// FU-A fragmentation.
			packets := packetizeNAL(frame.Data, seqNum, timestamp, ssrc, payloadType)
			for _, pkt := range packets {
				raw, err := pkt.Marshal()
				if err != nil {
					continue
				}
				if _, err := ws.track.Write(raw); err != nil {
					return
				}
				seqNum++
			}
		}
	}
}

// packetizeNAL wraps an H.264 NAL unit into one or more RTP packets.
// Single NAL unit mode for small NALs, FU-A fragmentation for large ones.
func packetizeNAL(nal []byte, startSeq uint16, ts uint32, ssrc uint32, pt uint8) []*rtp.Packet {
	if len(nal) == 0 {
		return nil
	}

	if len(nal) <= maxNALSize {
		// Single NAL unit packet.
		return []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: startSeq,
				Timestamp:      ts,
				SSRC:           ssrc,
				Marker:         true, // end of access unit
			},
			Payload: nal,
		}}
	}

	// FU-A fragmentation (RFC 6184 Section 5.8).
	nalHeader := nal[0]
	nalType := nalHeader & 0x1F
	nri := nalHeader & 0x60

	payload := nal[1:] // skip the NAL header byte
	var packets []*rtp.Packet
	seq := startSeq
	first := true

	for len(payload) > 0 {
		chunkSize := maxNALSize - 2 // 2 bytes for FU indicator + FU header
		if chunkSize > len(payload) {
			chunkSize = len(payload)
		}

		fuIndicator := nri | 28 // FU-A type = 28
		fuHeader := nalType
		if first {
			fuHeader |= 0x80 // Start bit
			first = false
		}
		if chunkSize == len(payload) {
			fuHeader |= 0x40 // End bit
		}

		chunk := make([]byte, 2+chunkSize)
		chunk[0] = fuIndicator
		chunk[1] = fuHeader
		copy(chunk[2:], payload[:chunkSize])

		isLast := chunkSize == len(payload)
		packets = append(packets, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           ssrc,
				Marker:         isLast,
			},
			Payload: chunk,
		})

		payload = payload[chunkSize:]
		seq++
	}

	return packets
}

