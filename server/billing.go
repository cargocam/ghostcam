package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"
)

// GetSubscription handles GET /api/v1/billing/subscription.
func (a *App) GetSubscription(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	sub, _ := a.DB.GetSubscription(r.Context(), userID)
	tierID := effectiveTier(sub, a.Stripe.SecretKey != "")

	writeJSON(w, http.StatusOK, map[string]any{
		"billing_enabled": a.Stripe.SecretKey != "",
		"tier":            tierID,
	})
}

// ListTiers handles GET /api/v1/billing/tiers.
func (a *App) ListTiers(w http.ResponseWriter, _ *http.Request) {
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
func (a *App) CreateCheckout(w http.ResponseWriter, r *http.Request) {
	if a.Stripe.SecretKey == "" {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := getUserID(r)

	var body checkoutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	priceID := a.tierToPriceID(body.Tier)
	if priceID == "" {
		writeError(w, http.StatusBadRequest, "invalid tier")
		return
	}

	stripe.Key = a.Stripe.SecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL:        stripe.String(body.SuccessURL),
		CancelURL:         stripe.String(body.CancelURL),
		ClientReferenceID: stripe.String(userID),
	}

	sub, _ := a.DB.GetSubscription(r.Context(), userID)
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
func (a *App) CreatePortal(w http.ResponseWriter, r *http.Request) {
	if a.Stripe.SecretKey == "" {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := getUserID(r)

	var body portalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sub, _ := a.DB.GetSubscription(r.Context(), userID)
	if sub == nil || sub.StripeCustomerID == nil {
		writeError(w, http.StatusBadRequest, "no_stripe_customer")
		return
	}

	stripe.Key = a.Stripe.SecretKey

	params := &stripe.BillingPortalSessionParams{
		Customer:  sub.StripeCustomerID,
		ReturnURL: stripe.String(body.ReturnURL),
	}
	if a.Stripe.PortalConfigID != "" {
		params.Configuration = stripe.String(a.Stripe.PortalConfigID)
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
func (a *App) GetUsage(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	ctx := r.Context()

	storageBytes, err := a.DB.GetUserStorageBytes(ctx, userID)
	if err != nil {
		slog.Error("get user storage failed", "error", err)
		storageBytes = 0
	}

	cameraCount, err := a.DB.GetCameraCount(ctx, userID)
	if err != nil {
		slog.Error("get camera count failed", "error", err)
		cameraCount = 0
	}

	sub, _ := a.DB.GetSubscription(ctx, userID)
	tier := billing.GetTier(effectiveTier(sub, a.Stripe.SecretKey != ""))

	writeJSON(w, http.StatusOK, map[string]any{
		"cameras_count":    cameraCount,
		"storage_bytes":    storageBytes,
		"camera_limit":     tier.CameraLimit,
		"storage_limit_gb": tier.StorageLimitGB,
	})
}

// StripeWebhook handles POST /api/v1/webhooks/stripe.
func (a *App) StripeWebhook(w http.ResponseWriter, r *http.Request) {
	if a.Stripe.SecretKey == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	var event stripe.Event
	if a.Stripe.WebhookSecret != "" {
		event, err = webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), a.Stripe.WebhookSecret)
		if err != nil {
			slog.Warn("stripe webhook signature verification failed", "error", err)
			http.Error(w, "", http.StatusBadRequest)
			return
		}
	} else if a.Config.PublicURL == "" {
		// Local dev only — no signature verification.
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "", http.StatusBadRequest)
			return
		}
	} else {
		slog.Error("stripe webhook rejected: STRIPE_WEBHOOK_SECRET not configured")
		http.Error(w, "", http.StatusForbidden)
		return
	}

	ctx := r.Context()

	seen, err := a.DB.CheckStripeEvent(ctx, event.ID)
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
		a.handleCheckoutCompleted(ctx, &event)
	case "customer.subscription.updated":
		a.handleSubscriptionUpdated(ctx, &event)
	case "customer.subscription.deleted":
		a.handleSubscriptionDeleted(ctx, &event)
	default:
		slog.Debug("unhandled stripe event type", "type", event.Type)
	}

	if err := a.DB.RecordStripeEvent(ctx, event.ID); err != nil {
		slog.Error("failed to record stripe event", "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

func (a *App) handleCheckoutCompleted(ctx context.Context, event *stripe.Event) {
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

	tier := a.priceIDToTier(session.Subscription.ID)

	customerID := session.Customer.ID
	subID := session.Subscription.ID

	if err := a.DB.UpdateSubscription(ctx, userID, &db.SubscriptionUpdate{
		Tier:                 &tier,
		Status:               strPtr("active"),
		StripeCustomerID:     &customerID,
		StripeSubscriptionID: &subID,
	}); err != nil {
		slog.Error("stripe: failed to update subscription after checkout", "user_id", userID, "error", err)
	}

	slog.Info("stripe: checkout completed", "user_id", userID, "tier", tier)
}

func (a *App) handleSubscriptionUpdated(ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		slog.Error("stripe: failed to unmarshal subscription", "error", err)
		return
	}

	customerID := sub.Customer.ID
	existing, err := a.DB.GetSubscriptionByStripeCustomer(ctx, customerID)
	if err != nil || existing == nil {
		slog.Warn("stripe: subscription.updated for unknown customer", "customer_id", customerID)
		return
	}

	tier := a.stripeTierFromSubscription(&sub)
	status := string(sub.Status)

	if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
		Tier:   &tier,
		Status: &status,
	}); err != nil {
		slog.Error("stripe: failed to update subscription", "user_id", existing.UserID, "error", err)
	}

	a.notifyCameraLimitExceeded(ctx, existing.UserID, tier)
}

