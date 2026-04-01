package handlers

import (
	"net/http"
)

// GetSubscription handles GET /api/v1/billing/subscription.
func (h *Handlers) GetSubscription(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": false,
		"tier":            "unlimited",
	})
}

// ListTiers handles GET /api/v1/billing/tiers.
func (h *Handlers) ListTiers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": false,
		"tiers":           []any{},
	})
}

// CreatePortal handles POST /api/v1/billing/portal.
func (h *Handlers) CreatePortal(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": false,
	})
}

// GetUsage handles GET /api/v1/billing/usage.
func (h *Handlers) GetUsage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": false,
	})
}

// StripeWebhook handles POST /api/v1/webhooks/stripe.
func (h *Handlers) StripeWebhook(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
