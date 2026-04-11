package billing

import (
	"testing"

	"github.com/stripe/stripe-go/v82"
)

func TestCacheGet_Free(t *testing.T) {
	c := NewCache()

	// The free tier is always resolvable, even on an empty cache, because
	// it is compile-time constant rather than sourced from Stripe.
	tier, ok := c.Get(FreeTierID)
	if !ok {
		t.Fatal("Get(\"free\") should always resolve")
	}
	if tier.Name != "Free" {
		t.Errorf("Free tier Name = %q, want %q", tier.Name, "Free")
	}
	if tier.CameraLimit == nil || *tier.CameraLimit != 1 {
		t.Errorf("Free tier CameraLimit = %v, want 1", tier.CameraLimit)
	}
	if tier.StorageLimitBytes() != 5*1024*1024*1024 {
		t.Errorf("Free tier StorageLimitBytes = %d, want 5 GiB", tier.StorageLimitBytes())
	}

	// Empty string is treated as the free tier (defensive for DB rows that
	// may have NULL collapsed to "").
	if _, ok := c.Get(""); !ok {
		t.Error("Get(\"\") should resolve to free tier")
	}
}

func TestCacheGet_Unknown(t *testing.T) {
	c := NewCache()

	// Fail-closed: unknown IDs do not resolve. Callers must log and fall
	// back to free rather than silently escalating entitlements.
	if _, ok := c.Get("godmode"); ok {
		t.Error("Get(\"godmode\") should not resolve on empty cache")
	}
	if _, ok := c.Get("price_unknown"); ok {
		t.Error("Get(\"price_unknown\") should not resolve on empty cache")
	}
}

func TestCacheGet_LegacyNames(t *testing.T) {
	c := NewCache()

	// Pre-refactor DB rows can have the tier column set to one of the
	// hardcoded legacy names. The cache must resolve them via the
	// grandfathered fallback so a deploy of the refactor doesn't
	// instantly drop every existing paid customer to free.
	tests := []struct {
		id      string
		wantCam *int
	}{
		{"starter", intPtr(4)},
		{"pro", intPtr(16)},
		{"enterprise", nil},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			tier, ok := c.Get(tt.id)
			if !ok {
				t.Fatalf("Get(%q) should resolve via legacy fallback", tt.id)
			}
			if tt.wantCam == nil && tier.CameraLimit != nil {
				t.Errorf("CameraLimit should be nil (unlimited), got %v", tier.CameraLimit)
			}
			if tt.wantCam != nil && (tier.CameraLimit == nil || *tier.CameraLimit != *tt.wantCam) {
				t.Errorf("CameraLimit = %v, want %d", tier.CameraLimit, *tt.wantCam)
			}
		})
	}
}

func TestTierFromStripe(t *testing.T) {
	tests := []struct {
		name        string
		product     *stripe.Product
		price       *stripe.Price
		wantOk      bool
		wantCam     *int
		wantStorage *int
	}{
		{
			name: "both limits as integers",
			product: &stripe.Product{
				ID: "prod_A", Name: "Starter",
				Metadata: map[string]string{
					"ghostcam_camera_limit": "4",
					"ghostcam_storage_gb":   "50",
				},
			},
			price:       &stripe.Price{ID: "price_A", UnitAmount: 500, Currency: "usd"},
			wantOk:      true,
			wantCam:     intPtr(4),
			wantStorage: intPtr(50),
		},
		{
			name: "unlimited literal",
			product: &stripe.Product{
				ID: "prod_B", Name: "Enterprise",
				Metadata: map[string]string{
					"ghostcam_camera_limit": "unlimited",
					"ghostcam_storage_gb":   "unlimited",
				},
			},
			price:       &stripe.Price{ID: "price_B", UnitAmount: 9900, Currency: "usd"},
			wantOk:      true,
			wantCam:     nil,
			wantStorage: nil,
		},
		{
			name: "only one metadata key present is accepted",
			product: &stripe.Product{
				ID: "prod_C", Name: "Storage-only",
				Metadata: map[string]string{
					"ghostcam_storage_gb": "100",
				},
			},
			price:       &stripe.Price{ID: "price_C", UnitAmount: 1000, Currency: "usd"},
			wantOk:      true,
			wantCam:     nil, // missing → unlimited (since no metadata key means not set)
			wantStorage: intPtr(100),
		},
		{
			name: "no ghostcam metadata at all is rejected",
			product: &stripe.Product{
				ID: "prod_D", Name: "Unrelated",
				Metadata: map[string]string{"other": "value"},
			},
			price:  &stripe.Price{ID: "price_D", UnitAmount: 1000, Currency: "usd"},
			wantOk: false,
		},
		{
			name: "invalid int metadata is rejected as missing",
			product: &stripe.Product{
				ID: "prod_E", Name: "Broken",
				Metadata: map[string]string{
					"ghostcam_camera_limit": "five",
					"ghostcam_storage_gb":   "lots",
				},
			},
			price:  &stripe.Price{ID: "price_E", UnitAmount: 1000, Currency: "usd"},
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier, ok := tierFromStripe(tt.price, tt.product)
			if ok != tt.wantOk {
				t.Fatalf("tierFromStripe ok = %v, want %v", ok, tt.wantOk)
			}
			if !ok {
				return
			}
			if (tt.wantCam == nil) != (tier.CameraLimit == nil) {
				t.Errorf("CameraLimit nilness mismatch: got %v want %v", tier.CameraLimit, tt.wantCam)
			}
			if tt.wantCam != nil && tier.CameraLimit != nil && *tier.CameraLimit != *tt.wantCam {
				t.Errorf("CameraLimit = %d, want %d", *tier.CameraLimit, *tt.wantCam)
			}
			if (tt.wantStorage == nil) != (tier.StorageLimitGB == nil) {
				t.Errorf("StorageLimitGB nilness mismatch: got %v want %v", tier.StorageLimitGB, tt.wantStorage)
			}
		})
	}
}

func TestParseLimit(t *testing.T) {
	tests := []struct {
		raw     string
		wantPtr *int
		wantOK  bool
	}{
		{"", nil, false},
		{"5", intPtr(5), true},
		{"0", intPtr(0), true},
		{"-1", nil, true}, // documented "unlimited" sentinel
		{"unlimited", nil, true},
		{"UNLIMITED", nil, true},
		{"  inf ", nil, true},
		{"not-a-number", nil, false},
		{"-5", nil, false}, // negative (that's not -1) is invalid
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseLimit(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if (got == nil) != (tt.wantPtr == nil) {
				t.Errorf("ptr nilness mismatch: got %v want %v", got, tt.wantPtr)
			}
			if got != nil && tt.wantPtr != nil && *got != *tt.wantPtr {
				t.Errorf("value = %d, want %d", *got, *tt.wantPtr)
			}
		})
	}
}

func TestStorageLimitBytes(t *testing.T) {
	// Free tier: finite limit, multiplied into bytes.
	if FreeTier.StorageLimitBytes() != 5*1024*1024*1024 {
		t.Errorf("free tier storage = %d, want 5 GiB", FreeTier.StorageLimitBytes())
	}

	// Nil limit: treated as unlimited, returned as zero so callers can
	// skip the check entirely.
	enterprise := Tier{ID: "price_X", Name: "Enterprise"}
	if enterprise.StorageLimitBytes() != 0 {
		t.Errorf("nil-limit tier StorageLimitBytes = %d, want 0", enterprise.StorageLimitBytes())
	}
}
