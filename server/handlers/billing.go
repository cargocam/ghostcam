package handlers

import (
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/ctxutil"
)

// GetSubscription handles GET /api/v1/billing/subscription.
func (h *Handlers) GetSubscription(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	tierID := defaultTierID
	sub, _ := h.DB.GetSubscription(r.Context(), userID)
	if sub != nil {
		tierID = sub.Tier
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": true,
		"tier":            tierID,
	})
}

// ListTiers handles GET /api/v1/billing/tiers.
func (h *Handlers) ListTiers(w http.ResponseWriter, _ *http.Request) {
	tiers := billing.AllTiers()
	result := make([]map[string]any, 0, len(tiers))
	for _, t := range tiers {
		result = append(result, map[string]any{
			"id":           t.ID,
			"name":         t.Name,
			"camera_limit": t.CameraLimit,
			"storage_gb":   t.StorageLimitGB,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tiers": result,
	})
}

// CreatePortal handles POST /api/v1/billing/portal.
// Returns a stub when Stripe is not configured; Stripe checkout/portal
// integration can be added later.
func (h *Handlers) CreatePortal(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": true,
		"message":         "Stripe portal not configured",
	})
}

// GetUsage handles GET /api/v1/billing/usage.
func (h *Handlers) GetUsage(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	ctx := r.Context()

	storageBytes, err := h.DB.GetUserStorageBytes(ctx, userID)
	if err != nil {
		slog.Error("get user storage failed", "error", err)
		storageBytes = 0
	}

	cameraCount, err := h.DB.GetCameraCount(ctx, userID)
	if err != nil {
		slog.Error("get camera count failed", "error", err)
		cameraCount = 0
	}

	tierID := defaultTierID
	sub, _ := h.DB.GetSubscription(ctx, userID)
	if sub != nil {
		tierID = sub.Tier
	}
	tier := billing.GetTier(tierID)

	writeJSON(w, http.StatusOK, map[string]any{
		"cameras_count":    cameraCount,
		"storage_bytes":    storageBytes,
		"camera_limit":     tier.CameraLimit,
		"storage_limit_gb": tier.StorageLimitGB,
	})
}

// StripeWebhook handles POST /api/v1/webhooks/stripe.
func (h *Handlers) StripeWebhook(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
