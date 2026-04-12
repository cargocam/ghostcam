package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// liveWSMsg is a JSON control message on the camera↔server WebSocket.
type liveWSMsg struct {
	Type string `json:"type"`
}

// RunLiveRelay maintains a persistent WebSocket to the server and
// streams H.264 NAL units when a viewer is watching. It reconnects
// with exponential backoff on failure. The WebSocket is purely
// additive — the camera works fine without it (live viewers just
// fall back to HLS).
func RunLiveRelay(ctx context.Context, client *Client, relay *LiveRelay) {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		err := runLiveWSSession(ctx, client, relay)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("live relay WebSocket error", "err", err)
		}

		slog.Info("live relay reconnecting", "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runLiveWSSession(ctx context.Context, client *Client, relay *LiveRelay) error {
	wsURL := buildWSURL(client.serverURL, client.deviceID)
	slog.Info("live relay connecting", "url", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+client.apiKey)

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	// Send ready message.
	readyMsg, _ := json.Marshal(liveWSMsg{Type: "ready"})
	if err := conn.Write(ctx, websocket.MessageText, readyMsg); err != nil {
		return err
	}

	slog.Info("live relay connected")

	// Read control messages from server in a goroutine.
	var streaming atomic.Bool
	controlCtx, controlCancel := context.WithCancel(ctx)
	defer controlCancel()

	go func() {
		defer controlCancel()
		for {
			_, data, err := conn.Read(controlCtx)
			if err != nil {
				return
			}
			var msg liveWSMsg
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "start_stream":
				slog.Info("live relay: viewer connected, starting stream")
				streaming.Store(true)
			case "stop_stream":
				slog.Info("live relay: no viewers, stopping stream")
				streaming.Store(false)
			}
		}
	}()

	// Main loop: forward NAL units when streaming is active.
	for {
		select {
		case <-controlCtx.Done():
			conn.Close(websocket.StatusNormalClosure, "closing")
			return controlCtx.Err()
		case nal, ok := <-relay.C():
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "relay closed")
				return nil
			}
			if !streaming.Load() {
				continue // discard frames when no viewer is watching
			}
			if err := sendNAL(ctx, conn, nal); err != nil {
				return err
			}
		}
	}
}

// sendNAL writes a binary WebSocket frame with the wire format:
//
//	[4 bytes big-endian timestamp_ms] [1 byte flags] [NAL data]
//
// The timestamp is milliseconds since an arbitrary epoch (we use
// time.Now); the server doesn't need absolute time, just ordering.
// Flag bit 0 = is_keyframe.
func sendNAL(ctx context.Context, conn *websocket.Conn, nal NALUnit) error {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[0:4], uint32(time.Now().UnixMilli()&0xFFFFFFFF))
	if nal.IsKeyframe {
		header[4] = 0x01
	}

	msg := make([]byte, 0, len(header)+len(nal.Data))
	msg = append(msg, header...)
	msg = append(msg, nal.Data...)

	return conn.Write(ctx, websocket.MessageBinary, msg)
}

// buildWSURL converts an HTTP server URL to a WebSocket URL for the
// live relay endpoint.
func buildWSURL(serverURL, deviceID string) string {
	base := strings.TrimRight(serverURL, "/")
	// http:// → ws://, https:// → wss://
	if strings.HasPrefix(base, "https://") {
		base = "wss://" + base[8:]
	} else if strings.HasPrefix(base, "http://") {
		base = "ws://" + base[7:]
	}
	return base + "/api/v1/cameras/" + deviceID + "/live"
}
