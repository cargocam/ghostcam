package main

import (
	"fmt"
	"testing"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
)

func TestCameraLimitAllowed(t *testing.T) {
	// Mirrors the presign handler's camera limit check logic:
	// Given a list of cameras ordered by enrolled_at and a tier limit,
	// only the N oldest cameras are allowed to upload.
	tests := []struct {
		name      string
		cameras   []string // device IDs in enrolled_at order
		tierID    string
		deviceID  string
		wantAllow bool
	}{
		{
			name:      "free tier, 1 camera, first camera allowed",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tierID:    "free",
			deviceID:  "cam-1",
			wantAllow: true,
		},
		{
			name:      "free tier, 1 camera, second camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tierID:    "free",
			deviceID:  "cam-2",
			wantAllow: false,
		},
		{
			name:      "free tier, 1 camera, third camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tierID:    "free",
			deviceID:  "cam-3",
			wantAllow: false,
		},
		{
			name:      "starter tier, 4 cameras, all within limit",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tierID:    "starter",
			deviceID:  "cam-3",
			wantAllow: true,
		},
		{
			name:      "starter tier, 5th camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3", "cam-4", "cam-5"},
			tierID:    "starter",
			deviceID:  "cam-5",
			wantAllow: false,
		},
		{
			name:      "enterprise tier, unlimited cameras",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tierID:    "enterprise",
			deviceID:  "cam-3",
			wantAllow: true,
		},
		{
			name:      "pro tier, 16 cameras, last allowed",
			cameras:   make16Cameras(),
			tierID:    "pro",
			deviceID:  "cam-16",
			wantAllow: true,
		},
		{
			name:      "pro tier, 17th camera blocked",
			cameras:   append(make16Cameras(), "cam-17"),
			tierID:    "pro",
			deviceID:  "cam-17",
			wantAllow: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier := billing.GetTier(tt.tierID)
			got := isCameraAllowed(tier, tt.cameras, tt.deviceID)
			if got != tt.wantAllow {
				t.Errorf("isCameraAllowed() = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

// isCameraAllowed mirrors the presign handler's camera limit logic.
func isCameraAllowed(tier billing.Tier, cameraIDs []string, deviceID string) bool {
	if tier.CameraLimit == nil {
		return true // unlimited
	}
	if len(cameraIDs) <= *tier.CameraLimit {
		return true // under limit
	}
	// Only the N oldest cameras are allowed.
	allowed := make(map[string]bool, *tier.CameraLimit)
	for i := 0; i < *tier.CameraLimit && i < len(cameraIDs); i++ {
		allowed[cameraIDs[i]] = true
	}
	return allowed[deviceID]
}

func make16Cameras() []string {
	cams := make([]string, 16)
	for i := range cams {
		cams[i] = fmt.Sprintf("cam-%d", i+1)
	}
	return cams
}

func TestEffectiveTier(t *testing.T) {
	tests := []struct {
		name             string
		sub              *db.SubscriptionRecord
		stripeConfigured bool
		want             string
	}{
		{
			name:             "no subscription, stripe configured",
			sub:              nil,
			stripeConfigured: true,
			want:             "free",
		},
		{
			name:             "no subscription, stripe not configured (dev)",
			sub:              nil,
			stripeConfigured: false,
			want:             "enterprise",
		},
		{
			name:             "free tier with stripe",
			sub:              &db.SubscriptionRecord{Tier: "free", Status: "active"},
			stripeConfigured: true,
			want:             "free",
		},
		{
			name:             "pro tier without stripe subscription ID",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "active", StripeSubscriptionID: nil},
			stripeConfigured: true,
			want:             "free", // no stripe sub = forced free
		},
		{
			name:             "pro tier with active stripe subscription",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "active", StripeSubscriptionID: strPtr("sub_123")},
			stripeConfigured: true,
			want:             "pro",
		},
		{
			name:             "pro tier with canceled stripe subscription",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "canceled", StripeSubscriptionID: strPtr("sub_123")},
			stripeConfigured: true,
			want:             "free", // canceled = forced free
		},
		{
			name:             "enterprise with active stripe",
			sub:              &db.SubscriptionRecord{Tier: "enterprise", Status: "active", StripeSubscriptionID: strPtr("sub_456")},
			stripeConfigured: true,
			want:             "enterprise",
		},
		{
			name:             "any tier without stripe configured = enterprise (dev mode)",
			sub:              &db.SubscriptionRecord{Tier: "free", Status: "active"},
			stripeConfigured: false,
			want:             "enterprise",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveTier(tt.sub, tt.stripeConfigured)
			if got != tt.want {
				t.Errorf("effectiveTier() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEpochMsToISO8601(t *testing.T) {
	tests := []struct {
		epochMs uint64
		want    string
	}{
		{0, "1970-01-01T00:00:00.000Z"},
		{1775401947875, "2026-04-05T15:12:27.875Z"},
		{1775538068000, "2026-04-07T05:01:08.000Z"},
	}

	for _, tt := range tests {
		got := epochMsToISO8601(tt.epochMs)
		if got != tt.want {
			t.Errorf("epochMsToISO8601(%d) = %q, want %q", tt.epochMs, got, tt.want)
		}
	}
}
