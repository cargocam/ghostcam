// Package billing defines subscription tiers and storage limits.
package billing

// Tier defines limits for a subscription tier.
type Tier struct {
	ID             string
	Name           string
	CameraLimit    *int // nil = unlimited
	StorageLimitGB *int // nil = unlimited
}

func intPtr(v int) *int { return &v }

// Tiers maps tier IDs to their limits. The set is closed: any ID not in this
// map is rejected by GetTier rather than silently falling back to unlimited.
var Tiers = map[string]Tier{
	"free":       {ID: "free", Name: "Free", CameraLimit: intPtr(1), StorageLimitGB: intPtr(5)},
	"starter":    {ID: "starter", Name: "Starter", CameraLimit: intPtr(4), StorageLimitGB: intPtr(50)},
	"pro":        {ID: "pro", Name: "Pro", CameraLimit: intPtr(16), StorageLimitGB: intPtr(500)},
	"enterprise": {ID: "enterprise", Name: "Enterprise", CameraLimit: nil, StorageLimitGB: nil},
}

// GetTier returns the tier for the given ID. On unknown input it returns the
// zero value and false — callers must handle !ok explicitly. This is
// fail-closed on purpose: a typo or DB corruption that produces an unknown
// tier string must not silently grant unlimited resources.
func GetTier(tierID string) (Tier, bool) {
	t, ok := Tiers[tierID]
	return t, ok
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
