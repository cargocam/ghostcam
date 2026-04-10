package billing

import "testing"

func TestGetTier(t *testing.T) {
	tests := []struct {
		id         string
		wantOk     bool
		wantName   string
		wantCamLim *int
		wantGB     *int
	}{
		{"free", true, "Free", intPtr(1), intPtr(5)},
		{"starter", true, "Starter", intPtr(4), intPtr(50)},
		{"pro", true, "Pro", intPtr(16), intPtr(500)},
		{"enterprise", true, "Enterprise", nil, nil},
		{"nonexistent", false, "", nil, nil},
		{"", false, "", nil, nil},
		{"unlimited", false, "", nil, nil}, // old fallback name — must not resolve
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			tier, ok := GetTier(tt.id)
			if ok != tt.wantOk {
				t.Fatalf("GetTier(%q) ok = %v, want %v", tt.id, ok, tt.wantOk)
			}
			if !ok {
				return
			}
			if tier.Name != tt.wantName {
				t.Errorf("GetTier(%q).Name = %q, want %q", tt.id, tier.Name, tt.wantName)
			}
			if tt.wantCamLim == nil && tier.CameraLimit != nil {
				t.Errorf("GetTier(%q).CameraLimit should be nil", tt.id)
			}
			if tt.wantCamLim != nil && (tier.CameraLimit == nil || *tier.CameraLimit != *tt.wantCamLim) {
				t.Errorf("GetTier(%q).CameraLimit = %v, want %d", tt.id, tier.CameraLimit, *tt.wantCamLim)
			}
		})
	}
}

func TestStorageLimitBytes(t *testing.T) {
	free, _ := GetTier("free")
	if free.StorageLimitBytes() != 5*1024*1024*1024 {
		t.Errorf("free tier storage = %d, want %d", free.StorageLimitBytes(), 5*1024*1024*1024)
	}

	enterprise, _ := GetTier("enterprise")
	if enterprise.StorageLimitBytes() != 0 {
		t.Errorf("enterprise storage should be 0 (unlimited), got %d", enterprise.StorageLimitBytes())
	}
}
