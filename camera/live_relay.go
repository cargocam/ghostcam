package main

import (
	"io"
	"sync"
)

// NALUnit represents a single H.264 Network Abstraction Layer unit
// extracted from an Annex B bytestream.
type NALUnit struct {
	Data       []byte
	IsKeyframe bool // true for IDR NAL units (type 5)
}

// LiveRelay accepts raw H.264 Annex B bytes via the io.Writer interface,
// parses NAL unit boundaries, and makes them available to consumers via
// a channel. It uses a ring buffer so a slow consumer (e.g. a stalled
// WebSocket) never blocks the capture pipeline.
type LiveRelay struct {
	mu   sync.Mutex
	buf  []byte   // accumulates bytes between start codes
	ring chan NALUnit
}

// NewLiveRelay creates a relay with the given ring buffer capacity.
// A capacity of ~60 NAL units covers ~2 seconds at 30fps, which is
// enough for the WebSocket sender to keep up under normal conditions.
func NewLiveRelay(ringSize int) *LiveRelay {
	return &LiveRelay{
		ring: make(chan NALUnit, ringSize),
	}
}

// C returns the channel from which consumers read parsed NAL units.
func (lr *LiveRelay) C() <-chan NALUnit {
	return lr.ring
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
		// Find first start code in buffer.
		start := findStartCode(lr.buf, 0)
		if start < 0 {
			if final && len(lr.buf) > 0 {
				lr.emit(lr.buf)
				lr.buf = nil
			}
			return
		}

		// Discard bytes before the first start code (shouldn't happen in
		// a clean stream, but be defensive).
		if start > 0 {
			lr.buf = lr.buf[start:]
			start = 0
		}

		// Skip past the start code to find the NAL data.
		scLen := startCodeLen(lr.buf)

		// Find the next start code — that marks the end of this NAL.
		end := findStartCode(lr.buf, scLen)
		if end < 0 {
			if final {
				lr.emit(lr.buf[scLen:])
				lr.buf = nil
			}
			return
		}

		// Emit the NAL unit (without the start code prefix).
		lr.emit(lr.buf[scLen:end])
		lr.buf = lr.buf[end:]
	}
}

// emit sends a NAL unit to the ring channel. If the channel is full,
// the oldest entry is dropped to make room — we never block the
// capture pipeline.
func (lr *LiveRelay) emit(data []byte) {
	if len(data) == 0 {
		return
	}

	nal := NALUnit{
		Data:       append([]byte(nil), data...), // copy — buffer will be reused
		IsKeyframe: isIDR(data[0]),
	}

	select {
	case lr.ring <- nal:
	default:
		// Ring full — drop oldest to make room.
		select {
		case <-lr.ring:
		default:
		}
		select {
		case lr.ring <- nal:
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
				return i // 3-byte start code
			}
			if i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1 {
				return i // 4-byte start code
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
// after the start code) is an IDR slice (type 5). The NAL unit type
// is the low 5 bits of the first byte.
func isIDR(firstByte byte) bool {
	nalType := firstByte & 0x1F
	return nalType == 5
}

// NullLiveRelay is an io.Writer that discards everything. Used when
// the live relay is disabled (no server WebSocket configured).
type NullLiveRelay struct{}

func (NullLiveRelay) Write(p []byte) (int, error) { return len(p), nil }
func (NullLiveRelay) Close() error                { return nil }

// LiveWriter is the interface satisfied by both LiveRelay and
// NullLiveRelay, allowing the capture pipeline to tee without caring
// whether live streaming is active.
type LiveWriter interface {
	io.Writer
	Close() error
}
