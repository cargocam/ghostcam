package main

import "testing"

// decideHasMore signals the client whether to re-call
// DELETE /api/v1/cameras/:id/footage to continue the purge. A full
// final batch means the DB may still have matching rows; any
// non-full batch (including empty) means we reached the tail.
func TestDecideHasMore(t *testing.T) {
	tests := []struct {
		name         string
		lastBatchLen int
		batchSize    int
		want         bool
	}{
		{"empty final batch — done", 0, 100, false},
		{"partial final batch — done", 17, 100, false},
		{"full final batch — keep going", 100, 100, true},
		{"batch larger than cap (impossible, but safe) — treated as done", 150, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideHasMore(tt.lastBatchLen, tt.batchSize)
			if got != tt.want {
				t.Errorf("decideHasMore(%d, %d) = %v, want %v",
					tt.lastBatchLen, tt.batchSize, got, tt.want)
			}
		})
	}
}
