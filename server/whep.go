package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	h264ClockRate = 90000
	opusClockRate = 48000
)

// whepSession tracks one viewer's WebRTC peer connection.
type whepSession struct {
	id         string
	deviceID   string
	pc         *webrtc.PeerConnection
	videoTrack *webrtc.TrackLocalStaticSample
	audioTrack *webrtc.TrackLocalStaticSample
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
		// Standby-mode wake: park a 60 s flag in Redis. The camera's
		// next telemetry poll reads it and opens the live WS, then a
		// follow-up WHEP request will find the session ready.
		if a.Redis != nil {
			a.Redis.Set(r.Context(), wakeLiveKey(deviceID), "1", 60*time.Second)
		}
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

	// Video track: H.264 via sample-based API (pion handles RTP packetization).
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   h264ClockRate,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
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

	// Audio track: Opus via sample-based API.
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
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
		sendStreamControl(context.Background(), session.cameraConn, "start_stream")
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
// them as samples to the appropriate WebRTC track. Pion handles RTP
// packetization (FU-A fragmentation, timestamps, marker bits).
func (a *App) forwardToTrack(ws *whepSession) {
	frameDuration := time.Second / 30 // ~33ms at 30fps
	audioDuration := 20 * time.Millisecond

	for {
		select {
		case <-ws.done:
			return
		case frame, ok := <-ws.viewerChan:
			if !ok {
				return
			}

			if frame.IsAudio {
				if err := ws.audioTrack.WriteSample(media.Sample{
					Data:     frame.Data,
					Duration: audioDuration,
				}); err != nil {
					return
				}
			} else {
				if err := ws.videoTrack.WriteSample(media.Sample{
					Data:     frame.Data,
					Duration: frameDuration,
				}); err != nil {
					return
				}
			}
		}
	}
}
