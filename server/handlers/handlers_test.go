package handlers

import (
	"testing"

	"github.com/cargocam/ghostcam/server/db"
)

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
