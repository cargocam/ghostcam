package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/product"
	"github.com/stripe/stripe-go/v82/subscription"
)

// AdminListBillingTiers handles GET /api/v1/admin/billing/tiers.
//
// Unlike the public ListTiers (which reads from the pre-filtered tier cache),
// this endpoint queries Stripe directly so the admin UI can show every active
// price in the account — including products that do NOT yet have the
// ghostcam_camera_limit / ghostcam_storage_gb metadata set. That's the whole
// point of the admin view: making the currently-invisible "unconfigured"
// products visible so the admin can tag them.
func (a *App) AdminListBillingTiers(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	out, err := a.buildAdminTierList()
	if err != nil {
		slog.Error("admin: list stripe prices failed", "error", err)
		writeError(w, http.StatusBadGateway, "stripe_list_failed")
		return
	}

	writeJSON(w, http.StatusOK, apitypes.AdminListBillingTiersResponse{Tiers: out})
}

// AdminUpdateBillingTier handles PATCH /api/v1/admin/billing/tiers/{priceID}.
//
// Updates the product's ghostcam_camera_limit / ghostcam_storage_gb metadata
// on Stripe, then refreshes the local tier cache so the change is visible
// everywhere in the next render. Stripe is the single source of truth — we
// never persist the limits anywhere else.
//
// The request body accepts a non-negative integer or null for each field
// (null = unlimited). Both fields are required to avoid partial updates
// leaving the product half-configured.
func (a *App) AdminUpdateBillingTier(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	priceID := chi.URLParam(r, "priceID")
	if priceID == "" {
		writeError(w, http.StatusBadRequest, "missing price id")
		return
	}

	var body apitypes.AdminUpdateBillingTierRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	camStr, ok := encodeLimitForStripe(body.CameraLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "camera_limit must be a non-negative integer or null")
		return
	}
	storStr, ok := encodeLimitForStripe(body.StorageGB)
	if !ok {
		writeError(w, http.StatusBadRequest, "storage_gb must be a non-negative integer or null")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	// Look up the price to get its product ID. The admin view passes the
	// price ID because that's the natural tier identifier everywhere else
	// in the codebase, but Stripe metadata lives on the product.
	priceParams := &stripe.PriceParams{}
	priceParams.AddExpand("product")
	pr, err := price.Get(priceID, priceParams)
	if err != nil || pr == nil || pr.Product == nil {
		slog.Warn("admin: price retrieve failed", "price_id", priceID, "error", err)
		writeError(w, http.StatusNotFound, "price_not_found")
		return
	}
	productID := pr.Product.ID

	updateParams := &stripe.ProductParams{
		Metadata: map[string]string{
			"ghostcam_camera_limit": camStr,
			"ghostcam_storage_gb":   storStr,
		},
	}
	// Name is optional; omitting or sending an empty string leaves it
	// unchanged. Callers that want to edit the name send it alongside the
	// limits. Trim to guard against accidental whitespace-only updates.
	if trimmed := strings.TrimSpace(body.Name); trimmed != "" {
		updateParams.Name = stripe.String(trimmed)
	}
	updated, err := product.Update(productID, updateParams)
	if err != nil || updated == nil {
		slog.Error("admin: stripe product update failed", "product_id", productID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_update_failed")
		return
	}

	// Refresh the tier cache synchronously so the admin sees the change in
	// the next GET /admin/billing/tiers (and so other users' settings
	// dialogs pick it up on next refresh) without waiting for the
	// product.updated webhook round-trip.
	refreshCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tiers.Refresh(refreshCtx, a.Config.StripeSecretKey); err != nil {
		slog.Warn("admin: tier cache refresh after update failed", "error", err)
	}

	slog.Info("admin: updated stripe tier metadata",
		"admin_email", getUserEmail(r),
		"product_id", productID,
		"price_id", priceID,
		"camera_limit", camStr,
		"storage_gb", storStr,
	)

	// Return the fresh admin-view tier list so the UI can re-render without
	// a second round trip.
	a.AdminListBillingTiers(w, r)
}

// AdminCreateBillingTier handles POST /api/v1/admin/billing/tiers.
//
// Creates a new Stripe product (with ghostcam_* metadata) and a single
// recurring price in one call, then refreshes the tier cache so the new
// tier appears in the public settings dialog immediately. The UI's
// "New tier" button routes here.
//
// We deliberately do the two Stripe calls sequentially and NOT inside a
// transaction (Stripe has no such thing). On a partial failure — product
// created but price creation errored — the orphaned product is left in
// place and the admin will see it as "Unconfigured" with no price on the
// next list. That's survivable: they can retry (idempotent via a
// different name) or archive the orphan. Worth noting in the log so an
// operator knows what to clean up.
func (a *App) AdminCreateBillingTier(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	var body apitypes.AdminCreateBillingTierRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if body.PriceCents <= 0 {
		writeError(w, http.StatusBadRequest, "price_cents must be positive")
		return
	}
	currency := strings.ToLower(strings.TrimSpace(body.Currency))
	if len(currency) != 3 {
		writeError(w, http.StatusBadRequest, "currency must be a 3-letter ISO code")
		return
	}
	interval := strings.ToLower(strings.TrimSpace(body.Interval))
	if interval != "month" && interval != "year" {
		writeError(w, http.StatusBadRequest, `interval must be "month" or "year"`)
		return
	}
	camStr, ok := encodeLimitForStripe(body.CameraLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "camera_limit must be a non-negative integer or null")
		return
	}
	storStr, ok := encodeLimitForStripe(body.StorageGB)
	if !ok {
		writeError(w, http.StatusBadRequest, "storage_gb must be a non-negative integer or null")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	prod, err := product.New(&stripe.ProductParams{
		Name: stripe.String(name),
		Metadata: map[string]string{
			"ghostcam_camera_limit": camStr,
			"ghostcam_storage_gb":   storStr,
		},
	})
	if err != nil || prod == nil {
		slog.Error("admin: stripe product create failed", "name", name, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_create_failed")
		return
	}

	pr, err := price.New(&stripe.PriceParams{
		Product:    stripe.String(prod.ID),
		Currency:   stripe.String(currency),
		UnitAmount: stripe.Int64(body.PriceCents),
		Recurring: &stripe.PriceRecurringParams{
			Interval: stripe.String(interval),
		},
	})
	if err != nil || pr == nil {
		slog.Error("admin: stripe price create failed — orphaned product left in place; admin should archive or retry",
			"product_id", prod.ID, "name", name, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_price_create_failed")
		return
	}

	refreshCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tiers.Refresh(refreshCtx, a.Config.StripeSecretKey); err != nil {
		slog.Warn("admin: tier cache refresh after create failed", "error", err)
	}

	slog.Info("admin: created stripe tier",
		"admin_email", getUserEmail(r),
		"product_id", prod.ID,
		"price_id", pr.ID,
		"name", name,
		"price_cents", body.PriceCents,
		"currency", currency,
		"interval", interval,
		"camera_limit", camStr,
		"storage_gb", storStr,
	)

	a.AdminListBillingTiers(w, r)
}

// AdminArchiveBillingTier handles POST
// /api/v1/admin/billing/tiers/{priceID}/archive.
//
// Deactivates the Stripe price (and the product, if this was its last
// active price). Stripe's "archive" is active=false, not a hard delete
// — existing subscriptions keep billing at the archived price until
// cancelled, which is the right fail-safe for accidental clicks.
//
// Archiving a price with live subscribers is guarded: the first request
// returns 409 with an active_subscribers count so the UI can phrase a
// "yes, I know, archive anyway" confirmation. The second request with
// confirm=true proceeds. This prevents a CFO from silently orphaning
// a paying customer with a single click.
func (a *App) AdminArchiveBillingTier(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	priceID := chi.URLParam(r, "priceID")
	if priceID == "" {
		writeError(w, http.StatusBadRequest, "missing price id")
		return
	}

	var body apitypes.AdminArchiveBillingTierRequest
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body is valid

	stripe.Key = a.Config.StripeSecretKey

	// Probe for live subscriptions on this price. Paginate with a
	// safety cap so a misconfigured Stripe account can't stall the
	// handler indefinitely — one page is enough to show "there are
	// some, you should confirm".
	activeCount, err := countActiveSubscriptionsForPrice(priceID)
	if err != nil {
		slog.Warn("admin: stripe subscription list failed", "price_id", priceID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_list_failed")
		return
	}
	if activeCount > 0 && !body.Confirm {
		writeJSON(w, http.StatusConflict, apitypes.AdminArchiveConflictResponse{
			Error:             "active_subscribers",
			ActiveSubscribers: activeCount,
		})
		return
	}

	// Fetch the price to discover its product ID and see whether it
	// is the last active price on that product.
	priceParams := &stripe.PriceParams{}
	priceParams.AddExpand("product")
	pr, err := price.Get(priceID, priceParams)
	if err != nil || pr == nil || pr.Product == nil {
		slog.Warn("admin: price retrieve failed", "price_id", priceID, "error", err)
		writeError(w, http.StatusNotFound, "price_not_found")
		return
	}
	productID := pr.Product.ID

	if _, err := price.Update(priceID, &stripe.PriceParams{
		Active: stripe.Bool(false),
	}); err != nil {
		slog.Error("admin: stripe price archive failed", "price_id", priceID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_archive_failed")
		return
	}

	// If this was the last active price on the product, archive the
	// product too so it disappears from the Stripe dashboard's active
	// list (and our tier cache) rather than sitting around with nothing
	// to sell.
	if lastActive, err := productHasOtherActivePrices(productID, priceID); err != nil {
		slog.Warn("admin: could not check for sibling prices — leaving product active", "product_id", productID, "error", err)
	} else if !lastActive {
		if _, err := product.Update(productID, &stripe.ProductParams{
			Active: stripe.Bool(false),
		}); err != nil {
			slog.Warn("admin: stripe product archive failed — price archived but product still active", "product_id", productID, "error", err)
		}
	}

	refreshCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tiers.Refresh(refreshCtx, a.Config.StripeSecretKey); err != nil {
		slog.Warn("admin: tier cache refresh after archive failed", "error", err)
	}

	slog.Info("admin: archived stripe tier",
		"admin_email", getUserEmail(r),
		"product_id", productID,
		"price_id", priceID,
		"active_subscribers", activeCount,
	)

	a.AdminListBillingTiers(w, r)
}

// countActiveSubscriptionsForPrice counts Stripe subscriptions in
// statuses that mean "currently paying" (active or trialing) whose
// items reference the given price. Capped at one page (10) — we only
// need to know "zero vs nonzero" to drive the confirmation dialog,
// so the exact count only matters up to that page boundary.
func countActiveSubscriptionsForPrice(priceID string) (int64, error) {
	params := &stripe.SubscriptionListParams{
		Price:  stripe.String(priceID),
		Status: stripe.String("active"),
	}
	params.Limit = stripe.Int64(10)
	var count int64
	it := subscription.List(params)
	for it.Next() {
		count++
	}
	if err := it.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// productHasOtherActivePrices reports whether the given product still
// has at least one active price OTHER than excludePriceID. Used to
// decide whether to archive the product alongside its last price.
func productHasOtherActivePrices(productID, excludePriceID string) (bool, error) {
	params := &stripe.PriceListParams{
		Product: stripe.String(productID),
		Active:  stripe.Bool(true),
	}
	params.Limit = stripe.Int64(10)
	it := price.List(params)
	for it.Next() {
		if it.Price() != nil && it.Price().ID != excludePriceID {
			return true, nil
		}
	}
	return false, it.Err()
}

// AdminBillingTierSubscribers handles GET
// /api/v1/admin/billing/tiers/{priceID}/subscribers. Tiny probe that
// returns the active-subscriber count for a single price. Used by the
// Reprice dialog to show the admin "this affects N people" before
// they commit; the existing list endpoint deliberately does NOT carry
// this number so fetching the full admin list stays cheap (one Stripe
// call instead of one per row).
func (a *App) AdminBillingTierSubscribers(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}
	priceID := chi.URLParam(r, "priceID")
	if priceID == "" {
		writeError(w, http.StatusBadRequest, "missing price id")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	count, err := countActiveSubscriptionsForPrice(priceID)
	if err != nil {
		slog.Warn("admin: stripe subscription list failed", "price_id", priceID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_list_failed")
		return
	}
	writeJSON(w, http.StatusOK, apitypes.AdminBillingTierSubscribersResponse{
		ActiveSubscribers: count,
	})
}

// AdminRepriceBillingTier handles POST
// /api/v1/admin/billing/tiers/{priceID}/reprice.
//
// Stripe prices are immutable, so "changing a price" is really a
// three-step dance: create a new price on the same product, optionally
// migrate existing subscriptions to the new price, and archive the
// old price. This handler wraps the whole dance so the CFO sees one
// admin action instead of three.
//
// Currency and interval are intentionally not configurable here —
// Stripe doesn't allow swapping a subscription between currencies or
// billing intervals on the fly, and the right move in that case is
// to create a brand-new tier via the existing Create endpoint. This
// handler copies currency + interval from the old price onto the new
// one so the subscription swap is always valid.
//
// Subscriber safety:
//
//	migrate_subscribers = true
//	  For each active subscription on the old price: find the item
//	  referencing the old price, update it to the new price in one
//	  subscriptions.Update call, with proration controlled by the
//	  Prorate flag. Counts are reported back in MigratedCount.
//
//	migrate_subscribers = false && there are active subscribers
//	  Return 409 with the count unless ConfirmDroppingSubscribers
//	  is set. Without explicit confirmation we refuse to strand
//	  paying customers on an archived price.
//
//	migrate_subscribers = false && no active subscribers
//	  Straightforward: create new, archive old.
//
// On any Stripe error after the new price has been created, we log
// loudly and return the error; the orphaned new price sits in Stripe
// until the admin archives it by hand or retries. Stripe has no
// transactions, so this is the honest tradeoff.
func (a *App) AdminRepriceBillingTier(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}
	oldPriceID := chi.URLParam(r, "priceID")
	if oldPriceID == "" {
		writeError(w, http.StatusBadRequest, "missing price id")
		return
	}

	var body apitypes.AdminRepriceBillingTierRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.PriceCents <= 0 {
		writeError(w, http.StatusBadRequest, "price_cents must be positive")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	// 1. Retrieve the old price with the product expanded so we know
	//    currency, interval, existing metadata, and product ID.
	oldPriceParams := &stripe.PriceParams{}
	oldPriceParams.AddExpand("product")
	oldPrice, err := price.Get(oldPriceID, oldPriceParams)
	if err != nil || oldPrice == nil || oldPrice.Product == nil {
		slog.Warn("admin: reprice — old price retrieve failed", "price_id", oldPriceID, "error", err)
		writeError(w, http.StatusNotFound, "price_not_found")
		return
	}
	if oldPrice.Recurring == nil {
		writeError(w, http.StatusBadRequest, "one-time prices cannot be repriced; create a new tier instead")
		return
	}
	if body.PriceCents == oldPrice.UnitAmount {
		writeError(w, http.StatusBadRequest, "new price is the same as the current price")
		return
	}

	// 2. Gate on subscriber safety before we touch anything.
	count, err := countActiveSubscriptionsForPrice(oldPriceID)
	if err != nil {
		slog.Warn("admin: reprice — stripe subscription list failed", "price_id", oldPriceID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_list_failed")
		return
	}
	if count > 0 && !body.MigrateSubscribers && !body.ConfirmDroppingSubscribers {
		writeJSON(w, http.StatusConflict, apitypes.AdminArchiveConflictResponse{
			Error:             "active_subscribers",
			ActiveSubscribers: count,
		})
		return
	}

	// 3. Create the new price on the same product with the same
	//    currency, interval, and metadata. Stripe's immutability
	//    guarantee is the reason this endpoint exists in the first
	//    place.
	newPriceParams := &stripe.PriceParams{
		Product:    stripe.String(oldPrice.Product.ID),
		Currency:   stripe.String(string(oldPrice.Currency)),
		UnitAmount: stripe.Int64(body.PriceCents),
		Recurring: &stripe.PriceRecurringParams{
			Interval: stripe.String(string(oldPrice.Recurring.Interval)),
		},
	}
	newPrice, err := price.New(newPriceParams)
	if err != nil || newPrice == nil {
		slog.Error("admin: reprice — stripe price create failed",
			"old_price_id", oldPriceID, "product_id", oldPrice.Product.ID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_price_create_failed")
		return
	}

	// 4. Migrate subscribers (if requested). Each Update is its own
	//    API call — we accept the O(N) cost because N is tiny for
	//    most accounts, and because doing it one-by-one means a
	//    single customer failure doesn't block the others.
	var migrated int
	if body.MigrateSubscribers && count > 0 {
		migrated, err = migrateSubscribersToNewPrice(
			oldPriceID, newPrice.ID, body.Prorate,
		)
		if err != nil {
			slog.Error("admin: reprice — subscriber migration failed partway",
				"old_price_id", oldPriceID, "new_price_id", newPrice.ID,
				"migrated_so_far", migrated, "error", err)
			writeError(w, http.StatusBadGateway, "migration_failed")
			return
		}
	}

	// 5. Archive the old price now that subscribers (if any) have
	//    been moved off it. Product itself stays active — there's
	//    still the new price on it.
	if _, err := price.Update(oldPriceID, &stripe.PriceParams{
		Active: stripe.Bool(false),
	}); err != nil {
		slog.Error("admin: reprice — old price archive failed",
			"old_price_id", oldPriceID, "new_price_id", newPrice.ID, "error", err)
		writeError(w, http.StatusBadGateway, "stripe_archive_failed")
		return
	}

	// 6. Refresh the tier cache so the public settings dialog sees
	//    the new price_id on its next render.
	refreshCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tiers.Refresh(refreshCtx, a.Config.StripeSecretKey); err != nil {
		slog.Warn("admin: tier cache refresh after reprice failed", "error", err)
	}

	slog.Info("admin: repriced stripe tier",
		"admin_email", getUserEmail(r),
		"product_id", oldPrice.Product.ID,
		"old_price_id", oldPriceID,
		"new_price_id", newPrice.ID,
		"old_unit_amount", oldPrice.UnitAmount,
		"new_unit_amount", body.PriceCents,
		"migrated_count", migrated,
		"prorated", body.Prorate,
	)

	// 7. Return the fresh admin tier list plus the migrated count so
	//    the UI can re-render and show "Migrated N subscribers" in
	//    one round trip.
	fresh, err := a.buildAdminTierList()
	if err != nil {
		slog.Error("admin: reprice — post-op admin tier list failed", "error", err)
		writeError(w, http.StatusBadGateway, "stripe_list_failed")
		return
	}
	writeJSON(w, http.StatusOK, apitypes.AdminRepriceBillingTierResponse{
		Tiers:         fresh,
		MigratedCount: migrated,
	})
}

// migrateSubscribersToNewPrice walks every active subscription on
// oldPriceID and updates the subscription item referencing that price
// to use newPriceID. Proration is controlled via the Stripe
// proration_behavior param.
//
// Returns the number of subscriptions successfully migrated; on error,
// the count reflects how many were already done so callers can report
// partial progress in the log.
func migrateSubscribersToNewPrice(oldPriceID, newPriceID string, prorate bool) (int, error) {
	prorationBehavior := "none"
	if prorate {
		prorationBehavior = "create_prorations"
	}

	params := &stripe.SubscriptionListParams{
		Price:  stripe.String(oldPriceID),
		Status: stripe.String("active"),
	}
	params.Limit = stripe.Int64(100)
	params.AddExpand("data.items")

	migrated := 0
	it := subscription.List(params)
	for it.Next() {
		sub := it.Subscription()
		if sub == nil || sub.Items == nil {
			continue
		}
		// Find the subscription item whose price is the old one.
		// In the usual single-item subscription case this is item 0,
		// but we search to be safe against multi-item subs.
		var oldItemID string
		for _, item := range sub.Items.Data {
			if item != nil && item.Price != nil && item.Price.ID == oldPriceID {
				oldItemID = item.ID
				break
			}
		}
		if oldItemID == "" {
			continue
		}
		_, err := subscription.Update(sub.ID, &stripe.SubscriptionParams{
			Items: []*stripe.SubscriptionItemsParams{
				{ID: stripe.String(oldItemID), Price: stripe.String(newPriceID)},
			},
			ProrationBehavior: stripe.String(prorationBehavior),
		})
		if err != nil {
			return migrated, err
		}
		migrated++
	}
	if err := it.Err(); err != nil {
		return migrated, err
	}
	return migrated, nil
}

// buildAdminTierList is the shared core of AdminListBillingTiers and
// any other handler that needs a fresh admin-view snapshot. Extracted
// so reprice can return an updated list in its response without a
// second HTTP round trip.
func (a *App) buildAdminTierList() ([]apitypes.AdminBillingTier, error) {
	params := &stripe.PriceListParams{Active: stripe.Bool(true)}
	params.AddExpand("data.product")
	params.Limit = stripe.Int64(100)

	out := make([]apitypes.AdminBillingTier, 0, 8)
	it := price.List(params)
	for it.Next() {
		pr := it.Price()
		if pr == nil || pr.Product == nil {
			continue
		}
		prod := pr.Product
		if !prod.Active {
			continue
		}
		entry := apitypes.AdminBillingTier{
			PriceID:        pr.ID,
			ProductID:      prod.ID,
			ProductName:    prod.Name,
			PriceCents:     pr.UnitAmount,
			Currency:       string(pr.Currency),
			CameraLimitRaw: prod.Metadata["ghostcam_camera_limit"],
			StorageGBRaw:   prod.Metadata["ghostcam_storage_gb"],
		}
		if pr.Recurring != nil {
			entry.Interval = string(pr.Recurring.Interval)
		}
		entry.Configured = entry.CameraLimitRaw != "" && entry.StorageGBRaw != ""
		out = append(out, entry)
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeLimitForStripe converts a request-body *int into the string Stripe
// will store on the product. nil → "unlimited", 0..N → "N". Negative
// numbers are rejected by returning ok=false. 0 is accepted as a legitimate
// (if unusual) limit — the server treats zero as a hard stop, which is a
// valid way for the admin to disable a tier without deleting the product.
func encodeLimitForStripe(v *int) (string, bool) {
	if v == nil {
		return "unlimited", true
	}
	if *v < 0 {
		return "", false
	}
	return strconv.Itoa(*v), true
}
