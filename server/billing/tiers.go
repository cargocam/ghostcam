// Package billing resolves subscription tiers from Stripe products.
//
// The set of paid tiers is not hardcoded. Each Stripe product whose active
// price appears in the account is treated as a tier; its display name comes
// from product.name and its enforcement limits come from product.metadata:
//
//	ghostcam_camera_limit   int or "unlimited"
//	ghostcam_storage_gb     int or "unlimited"
//
// Products missing both metadata keys are skipped entirely — fail-closed
// against incomplete dashboard configuration. The free tier is not in
// Stripe and stays as a compile-time constant (FreeTier).
//
// The Cache is refreshed on server startup and hourly afterwards. Handlers
// look up tiers synchronously against the cached map; the Stripe API is
// never hit on the hot path.
package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/client"
)

// Tier defines the limits and display metadata for a subscription tier.
//
// For paid tiers, ID is the Stripe price ID ("price_...") and the rest is
// derived from the Stripe product. For the free tier, ID is the string
// "free" and the struct is populated from FreeTier.
type Tier struct {
	ID             string
	Name           string
	CameraLimit    *int   // nil = unlimited
	StorageLimitGB *int   // nil = unlimited
	PriceCents     int64  // per-interval price; 0 for free
	Currency       string // 3-letter ISO (e.g. "usd"); empty for free
	Interval       string // "month" / "year" / "" for free
}

// FreeTierID is the reserved tier identifier for unpaid accounts.
const FreeTierID = "free"

// FreeTier is the hardcoded free-tier definition. It is not looked up in
// Stripe and not affected by cache refreshes.
var FreeTier = Tier{
	ID:             FreeTierID,
	Name:           "Free",
	CameraLimit:    intPtr(1),
	StorageLimitGB: intPtr(5),
}

// LegacyTierNames is the set of pre-refactor tier identifiers that may
// still exist in subscriptions.tier rows. These are never produced by new
// checkouts — the app writes Stripe price IDs directly — but rows created
// before the Stripe-driven refactor still carry them. The server-side
// lazy-migration path in App.effectiveTier detects these strings, fetches
// the current Stripe price ID for the subscription, and rewrites the DB
// row. Exported so the legacy check lives next to the Tier type, not in
// a parallel list somewhere else in the codebase.
var LegacyTierNames = map[string]struct{}{
	"starter":    {},
	"pro":        {},
	"enterprise": {},
}

// IsLegacyTierName reports whether the given tier string is one of the
// pre-refactor hardcoded names. Callers use this to decide whether a DB
// row needs the one-shot Stripe-backed migration.
func IsLegacyTierName(tier string) bool {
	_, ok := LegacyTierNames[tier]
	return ok
}

// StorageLimitBytes returns the storage limit in bytes, or 0 for unlimited.
func (t Tier) StorageLimitBytes() uint64 {
	if t.StorageLimitGB == nil {
		return 0
	}
	return uint64(*t.StorageLimitGB) * 1024 * 1024 * 1024
}

// Cache holds the current Stripe-derived tier set. It is safe for concurrent
// use; readers take RLock, Refresh takes Lock.
type Cache struct {
	mu    sync.RWMutex
	tiers map[string]Tier // key = Stripe price ID
	order []string        // price IDs in display order (lowest price first)
}

// NewCache returns an empty cache. Call Refresh before the first lookup to
// populate it, or rely on Get falling back to the free tier on miss.
func NewCache() *Cache {
	return &Cache{tiers: map[string]Tier{}}
}

// Get returns the tier for the given ID. Recognized IDs:
//   - FreeTierID ("free") — always returns FreeTier
//   - any Stripe price ID present in the cache
//
// Legacy tier names (starter/pro/enterprise) are NOT resolved here — they
// must go through App.effectiveTier's lazy Stripe-backed migration so the
// DB row is rewritten to the correct price ID the first time it's read.
// Returning a hardcoded tier from the cache would make Stripe metadata
// no longer the single source of truth.
//
// Unknown IDs return (zero, false). Callers must fail closed.
func (c *Cache) Get(id string) (Tier, bool) {
	if id == FreeTierID || id == "" {
		return FreeTier, true
	}
	c.mu.RLock()
	t, ok := c.tiers[id]
	c.mu.RUnlock()
	return t, ok
}

