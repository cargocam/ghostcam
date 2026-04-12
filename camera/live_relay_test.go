package main

import (
	"testing"
)

func TestFindStartCode(t *testing.T) {
	tests := []struct {
		name   string
		buf    []byte
		offset int
		want   int
	}{
		{"3-byte at start", []byte{0, 0, 1, 0x65}, 0, 0},
		{"4-byte at start", []byte{0, 0, 0, 1, 0x65}, 0, 0},
		{"3-byte with offset", []byte{0xFF, 0, 0, 1, 0x65}, 0, 1},
		{"4-byte with offset", []byte{0xFF, 0, 0, 0, 1, 0x65}, 0, 1},
		{"none found", []byte{0xFF, 0xFF, 0xFF}, 0, -1},
		{"skip past offset", []byte{0, 0, 1, 0x65, 0, 0, 1, 0x41}, 3, 4},
		{"too short", []byte{0, 0}, 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findStartCode(tt.buf, tt.offset)
			if got != tt.want {
				t.Errorf("findStartCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsIDR(t *testing.T) {
	tests := []struct {
		name      string
		firstByte byte
		want      bool
	}{
		{"IDR slice", 0x65, true},
		{"IDR slice alt", 0x25, true},
		{"non-IDR slice", 0x41, false},
		{"SPS", 0x67, false},
		{"PPS", 0x68, false},
		{"SEI", 0x06, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIDR(tt.firstByte)
			if got != tt.want {
				t.Errorf("isIDR(0x%02x) = %v, want %v", tt.firstByte, got, tt.want)
			}
		})
	}
}

func TestLiveRelayParsesNALs(t *testing.T) {
	lr := NewLiveRelay(16)

	// Write an Annex B stream with 3 NAL units:
	// SPS (type 7), PPS (type 8), IDR (type 5)
	stream := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0x00, 0x1E, // SPS
		0, 0, 0, 1, 0x68, 0xCE, 0x38, 0x80, // PPS
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, // IDR
	}

	if _, err := lr.Write(stream); err != nil {
		t.Fatal(err)
	}

	// Should have 2 frames so far (SPS and PPS); IDR is still buffered.
	got := drainAvailable(lr)
	if len(got) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(got))
	}
	if got[0].IsKeyframe {
		t.Error("SPS should not be keyframe")
	}
	if got[0].IsAudio {
		t.Error("video frame should not be audio")
	}

	// Close flushes the trailing IDR.
	lr.Close()
	got = drainAvailable(lr)
	if len(got) != 1 {
		t.Fatalf("expected 1 frame after close, got %d", len(got))
	}
	if !got[0].IsKeyframe {
		t.Error("last frame should be IDR keyframe")
	}
}

func TestLiveRelayAudio(t *testing.T) {
	lr := NewLiveRelay(16)

	lr.PushAudio([]byte{0x01, 0x02, 0x03})
	lr.PushAudio([]byte{0x04, 0x05, 0x06})

	got := drainAvailable(lr)
	if len(got) != 2 {
		t.Fatalf("expected 2 audio frames, got %d", len(got))
	}
	if !got[0].IsAudio {
		t.Error("frame should be audio")
	}
	if got[0].IsKeyframe {
		t.Error("audio frame should not be keyframe")
	}
}

func TestLiveRelayRingOverflow(t *testing.T) {
	lr := NewLiveRelay(2)

	for i := 0; i < 4; i++ {
		stream := []byte{0, 0, 1, 0x41, byte(i)}
		lr.Write(stream)
	}
	lr.Close()

	got := drainAvailable(lr)
	if len(got) > 2 {
		t.Errorf("expected at most 2 frames from ring of size 2, got %d", len(got))
	}
}

func drainAvailable(lr *LiveRelay) []LiveFrame {
	var out []LiveFrame
	for {
		select {
		case frame, ok := <-lr.ring:
			if !ok {
				return out
			}
			out = append(out, frame)
		default:
			return out
		}
	}
}
