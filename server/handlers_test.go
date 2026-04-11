package main

import (
	"fmt"
	"testing"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
)

// buildTier returns a Tier struct with the given camera limit for tests.
// A nil cameraLimit means unlimited.
func buildTier(id string, cameraLimit *int) billing.Tier {
	return billing.Tier{ID: id, Name: id, CameraLimit: cameraLimit}
}

func intPtr(v int) *int { return &v }

func TestCameraLimitAllowed(t *testing.T) {
	// Mirrors the presign handler's camera limit check logic:
	// Given a list of cameras ordered by enrolled_at and a tier limit,
	// only the N oldest cameras are allowed to upload.
	free := billing.FreeTier
	starter := buildTier("starter", intPtr(4))
	pro := buildTier("pro", intPtr(16))
	enterprise := buildTier("enterprise", nil)

	tests := []struct {
		name      string
		cameras   []string // device IDs in enrolled_at order
		tier      billing.Tier
		deviceID  string
		wantAllow bool
	}{
		{
			name:      "free tier, 1 camera, first camera allowed",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tier:      free,
			deviceID:  "cam-1",
			wantAllow: true,
		},
		{
			name:      "free tier, 1 camera, second camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tier:      free,
			deviceID:  "cam-2",
			wantAllow: false,
		},
		{
			name:      "free tier, 1 camera, third camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tier:      free,
			deviceID:  "cam-3",
			wantAllow: false,
		},
		{
			name:      "starter tier, 4 cameras, all within limit",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tier:      starter,
			deviceID:  "cam-3",
			wantAllow: true,
		},
		{
			name:      "starter tier, 5th camera blocked",
			cameras:   []string{"cam-1", "cam-2", "cam-3", "cam-4", "cam-5"},
			tier:      starter,
			deviceID:  "cam-5",
			wantAllow: false,
		},
		{
			name:      "enterprise tier, unlimited cameras",
			cameras:   []string{"cam-1", "cam-2", "cam-3"},
			tier:      enterprise,
			deviceID:  "cam-3",
			wantAllow: true,
		},
		{
			name:      "pro tier, 16 cameras, last allowed",
			cameras:   make16Cameras(),
			tier:      pro,
			deviceID:  "cam-16",
			wantAllow: true,
		},
		{
			name:      "pro tier, 17th camera blocked",
			cameras:   append(make16Cameras(), "cam-17"),
			tier:      pro,
			deviceID:  "cam-17",
			wantAllow: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCameraAllowed(tt.tier, tt.cameras, tt.deviceID)
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

// TestResolveEffectiveTier covers the full decision matrix of the pure
// tier-resolution function. The interesting security property is
// fail-closed: any unrecognised tier string must collapse to free rather
// than escalate the user to an unlimited paid tier.
func TestResolveEffectiveTier(t *testing.T) {
	// Empty cache — any paid tier lookup misses. Exercises the fail-closed
	// path plus the legacy name grandfathering path baked into Cache.Get.
	cache := billing.NewCache()

	tests := []struct {
		name             string
		sub              *db.SubscriptionRecord
		stripeConfigured bool
		wantID           string
	}{
		{
			name:             "no subscription, stripe configured",
			sub:              nil,
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "no subscription, stripe not configured (dev)",
			sub:              nil,
			stripeConfigured: false,
			wantID:           "dev-unlimited",
		},
		{
			name:             "free tier with stripe",
			sub:              &db.SubscriptionRecord{Tier: billing.FreeTierID, Status: "active"},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "paid tier without stripe subscription ID (grandfathered name)",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "active", StripeSubscriptionID: nil},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			// Legacy name + active subscription: the pure function
			// no longer resolves legacy names (Stripe is now the only
			// source of truth for paid tier limits). The fail-closed
			// fallback is free. The App.effectiveTier wrapper runs a
			// one-shot migration that fetches the live Stripe price ID
			// and rewrites the DB — after that migration, the sub row
			// carries a price ID and resolveEffectiveTier's second
			// pass hits the cache. That migration path is covered by
			// the integration test, not here.
			name:             "legacy pro tier with active stripe — resolves to free (lazy migration required)",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "active", StripeSubscriptionID: strPtr("sub_123")},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "legacy pro tier with canceled stripe subscription",
			sub:              &db.SubscriptionRecord{Tier: "pro", Status: "canceled", StripeSubscriptionID: strPtr("sub_123")},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "legacy enterprise with active stripe — resolves to free (lazy migration required)",
			sub:              &db.SubscriptionRecord{Tier: "enterprise", Status: "active", StripeSubscriptionID: strPtr("sub_456")},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "any tier without stripe configured = dev-unlimited",
			sub:              &db.SubscriptionRecord{Tier: billing.FreeTierID, Status: "active"},
			stripeConfigured: false,
			wantID:           "dev-unlimited",
		},
		{
			// SECURITY: unknown tier strings in the DB must not grant
			// unlimited resources via a fall-through to a paid tier. The
			// safest default is the most restrictive tier (free).
			name:             "unknown tier string falls back to free (fail-closed)",
			sub:              &db.SubscriptionRecord{Tier: "godmode", Status: "active", StripeSubscriptionID: strPtr("sub_999")},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
		{
			name:             "empty tier string falls back to free (fail-closed)",
			sub:              &db.SubscriptionRecord{Tier: "", Status: "active", StripeSubscriptionID: strPtr("sub_000")},
			stripeConfigured: true,
			wantID:           billing.FreeTierID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEffectiveTier(tt.sub, tt.stripeConfigured, cache)
			if got.ID != tt.wantID {
				t.Errorf("resolveEffectiveTier().ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}
