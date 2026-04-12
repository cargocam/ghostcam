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
		{"IDR slice", 0x65, true},       // nal_type=5, nal_ref_idc=3
		{"IDR slice alt", 0x25, true},    // nal_type=5, nal_ref_idc=1
		{"non-IDR slice", 0x41, false},   // nal_type=1
		{"SPS", 0x67, false},             // nal_type=7
		{"PPS", 0x68, false},             // nal_type=8
		{"SEI", 0x06, false},             // nal_type=6
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

	// Write in one go — the last NAL won't be emitted because there's
	// no subsequent start code to delimit it.
	if _, err := lr.Write(stream); err != nil {
		t.Fatal(err)
	}

	// Should have 2 NALs so far (SPS and PPS); IDR is still buffered.
	got := drainAvailable(lr)
	if len(got) != 2 {
		t.Fatalf("expected 2 NALs, got %d", len(got))
	}
	if got[0].IsKeyframe {
		t.Error("SPS should not be keyframe")
	}
	if got[1].IsKeyframe {
		t.Error("PPS should not be keyframe")
	}

	// Close flushes the trailing IDR.
	lr.Close()
	got = drainAvailable(lr)
	if len(got) != 1 {
		t.Fatalf("expected 1 NAL after close, got %d", len(got))
	}
	if !got[0].IsKeyframe {
		t.Error("last NAL should be IDR keyframe")
	}
}

func TestLiveRelayRingOverflow(t *testing.T) {
	lr := NewLiveRelay(2)

	// Write 4 NAL units to a ring of size 2 — oldest should be dropped.
	for i := 0; i < 4; i++ {
		stream := []byte{0, 0, 1, 0x41, byte(i)} // non-IDR slice
		lr.Write(stream)
	}
	// Flush trailing
	lr.Close()

	got := drainAvailable(lr)
	// We should get at most 2 (ring capacity), the most recent ones.
	if len(got) > 2 {
		t.Errorf("expected at most 2 NALs from ring of size 2, got %d", len(got))
	}
}

func drainAvailable(lr *LiveRelay) []NALUnit {
	var out []NALUnit
	for {
		select {
		case nal, ok := <-lr.ring:
			if !ok {
				return out
			}
			out = append(out, nal)
		default:
			return out
		}
	}
}
