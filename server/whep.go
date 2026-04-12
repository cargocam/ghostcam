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
	h264ClockRate = 90000
	opusClockRate = 48000
	maxNALSize    = 1200
)

// whepSession tracks one viewer's WebRTC peer connection.
type whepSession struct {
	id         string
	deviceID   string
	pc         *webrtc.PeerConnection
	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP
	viewerID   uint64
	viewerChan <-chan MediaFrame
	done       chan struct{}
}

// WHEPManager tracks active viewer sessions for cleanup.
type WHEPManager struct {
	mu       sync.Mutex
	sessions map[string]*whepSession
}

func NewWHEPManager() *WHEPManager {
	return &WHEPManager{
		sessions: make(map[string]*whepSession),
	}
}

// WHEPOffer handles POST /api/v1/whep/{deviceID} — WHEP offer/answer.
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

	offerBytes, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "reading offer", http.StatusBadRequest)
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offerBytes),
	}

	se := webrtc.SettingEngine{}
	if ip := a.Config.PublicIP; ip != "" {
		se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
	}
	se.SetEphemeralUDPPortRange(50000, 50200)
	se.SetLite(true)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		slog.Error("whep: creating peer connection", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Video track: H.264
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: h264ClockRate,
		},
		"video",
		"ghostcam-"+deviceID,
	)
	if err != nil {
		pc.Close()
		slog.Error("whep: creating video track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		slog.Error("whep: adding video track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Audio track: Opus
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: opusClockRate,
			Channels:  2,
		},
		"audio",
		"ghostcam-"+deviceID,
	)
	if err != nil {
		pc.Close()
		slog.Error("whep: creating audio track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		pc.Close()
		slog.Error("whep: adding audio track", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		slog.Error("whep: setting remote description", "error", err)
		http.Error(w, "invalid SDP offer", http.StatusBadRequest)
		return
	}

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

	<-webrtc.GatheringCompletePromise(pc)

	viewerID, viewerChan := session.subscribe()

	if session.viewerCount() == 1 {
		sendStreamControl(r.Context(), session.cameraConn, "start_stream")
	}

	sessionID := uuid.New().String()
	ws := &whepSession{
		id:         sessionID,
		deviceID:   deviceID,
		pc:         pc,
		videoTrack: videoTrack,
		audioTrack: audioTrack,
		viewerID:   viewerID,
		viewerChan: viewerChan,
		done:       make(chan struct{}),
	}

	a.WHEP.mu.Lock()
	a.WHEP.sessions[sessionID] = ws
	a.WHEP.mu.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Debug("whep: connection state", "session", sessionID, "state", state.String())
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateDisconnected ||
			state == webrtc.PeerConnectionStateClosed {
			a.cleanupWHEPSession(sessionID)
		}
	})

	go a.forwardToTrack(ws)

	localDesc := pc.LocalDescription()
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/v1/whep/"+deviceID+"/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(localDesc.SDP))
}

// WHEPDelete handles DELETE /api/v1/whep/{deviceID}/{sessionID}.
func (a *App) WHEPDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	a.cleanupWHEPSession(sessionID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) cleanupWHEPSession(sessionID string) {
	a.WHEP.mu.Lock()
	ws, ok := a.WHEP.sessions[sessionID]
	if !ok {
		a.WHEP.mu.Unlock()
		return
	}
	delete(a.WHEP.sessions, sessionID)
	a.WHEP.mu.Unlock()

	select {
	case <-ws.done:
	default:
		close(ws.done)
	}

	ws.pc.Close()

	session := a.Live.GetSession(ws.deviceID)
	if session != nil {
		session.unsubscribe(ws.viewerID)
		if session.viewerCount() == 0 {
			sendStreamControl(context.Background(), session.cameraConn, "stop_stream")
		}
	}

	slog.Info("whep: session closed", "session", sessionID, "device_id", ws.deviceID)
}

// forwardToTrack reads media frames from the live session and writes
// them as RTP packets to the appropriate WebRTC track.
func (a *App) forwardToTrack(ws *whepSession) {
	var videoSeq uint16
	var audioSeq uint16
	var videoTS uint32
	var audioTS uint32

	for {
		select {
		case <-ws.done:
			return
		case frame, ok := <-ws.viewerChan:
			if !ok {
				return
			}

			if frame.IsAudio {
				// Opus: each packet is one frame. Advance timestamp by
				// 960 samples (20ms at 48kHz).
				audioTS += 960
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    111, // dynamic PT for Opus
						SequenceNumber: audioSeq,
						Timestamp:      audioTS,
						SSRC:           2,
						Marker:         true,
					},
					Payload: frame.Data,
				}
				raw, err := pkt.Marshal()
				if err != nil {
					continue
				}
				if _, err := ws.audioTrack.Write(raw); err != nil {
					return
				}
				audioSeq++
			} else {
				// H.264: advance by one frame period (~3000 ticks at 30fps).
				videoTS += h264ClockRate / 30
				packets := packetizeNAL(frame.Data, videoSeq, videoTS, 1, 96)
				for _, pkt := range packets {
					raw, err := pkt.Marshal()
					if err != nil {
						continue
					}
					if _, err := ws.videoTrack.Write(raw); err != nil {
						return
					}
					videoSeq++
				}
			}
		}
	}
}

// packetizeNAL wraps an H.264 NAL unit into one or more RTP packets.
func packetizeNAL(nal []byte, startSeq uint16, ts uint32, ssrc uint32, pt uint8) []*rtp.Packet {
	if len(nal) == 0 {
		return nil
	}

	if len(nal) <= maxNALSize {
		return []*rtp.Packet{{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: startSeq,
				Timestamp:      ts,
				SSRC:           ssrc,
				Marker:         true,
			},
			Payload: nal,
		}}
	}

	// FU-A fragmentation (RFC 6184 Section 5.8).
	nalHeader := nal[0]
	nalType := nalHeader & 0x1F
	nri := nalHeader & 0x60

	payload := nal[1:]
	var packets []*rtp.Packet
	seq := startSeq
	first := true

	for len(payload) > 0 {
		chunkSize := maxNALSize - 2
		if chunkSize > len(payload) {
			chunkSize = len(payload)
		}

		fuIndicator := nri | 28
		fuHeader := nalType
		if first {
			fuHeader |= 0x80
			first = false
		}
		if chunkSize == len(payload) {
			fuHeader |= 0x40
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
