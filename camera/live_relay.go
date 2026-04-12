package main

import (
	"io"
	"sync"
)

// LiveFrame carries a single media frame — either an H.264 NAL unit
// (video) or an Opus packet (audio). The Type field discriminates.
type LiveFrame struct {
	Data       []byte
	IsKeyframe bool // video only: true for IDR NAL units (type 5)
	IsAudio    bool // true = Opus audio, false = H.264 video
}

// LiveRelay accepts raw H.264 Annex B bytes via the io.Writer interface,
// parses NAL unit boundaries, and makes them available to consumers via
// a channel. Audio frames are pushed separately via PushAudio.
//
// The ring buffer drops oldest frames so a slow consumer never blocks
// the capture pipeline.
type LiveRelay struct {
	mu   sync.Mutex
	buf  []byte // accumulates bytes between start codes
	ring chan LiveFrame
}

// NewLiveRelay creates a relay with the given ring buffer capacity.
// A capacity of ~120 covers ~4 seconds of interleaved video+audio,
// enough for the WebSocket sender to keep up under normal conditions.
func NewLiveRelay(ringSize int) *LiveRelay {
	return &LiveRelay{
		ring: make(chan LiveFrame, ringSize),
	}
}

// C returns the channel from which consumers read parsed frames.
func (lr *LiveRelay) C() <-chan LiveFrame {
	return lr.ring
}

// PushAudio enqueues an Opus audio frame into the ring buffer.
// Called by the OGG reader goroutine, not by the io.Writer path.
func (lr *LiveRelay) PushAudio(data []byte) {
	frame := LiveFrame{
		Data:    append([]byte(nil), data...),
		IsAudio: true,
	}
	lr.enqueue(frame)
}

// Write implements io.Writer. It is called by io.MultiWriter from the
// capture pipeline tee. The bytes are raw H.264 Annex B bytestream
// (from rpicam-vid or ffmpeg).
func (lr *LiveRelay) Write(p []byte) (int, error) {
	lr.mu.Lock()
	defer lr.mu.Unlock()

	n := len(p)
	lr.buf = append(lr.buf, p...)
	lr.flush(false)
	return n, nil
}

// Close flushes any remaining buffered NAL and closes the channel.
func (lr *LiveRelay) Close() error {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.flush(true)
	close(lr.ring)
	return nil
}

// flush scans the buffer for Annex B start codes and emits complete
// NAL units. If final is true, the trailing data (after the last start
// code) is also emitted.
func (lr *LiveRelay) flush(final bool) {
	for {
		start := findStartCode(lr.buf, 0)
		if start < 0 {
			if final && len(lr.buf) > 0 {
				lr.emitVideo(lr.buf)
				lr.buf = nil
			}
			return
		}

		if start > 0 {
			lr.buf = lr.buf[start:]
		}

		scLen := startCodeLen(lr.buf)

		end := findStartCode(lr.buf, scLen)
		if end < 0 {
			if final {
				lr.emitVideo(lr.buf[scLen:])
				lr.buf = nil
			}
			return
		}

		lr.emitVideo(lr.buf[scLen:end])
		lr.buf = lr.buf[end:]
	}
}

// emitVideo sends a video NAL unit to the ring.
func (lr *LiveRelay) emitVideo(data []byte) {
	if len(data) == 0 {
		return
	}
	frame := LiveFrame{
		Data:       append([]byte(nil), data...),
		IsKeyframe: isIDR(data[0]),
	}
	lr.enqueue(frame)
}

// enqueue adds a frame to the ring, dropping the oldest if full.
func (lr *LiveRelay) enqueue(frame LiveFrame) {
	select {
	case lr.ring <- frame:
	default:
		// Ring full — drop oldest to make room.
		select {
		case <-lr.ring:
		default:
		}
		select {
		case lr.ring <- frame:
		default:
		}
	}
}

// findStartCode locates the next Annex B start code (0x000001 or
// 0x00000001) starting at offset. Returns -1 if not found.
func findStartCode(buf []byte, offset int) int {
	for i := offset; i < len(buf)-2; i++ {
		if buf[i] == 0 && buf[i+1] == 0 {
			if buf[i+2] == 1 {
				return i
			}
			if i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// startCodeLen returns 3 or 4 depending on whether the start code at
// the beginning of buf is 0x000001 or 0x00000001.
func startCodeLen(buf []byte) int {
	if len(buf) >= 4 && buf[0] == 0 && buf[1] == 0 && buf[2] == 0 && buf[3] == 1 {
		return 4
	}
	return 3
}

// isIDR returns true if the NAL unit type (encoded in the first byte
// after the start code) is an IDR slice (type 5).
func isIDR(firstByte byte) bool {
	return firstByte&0x1F == 5
}

// NullLiveRelay is an io.Writer that discards everything. Used when
// the live relay is disabled.
type NullLiveRelay struct{}

func (NullLiveRelay) Write(p []byte) (int, error) { return len(p), nil }
func (NullLiveRelay) Close() error                { return nil }
func (NullLiveRelay) PushAudio([]byte)             {}

// LiveWriter is the interface satisfied by both LiveRelay and
// NullLiveRelay, allowing the capture pipeline to tee without caring
// whether live streaming is active.
type LiveWriter interface {
	io.Writer
	Close() error
	PushAudio(data []byte)
}
