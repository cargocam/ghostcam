package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/product"
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

	updated, err := product.Update(productID, &stripe.ProductParams{
		Metadata: map[string]string{
			"ghostcam_camera_limit": camStr,
			"ghostcam_storage_gb":   storStr,
		},
	})
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
