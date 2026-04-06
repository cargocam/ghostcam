package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"
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
		"billing_enabled": h.Stripe.SecretKey != "",
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

type checkoutRequest struct {
	Tier       string `json:"tier"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// CreateCheckout handles POST /api/v1/billing/checkout.
// Creates a Stripe Checkout Session and returns the redirect URL.
func (h *Handlers) CreateCheckout(w http.ResponseWriter, r *http.Request) {
	if h.Stripe.SecretKey == "" {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := ctxutil.GetUserID(r)

	var body checkoutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Map tier to Stripe price ID
	priceID := h.tierToPriceID(body.Tier)
	if priceID == "" {
		writeError(w, http.StatusBadRequest, "invalid tier")
		return
	}

	stripe.Key = h.Stripe.SecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(body.SuccessURL),
		CancelURL:  stripe.String(body.CancelURL),
		ClientReferenceID: stripe.String(userID),
	}

	// If user already has a Stripe customer ID, reuse it
	sub, _ := h.DB.GetSubscription(r.Context(), userID)
	if sub != nil && sub.StripeCustomerID != nil {
		params.Customer = sub.StripeCustomerID
	}

	session, err := checkoutsession.New(params)
	if err != nil {
		slog.Error("stripe checkout session creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "checkout_failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"url": session.URL})
}

type portalRequest struct {
	ReturnURL string `json:"return_url"`
}

// CreatePortal handles POST /api/v1/billing/portal.
// Creates a Stripe Customer Portal session for subscription management.
func (h *Handlers) CreatePortal(w http.ResponseWriter, r *http.Request) {
	if h.Stripe.SecretKey == "" {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := ctxutil.GetUserID(r)

	var body portalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sub, _ := h.DB.GetSubscription(r.Context(), userID)
	if sub == nil || sub.StripeCustomerID == nil {
		writeError(w, http.StatusBadRequest, "no_stripe_customer")
		return
	}

	stripe.Key = h.Stripe.SecretKey

	params := &stripe.BillingPortalSessionParams{
		Customer:  sub.StripeCustomerID,
		ReturnURL: stripe.String(body.ReturnURL),
	}
	if h.Stripe.PortalConfigID != "" {
		params.Configuration = stripe.String(h.Stripe.PortalConfigID)
	}

	session, err := portalsession.New(params)
	if err != nil {
		slog.Error("stripe portal session creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "portal_failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"url": session.URL})
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
func (h *Handlers) StripeWebhook(w http.ResponseWriter, r *http.Request) {
	if h.Stripe.SecretKey == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	// Verify signature if webhook secret is configured
	var event stripe.Event
	if h.Stripe.WebhookSecret != "" {
		event, err = webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), h.Stripe.WebhookSecret)
		if err != nil {
			slog.Warn("stripe webhook signature verification failed", "error", err)
			http.Error(w, "", http.StatusBadRequest)
			return
		}
	} else {
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()

	// Idempotency check
	seen, err := h.DB.CheckStripeEvent(ctx, event.ID)
	if err != nil {
		slog.Error("stripe event idempotency check failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if seen {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		h.handleCheckoutCompleted(ctx, &event)
	case "customer.subscription.updated":
		h.handleSubscriptionUpdated(ctx, &event)
	case "customer.subscription.deleted":
		h.handleSubscriptionDeleted(ctx, &event)
	default:
		slog.Debug("unhandled stripe event type", "type", event.Type)
	}

	// Record event as processed
	if err := h.DB.RecordStripeEvent(ctx, event.ID); err != nil {
		slog.Error("failed to record stripe event", "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handlers) handleCheckoutCompleted(ctx context.Context, event *stripe.Event) {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		slog.Error("stripe: failed to unmarshal checkout session", "error", err)
		return
	}

	userID := session.ClientReferenceID
	if userID == "" {
		slog.Warn("stripe: checkout.session.completed missing client_reference_id")
		return
	}

	// Determine tier from the price
	tier := h.priceIDToTier(session.Subscription.ID)

	customerID := session.Customer.ID
	subID := session.Subscription.ID

	if err := h.DB.UpdateSubscription(ctx, userID, &db.SubscriptionUpdate{
		Tier:                 &tier,
		Status:               strPtr("active"),
		StripeCustomerID:     &customerID,
		StripeSubscriptionID: &subID,
	}); err != nil {
		slog.Error("stripe: failed to update subscription after checkout", "user_id", userID, "error", err)
	}

	slog.Info("stripe: checkout completed", "user_id", userID, "tier", tier)
}

func (h *Handlers) handleSubscriptionUpdated(ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		slog.Error("stripe: failed to unmarshal subscription", "error", err)
		return
	}

	customerID := sub.Customer.ID
	existing, err := h.DB.GetSubscriptionByStripeCustomer(ctx, customerID)
	if err != nil || existing == nil {
		slog.Warn("stripe: subscription.updated for unknown customer", "customer_id", customerID)
		return
	}

	tier := h.stripeTierFromSubscription(&sub)
	status := string(sub.Status)

	if err := h.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
		Tier:   &tier,
		Status: &status,
	}); err != nil {
		slog.Error("stripe: failed to update subscription", "user_id", existing.UserID, "error", err)
	}
}

func (h *Handlers) handleSubscriptionDeleted(ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		slog.Error("stripe: failed to unmarshal subscription", "error", err)
		return
	}

	customerID := sub.Customer.ID
	existing, err := h.DB.GetSubscriptionByStripeCustomer(ctx, customerID)
	if err != nil || existing == nil {
		slog.Warn("stripe: subscription.deleted for unknown customer", "customer_id", customerID)
		return
	}

	// Downgrade to free tier
	freeTier := "free"
	canceledStatus := "canceled"
	if err := h.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
		Tier:   &freeTier,
		Status: &canceledStatus,
	}); err != nil {
		slog.Error("stripe: failed to downgrade subscription", "user_id", existing.UserID, "error", err)
	}

	slog.Info("stripe: subscription deleted, downgraded to free", "user_id", existing.UserID)
}

func (h *Handlers) tierToPriceID(tier string) string {
	switch tier {
	case "starter":
		return h.Stripe.PriceIDStarter
	case "pro":
		return h.Stripe.PriceIDPro
	case "enterprise":
		return h.Stripe.PriceIDEnterprise
	default:
		return ""
	}
}

func (h *Handlers) priceIDToTier(priceID string) string {
	switch priceID {
	case h.Stripe.PriceIDStarter:
		return "starter"
	case h.Stripe.PriceIDPro:
		return "pro"
	case h.Stripe.PriceIDEnterprise:
		return "enterprise"
	default:
		return "starter" // default if unknown
	}
}

func (h *Handlers) stripeTierFromSubscription(sub *stripe.Subscription) string {
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		priceID := sub.Items.Data[0].Price.ID
		return h.priceIDToTier(priceID)
	}
	return "starter"
}

func strPtr(s string) *string { return &s }