func (a *App) handleSubscriptionDeleted(ctx context.Context, event *stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		slog.Error("stripe: failed to unmarshal subscription", "error", err)
		return
	}

	customerID := sub.Customer.ID
	existing, err := a.DB.GetSubscriptionByStripeCustomer(ctx, customerID)
	if err != nil || existing == nil {
		slog.Warn("stripe: subscription.deleted for unknown customer", "customer_id", customerID)
		return
	}

	freeTier := "free"
	canceledStatus := "canceled"
	if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
		Tier:   &freeTier,
		Status: &canceledStatus,
	}); err != nil {
		slog.Error("stripe: failed to downgrade subscription", "user_id", existing.UserID, "error", err)
	}

	slog.Info("stripe: subscription deleted, downgraded to free", "user_id", existing.UserID)

	a.notifyCameraLimitExceeded(ctx, existing.UserID, freeTier)
}

func (a *App) tierToPriceID(tier string) string {
	switch tier {
	case "starter":
		return a.Stripe.PriceIDStarter
	case "pro":
		return a.Stripe.PriceIDPro
	case "enterprise":
		return a.Stripe.PriceIDEnterprise
	default:
		return ""
	}
}

func (a *App) priceIDToTier(priceID string) string {
	switch priceID {
	case a.Stripe.PriceIDStarter:
		return "starter"
	case a.Stripe.PriceIDPro:
		return "pro"
	case a.Stripe.PriceIDEnterprise:
		return "enterprise"
	default:
		return "starter"
	}
}

func (a *App) stripeTierFromSubscription(sub *stripe.Subscription) string {
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		priceID := sub.Items.Data[0].Price.ID
		return a.priceIDToTier(priceID)
	}
	return "starter"
}

// notifyCameraLimitExceeded emits a camera_limit_exceeded SSE event if the
// user's camera count exceeds the new tier limit.
func (a *App) notifyCameraLimitExceeded(ctx context.Context, userID, tierID string) {
	tier := billing.GetTier(tierID)
	if tier.CameraLimit == nil {
		return
	}
	count, err := a.DB.GetCameraCount(ctx, userID)
	if err != nil || count <= int64(*tier.CameraLimit) {
		return
	}
	if a.Redis == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"user_id":      userID,
		"camera_count": count,
		"camera_limit": *tier.CameraLimit,
		"tier":         tierID,
	})
	eventID, _ := redis.WriteEvent(ctx, a.Redis.RDB(), userID, "", "camera_limit_exceeded", string(payload))
	withID, _ := json.Marshal(map[string]any{
		"event_id":     eventID,
		"user_id":      userID,
		"camera_count": count,
		"camera_limit": *tier.CameraLimit,
		"tier":         tierID,
	})
	a.Redis.RDB().Publish(ctx, fmt.Sprintf("storage_capped:%s", userID), withID)
	slog.Info("camera limit exceeded after tier change", "user_id", userID, "count", count, "limit", *tier.CameraLimit, "tier", tierID)
}

func strPtr(s string) *string { return &s }