// All returns every tier known to the cache, with FreeTier prepended so UIs
// can render a complete ladder without special-casing free. Paid tiers are
// returned in cache-assigned display order (ascending price).
func (c *Cache) All() []Tier {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Tier, 0, len(c.tiers)+1)
	out = append(out, FreeTier)
	for _, id := range c.order {
		if t, ok := c.tiers[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Refresh fetches every active price in the Stripe account (with the
// associated product expanded), parses metadata into Tier structs, and
// replaces the cache contents atomically. Products missing both metadata
// keys are skipped — we do not guess limits.
//
// On error the existing cache is left untouched so a transient Stripe
// outage doesn't drop all tiers.
func (c *Cache) Refresh(ctx context.Context, stripeKey string) error {
	if stripeKey == "" {
		return fmt.Errorf("stripe key is empty")
	}

	sc := &client.API{}
	sc.Init(stripeKey, nil)

	next := make(map[string]Tier)
	var order []priceOrdering

	params := &stripe.PriceListParams{
		Active: stripe.Bool(true),
	}
	params.AddExpand("data.product")
	params.Limit = stripe.Int64(100)

	it := sc.Prices.List(params)
	for it.Next() {
		price := it.Price()
		if price == nil || price.Product == nil {
			continue
		}
		product := price.Product
		if !product.Active {
			continue
		}
		tier, ok := tierFromStripe(price, product)
		if !ok {
			slog.Warn("billing: skipping stripe product without ghostcam metadata",
				"product_id", product.ID, "product_name", product.Name, "price_id", price.ID)
			continue
		}
		next[tier.ID] = tier
		order = append(order, priceOrdering{id: tier.ID, amount: price.UnitAmount})
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("listing stripe prices: %w", err)
	}

	// Stable display order: cheapest first, with deterministic tiebreak by
	// price ID so two $5 tiers always sort identically across refreshes.
	sortPriceOrdering(order)
	ids := make([]string, 0, len(order))
	for _, o := range order {
		ids = append(ids, o.id)
	}

	c.mu.Lock()
	c.tiers = next
	c.order = ids
	c.mu.Unlock()

	slog.Info("billing: stripe tier cache refreshed", "count", len(next))
	return nil
}

type priceOrdering struct {
	id     string
	amount int64
}

func sortPriceOrdering(s []priceOrdering) {
	// Simple insertion sort — the tier count is tiny (typically <10).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			if s[j].amount < s[j-1].amount ||
				(s[j].amount == s[j-1].amount && s[j].id < s[j-1].id) {
				s[j], s[j-1] = s[j-1], s[j]
				continue
			}
			break
		}
	}
}

// tierFromStripe parses a Stripe price + product into a Tier. Returns
// (zero, false) if the product metadata is incomplete.
func tierFromStripe(price *stripe.Price, product *stripe.Product) (Tier, bool) {
	cameraLimit, cameraOK := parseLimit(product.Metadata["ghostcam_camera_limit"])
	storageGB, storageOK := parseLimit(product.Metadata["ghostcam_storage_gb"])
	if !cameraOK && !storageOK {
		return Tier{}, false
	}

	interval := ""
	if price.Recurring != nil {
		interval = string(price.Recurring.Interval)
	}

	name := product.Name
	if name == "" {
		name = product.ID
	}

	return Tier{
		ID:             price.ID,
		Name:           name,
		CameraLimit:    cameraLimit,
		StorageLimitGB: storageGB,
		PriceCents:     price.UnitAmount,
		Currency:       string(price.Currency),
		Interval:       interval,
	}, true
}

// parseLimit converts a metadata string into a *int limit. Returns
// (nil, true) for the literal string "unlimited", (parsed, true) for a
// non-negative integer, and (nil, false) for missing/invalid values.
func parseLimit(raw string) (*int, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return nil, false
	}
	if raw == "unlimited" || raw == "inf" || raw == "-1" {
		return nil, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return nil, false
	}
	return intPtr(n), true
}

func intPtr(v int) *int { return &v }
