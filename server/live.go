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
// requests WHEP subscribes to that session's media fan-out.
type LiveManager struct {
	mu       sync.Mutex
	sessions map[string]*LiveSession // deviceID → session
}

func NewLiveManager() *LiveManager {
	return &LiveManager{
		sessions: make(map[string]*LiveSession),
	}
}

// MediaFrame is a video or audio frame received from a camera.
type MediaFrame struct {
	Data        []byte
	IsKeyframe  bool   // video only: true for IDR NAL units
	IsAudio     bool   // true = Opus audio, false = H.264 video
	TimestampMs uint32
}

// LiveSession holds state for one camera's live stream.
type LiveSession struct {
	mu         sync.Mutex
	deviceID   string
	cameraConn *websocket.Conn

	// Video state: latest parameter sets + keyframe for viewer sync.
	sps      []byte
	pps      []byte
	keyframe *MediaFrame
	ring     []MediaFrame // recent frames after keyframe
	ringCap  int

	// Fan-out: registered viewer channels.
	viewers map[uint64]chan MediaFrame
	nextID  uint64
}

func newLiveSession(deviceID string, conn *websocket.Conn) *LiveSession {
	return &LiveSession{
		deviceID:   deviceID,
		cameraConn: conn,
		ringCap:    90,
		viewers:    make(map[uint64]chan MediaFrame),
	}
}

// subscribe registers a viewer and returns a channel + ID. The viewer
// receives the buffered keyframe (with SPS/PPS) immediately so it can
// start decoding without waiting for the next GOP.
func (ls *LiveSession) subscribe() (uint64, <-chan MediaFrame) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	id := ls.nextID
	ls.nextID++
	ch := make(chan MediaFrame, 120)
	ls.viewers[id] = ch

	// Send buffered parameter sets + keyframe.
	if ls.sps != nil {
		select {
		case ch <- MediaFrame{Data: ls.sps}:
		default:
		}
	}
	if ls.pps != nil {
		select {
		case ch <- MediaFrame{Data: ls.pps}:
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

// push delivers a media frame to all subscribed viewers.
func (ls *LiveSession) push(frame MediaFrame) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if !frame.IsAudio {
		nalType := nalUnitType(frame.Data)
		switch nalType {
		case 7: // SPS
			ls.sps = append([]byte(nil), frame.Data...)
			return // sent to new viewers on subscribe, not fanned out
		case 8: // PPS
			ls.pps = append([]byte(nil), frame.Data...)
			return
		case 5: // IDR
			frame.IsKeyframe = true
			ls.keyframe = &frame
			ls.ring = ls.ring[:0]
		default:
			if len(ls.ring) < ls.ringCap {
				ls.ring = append(ls.ring, frame)
			}
		}
	}

	for id, ch := range ls.viewers {
		select {
		case ch <- frame:
		default:
			slog.Debug("live: viewer too slow, dropping frame",
				"viewer_id", id, "device_id", ls.deviceID, "audio", frame.IsAudio)
		}
	}
}

func nalUnitType(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0] & 0x1F
}

// GetSession returns the live session for a device, or nil.
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
// to a WebSocket for receiving the camera's live H.264 + Opus stream.
func (a *App) CameraLiveWS(w http.ResponseWriter, r *http.Request) {
	deviceID := getCameraDeviceID(r)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
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

		session.mu.Lock()
		for id, ch := range session.viewers {
			close(ch)
			delete(session.viewers, id)
		}
		session.mu.Unlock()

		slog.Info("live: camera disconnected", "device_id", deviceID)
	}()

	// Read binary frames from the camera.
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		if msgType == websocket.MessageText {
			continue
		}

		// Binary frame: [4B timestamp_ms] [1B flags] [payload]
		// flags bit 0: is_keyframe, bit 1: is_audio
		if len(data) < 6 {
			continue
		}

		tsMs := binary.BigEndian.Uint32(data[0:4])
		flags := data[4]
		payload := data[5:]

		frame := MediaFrame{
			Data:        payload,
			IsKeyframe:  flags&0x01 != 0,
			IsAudio:     flags&0x02 != 0,
			TimestampMs: tsMs,
		}

		session.push(frame)
	}
}

// sendStreamControl sends a start_stream or stop_stream message to the camera.
func sendStreamControl(ctx context.Context, conn *websocket.Conn, msgType string) error {
	msg, _ := json.Marshal(liveWSMsg{Type: msgType})
	return conn.Write(ctx, websocket.MessageText, msg)
}
