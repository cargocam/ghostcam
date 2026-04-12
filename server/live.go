package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"nhooyr.io/websocket"
)

// LiveManager tracks per-camera live streaming sessions. Each camera
// that connects via WebSocket gets a LiveSession; each viewer that
// requests WHEP subscribes to that session's NAL fan-out.
type LiveManager struct {
	mu       sync.Mutex
	sessions map[string]*LiveSession // deviceID → session
}

// NewLiveManager creates a manager with no active sessions.
func NewLiveManager() *LiveManager {
	return &LiveManager{
		sessions: make(map[string]*LiveSession),
	}
}

// NALFrame is an H.264 NAL unit received from a camera.
type NALFrame struct {
	Data       []byte
	IsKeyframe bool
	TimestampMs uint32
}

// LiveSession holds state for one camera's live stream.
type LiveSession struct {
	mu         sync.Mutex
	deviceID   string
	cameraConn *websocket.Conn // camera's ingest WebSocket

	// Ring buffer: latest keyframe + subsequent frames. Viewers joining
	// mid-stream receive the buffered keyframe so they can decode
	// immediately without waiting for the next GOP.
	keyframe *NALFrame   // latest IDR + preceding SPS/PPS
	sps      []byte      // latest SPS NAL
	pps      []byte      // latest PPS NAL
	ring     []NALFrame  // recent non-keyframe NALs after the last keyframe
	ringCap  int

	// Fan-out: registered viewer channels.
	viewers map[uint64]chan NALFrame
	nextID  uint64
}

func newLiveSession(deviceID string, conn *websocket.Conn) *LiveSession {
	return &LiveSession{
		deviceID:   deviceID,
		cameraConn: conn,
		ringCap:    90, // ~3 seconds at 30fps
		viewers:    make(map[uint64]chan NALFrame),
	}
}

// subscribe registers a viewer and returns a channel + ID. The channel
// receives NAL frames as they arrive from the camera. Call unsubscribe
// with the returned ID when done.
func (ls *LiveSession) subscribe() (uint64, <-chan NALFrame) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	id := ls.nextID
	ls.nextID++
	ch := make(chan NALFrame, 120) // buffered so slow viewers don't block
	ls.viewers[id] = ch

	// Send buffered parameter sets + keyframe so the viewer can start
	// decoding immediately.
	if ls.sps != nil {
		select {
		case ch <- NALFrame{Data: ls.sps}:
		default:
		}
	}
	if ls.pps != nil {
		select {
		case ch <- NALFrame{Data: ls.pps}:
		default:
		}
	}
	if ls.keyframe != nil {
		select {
		case ch <- *ls.keyframe:
		default:
		}
	}

	return id, ch
}

func (ls *LiveSession) unsubscribe(id uint64) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ch, ok := ls.viewers[id]; ok {
		close(ch)
		delete(ls.viewers, id)
	}
}

func (ls *LiveSession) viewerCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.viewers)
}

// push delivers a NAL frame to all subscribed viewers.
func (ls *LiveSession) push(frame NALFrame) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	nalType := nalUnitType(frame.Data)

	// Track SPS/PPS parameter sets — sent to new viewers on subscribe.
	switch nalType {
	case 7: // SPS
		ls.sps = append([]byte(nil), frame.Data...)
		return // don't fan-out SPS by itself, it's sent with keyframe
	case 8: // PPS
		ls.pps = append([]byte(nil), frame.Data...)
		return
	case 5: // IDR
		frame.IsKeyframe = true
		ls.keyframe = &frame
		ls.ring = ls.ring[:0] // reset ring on keyframe
	default:
		if len(ls.ring) < ls.ringCap {
			ls.ring = append(ls.ring, frame)
		}
	}

	for id, ch := range ls.viewers {
		select {
		case ch <- frame:
		default:
			// Viewer too slow — skip frame rather than blocking the
			// camera ingest. The viewer's WebRTC jitter buffer will
			// handle the gap, or they'll see a brief glitch until the
			// next keyframe.
			slog.Debug("live: viewer too slow, dropping frame", "viewer_id", id, "device_id", ls.deviceID)
		}
	}
}

// nalUnitType extracts the NAL unit type from the first byte.
func nalUnitType(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0] & 0x1F
}

// GetSession returns the live session for a device, or nil if no camera
// is connected.
func (lm *LiveManager) GetSession(deviceID string) *LiveSession {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.sessions[deviceID]
}

// liveWSMsg is a JSON control message on the camera↔server WebSocket.
type liveWSMsg struct {
	Type string `json:"type"`
}

// CameraLiveWS handles GET /api/v1/cameras/{deviceID}/live — upgrades
// to a WebSocket for receiving the camera's live H.264 stream.
func (a *App) CameraLiveWS(w http.ResponseWriter, r *http.Request) {
	deviceID := getCameraDeviceID(r)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Camera connects from Docker/LAN — accept any origin.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("live: WebSocket accept failed", "device_id", deviceID, "error", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	// Wait for "ready" message from camera.
	_, data, err := conn.Read(ctx)
	if err != nil {
		slog.Warn("live: waiting for ready", "device_id", deviceID, "error", err)
		return
	}
	var readyMsg liveWSMsg
	if json.Unmarshal(data, &readyMsg) != nil || readyMsg.Type != "ready" {
		slog.Warn("live: expected ready message", "device_id", deviceID)
		return
	}

	session := newLiveSession(deviceID, conn)

	a.Live.mu.Lock()
	// Close existing session for this device if any.
	if old, ok := a.Live.sessions[deviceID]; ok {
		old.cameraConn.Close(websocket.StatusGoingAway, "replaced")
	}
	a.Live.sessions[deviceID] = session
	a.Live.mu.Unlock()

	slog.Info("live: camera connected", "device_id", deviceID)

	defer func() {
		a.Live.mu.Lock()
		if a.Live.sessions[deviceID] == session {
			delete(a.Live.sessions, deviceID)
		}
		a.Live.mu.Unlock()

		// Close all viewer channels.
		session.mu.Lock()
		for id, ch := range session.viewers {
			close(ch)
			delete(session.viewers, id)
		}
		session.mu.Unlock()

		slog.Info("live: camera disconnected", "device_id", deviceID)
	}()

	// Read binary frames (H.264 NALs) from the camera.
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		if msgType == websocket.MessageText {
			// Text frame = control message (shouldn't normally come from
			// camera after ready, but ignore gracefully).
			continue
		}

		// Binary frame: [4 bytes timestamp_ms] [1 byte flags] [NAL data]
		if len(data) < 6 {
			continue
		}

		tsMs := binary.BigEndian.Uint32(data[0:4])
		flags := data[4]
		nalData := data[5:]

		frame := NALFrame{
			Data:        nalData,
			IsKeyframe:  flags&0x01 != 0,
			TimestampMs: tsMs,
		}

		session.push(frame)
	}
}

// sendStreamControl sends a start_stream or stop_stream message to the
// camera. Uses the request context (or a background context for cleanup).
func sendStreamControl(ctx context.Context, conn *websocket.Conn, msgType string) error {
	msg, _ := json.Marshal(liveWSMsg{Type: msgType})
	return conn.Write(ctx, websocket.MessageText, msg)
}
