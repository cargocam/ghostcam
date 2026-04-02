// Package billing defines subscription tiers and storage limits.
package billing

// Tier defines limits for a subscription tier.
type Tier struct {
	ID             string
	Name           string
	CameraLimit    *int
	StorageLimitGB *int
}

func intPtr(v int) *int { return &v }

// Tiers maps tier IDs to their limits.
var Tiers = map[string]Tier{
	"free":       {ID: "free", Name: "Free", CameraLimit: intPtr(1), StorageLimitGB: intPtr(5)},
	"starter":    {ID: "starter", Name: "Starter", CameraLimit: intPtr(4), StorageLimitGB: intPtr(50)},
	"pro":        {ID: "pro", Name: "Pro", CameraLimit: intPtr(16), StorageLimitGB: intPtr(500)},
	"enterprise": {ID: "enterprise", Name: "Enterprise", CameraLimit: nil, StorageLimitGB: nil},
	"unlimited":  {ID: "unlimited", Name: "Unlimited", CameraLimit: nil, StorageLimitGB: nil},
}

// GetTier returns the tier for the given ID, defaulting to "unlimited" if not found.
func GetTier(tierID string) Tier {
	if t, ok := Tiers[tierID]; ok {
		return t
	}
	return Tiers["unlimited"]
}

// StorageLimitBytes returns the storage limit in bytes for a tier, or 0 for unlimited.
func (t Tier) StorageLimitBytes() uint64 {
	if t.StorageLimitGB == nil {
		return 0
	}
	return uint64(*t.StorageLimitGB) * 1024 * 1024 * 1024
}

// AllTiers returns all tiers in display order.
func AllTiers() []Tier {
	return []Tier{
		Tiers["free"],
		Tiers["starter"],
		Tiers["pro"],
		Tiers["enterprise"],
	}
}
