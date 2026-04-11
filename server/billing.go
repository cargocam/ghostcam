package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"
)

// GetSubscription handles GET /api/v1/billing/subscription.
func (a *App) GetSubscription(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	sub, _ := a.DB.GetSubscription(r.Context(), userID)
	tier := a.effectiveTier(r.Context(), sub)

	writeJSON(w, http.StatusOK, apitypes.SubscriptionResponse{
		BillingEnabled: a.stripeConfigured(),
		Tier:           tier.ID,
		TierName:       tier.Name,
	})
}

// ListTiers handles GET /api/v1/billing/tiers.
func (a *App) ListTiers(w http.ResponseWriter, _ *http.Request) {
	tiers := a.Tiers.All()
	result := make([]apitypes.TierInfo, 0, len(tiers))
	for _, t := range tiers {
		result = append(result, apitypes.TierInfo{
			ID:          t.ID,
			Name:        t.Name,
			CameraLimit: t.CameraLimit,
			StorageGB:   t.StorageLimitGB,
			PriceCents:  t.PriceCents,
			Currency:    t.Currency,
			Interval:    t.Interval,
		})
	}
	writeJSON(w, http.StatusOK, apitypes.ListTiersResponse{Tiers: result})
}

// RefreshTiers handles POST /api/v1/billing/tiers/refresh. Synchronously
// reloads the tier cache from Stripe and returns the fresh list. Intended
// for the UI's Retry affordance — a user who just tagged a Stripe product
// with ghostcam_camera_limit / ghostcam_storage_gb metadata can hit Retry
// and see the update immediately, without waiting for the product.updated
// webhook or the hourly background tick. Rate-limited independently so a
// stuck client looping on retry cannot hammer the Stripe API.
func (a *App) RefreshTiers(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tiers.Refresh(ctx, a.Config.StripeSecretKey); err != nil {
		slog.Warn("billing: tier cache refresh-on-demand failed", "error", err)
		writeError(w, http.StatusBadGateway, "stripe_refresh_failed")
		return
	}
	a.ListTiers(w, r)
}

// CreateCheckout handles POST /api/v1/billing/checkout.
// Creates a Stripe Checkout Session and returns the redirect URL.
//
// The request body carries a Stripe price ID in the Tier field (not a
// friendly name like "starter"). The server validates it is present in the
// tier cache — unknown IDs are 400'd rather than forwarded to Stripe, so a
// compromised client can't spin up a checkout session for an arbitrary
// product.
func (a *App) CreateCheckout(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := getUserID(r)

	var body apitypes.CheckoutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	tier, ok := a.Tiers.Get(body.Tier)
	if !ok || tier.ID == billing.FreeTierID {
		writeError(w, http.StatusBadRequest, "invalid tier")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(tier.ID),
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

	writeJSON(w, http.StatusOK, apitypes.CheckoutResponse{URL: session.URL})
}

// CreatePortal handles POST /api/v1/billing/portal.
func (a *App) CreatePortal(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured")
		return
	}

	userID := getUserID(r)

	var body apitypes.PortalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sub, _ := a.DB.GetSubscription(r.Context(), userID)
	if sub == nil || sub.StripeCustomerID == nil {
		writeError(w, http.StatusBadRequest, "no_stripe_customer")
		return
	}

	stripe.Key = a.Config.StripeSecretKey

	params := &stripe.BillingPortalSessionParams{
		Customer:  sub.StripeCustomerID,
		ReturnURL: stripe.String(body.ReturnURL),
	}
	if a.Config.StripePortalConfigID != "" {
		params.Configuration = stripe.String(a.Config.StripePortalConfigID)
	}

	session, err := portalsession.New(params)
	if err != nil {
		slog.Error("stripe portal session creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "portal_failed")
		return
	}

	writeJSON(w, http.StatusOK, apitypes.PortalResponse{URL: session.URL})
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
	tier := a.effectiveTier(r.Context(), sub)

	writeJSON(w, http.StatusOK, apitypes.UsageResponse{
		CamerasCount:   cameraCount,
		StorageBytes:   storageBytes,
		CameraLimit:    tier.CameraLimit,
		StorageLimitGB: tier.StorageLimitGB,
	})
}

// StripeWebhook handles POST /api/v1/webhooks/stripe.
func (a *App) StripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !a.stripeConfigured() {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	var event stripe.Event
	if a.Config.StripeWebhookSecret != "" {
		// Ignore API version mismatches between the webhook endpoint
		// (pinned to the Stripe account's default at creation time) and
		// the stripe-go version we link against. We only read a small,
		// stable subset of fields on the events we handle
		// (checkout.session.completed, customer.subscription.*,
		// product.*, price.*), so a version skew does not meaningfully
		// affect deserialization for our use. Without this flag,
		// stripe-go refuses to construct events at all when the
		// versions differ, which means any webhook endpoint created in
		// the Stripe dashboard against an older account default will be
		// rejected with 400 until the account is upgraded.
		event, err = webhook.ConstructEventWithOptions(
			body,
			r.Header.Get("Stripe-Signature"),
			a.Config.StripeWebhookSecret,
			webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true},
		)
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
	case "product.created", "product.updated", "product.deleted",
		"price.created", "price.updated", "price.deleted":
		// A product or price changed in Stripe — refresh the tier cache
		// so the UI picks up the change on the next render. Webhooks
		// and the settings Retry button are the only refresh paths; the
		// server has no hourly ticker. The refresh runs asynchronously
		// in a bounded context so a slow Stripe API call doesn't block
		// webhook delivery (Stripe retries on timeout, which would risk
		// event reordering).
		eventType := event.Type
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := a.Tiers.Refresh(refreshCtx, a.Config.StripeSecretKey); err != nil {
				slog.Warn("billing: tier cache refresh after webhook failed",
					"event_type", eventType, "error", err)
			}
		}()
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

	subID := stripeSubID(session.Subscription)
	customerID := session.Customer.ID

	// Re-fetch the subscription via the API rather than reading it from
	// the event payload. Stripe delivers webhooks pinned to the account's
	// default API version (often older than what stripe-go expects), so
	// fields like items.data[i].price may be missing from the event's
	// subscription object — subscription.Get returns it in stripe-go's
	// pinned API version, with the shape the rest of the code assumes.
	tier := billing.FreeTierID
	if subID != "" {
		stripe.Key = a.Config.StripeSecretKey
		freshSub, err := subscription.Get(subID, nil)
		if err != nil || freshSub == nil {
			slog.Error("stripe: checkout — subscription.Get failed, keeping user on free",
				"user_id", userID, "subscription_id", subID, "error", err)
		} else if resolved, ok := a.stripeTierFromSubscription(freshSub); !ok {
			slog.Error("stripe: checkout with malformed subscription, keeping user on free",
				"user_id", userID, "subscription_id", subID)
		} else if _, known := a.Tiers.Get(resolved); !known {
			slog.Error("stripe: checkout with unknown price ID, keeping user on free",
				"user_id", userID, "price_id", resolved, "subscription_id", subID)
		} else {
			tier = resolved
		}
	}

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

	// Re-fetch the subscription via the API: the event payload is pinned
	// to the account's default API version, which may omit fields that
	// stripe-go expects (items.data[i].price, most importantly). See the
	// note in handleCheckoutCompleted.
	stripe.Key = a.Config.StripeSecretKey
	freshSub, err := subscription.Get(sub.ID, nil)
	if err != nil || freshSub == nil {
		slog.Error("stripe: subscription.updated — subscription.Get failed, status-only update",
			"user_id", existing.UserID, "subscription_id", sub.ID, "error", err)
		status := string(sub.Status)
		if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
			Status: &status,
		}); err != nil {
			slog.Error("stripe: failed to update subscription status", "user_id", existing.UserID, "error", err)
		}
		return
	}

	// Fail-closed: unrecognised prices must not escalate the user to a paid
	// tier. Keep the stored tier unchanged and log loudly so the
	// configuration drift is visible.
	tier, ok := a.stripeTierFromSubscription(freshSub)
	if !ok {
		slog.Error("stripe: subscription.updated with malformed payload, leaving tier unchanged",
			"user_id", existing.UserID, "subscription_id", sub.ID)
		status := string(freshSub.Status)
		if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
			Status: &status,
		}); err != nil {
			slog.Error("stripe: failed to update subscription status", "user_id", existing.UserID, "error", err)
		}
		return
	}
	if _, known := a.Tiers.Get(tier); !known {
		slog.Error("stripe: subscription.updated with unknown price ID, leaving tier unchanged",
			"user_id", existing.UserID, "price_id", tier, "subscription_id", sub.ID)
		status := string(freshSub.Status)
		if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
			Status: &status,
		}); err != nil {
			slog.Error("stripe: failed to update subscription status", "user_id", existing.UserID, "error", err)
		}
		return
	}
	status := string(freshSub.Status)

	if err := a.DB.UpdateSubscription(ctx, existing.UserID, &db.SubscriptionUpdate{
		Tier:   &tier,
		Status: &status,
	}); err != nil {
		slog.Error("stripe: failed to update subscription", "user_id", existing.UserID, "error", err)
	}

	a.notifyCameraLimitExceeded(ctx, existing.UserID, tier)
}

// stripeSubID safely extracts the subscription ID from a possibly-nil
// *stripe.Subscription returned on a CheckoutSession.
func stripeSubID(sub *stripe.Subscription) string {
	if sub == nil {
		return ""
	}
	return sub.ID
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

	freeTier := billing.FreeTierID
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

// stripeTierFromSubscription extracts the Stripe price ID from the first
// line item on a Stripe subscription. Price IDs are the canonical tier
// identifier under the Stripe-driven tier model — callers should pass the
// returned string to a.Tiers.Get to validate and resolve its limits.
// Returns ok=false only when the subscription shape itself is malformed.
func (a *App) stripeTierFromSubscription(sub *stripe.Subscription) (string, bool) {
	if sub == nil || sub.Items == nil || len(sub.Items.Data) == 0 {
		return "", false
	}
	item := sub.Items.Data[0]
	if item == nil || item.Price == nil || item.Price.ID == "" {
		return "", false
	}
	return item.Price.ID, true
}

// notifyCameraLimitExceeded emits a camera_limit_exceeded SSE event if the
// user's camera count exceeds the new tier limit. Unknown tier IDs are a
// programming error (callers guarantee validity) but we bail out silently
// rather than panic — this is notification code, not an auth decision.
func (a *App) notifyCameraLimitExceeded(ctx context.Context, userID, tierID string) {
	tier, ok := a.Tiers.Get(tierID)
	if !ok {
		slog.Error("notifyCameraLimitExceeded: unknown tier", "user_id", userID, "tier", tierID)
		return
	}
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
	stored := apitypes.CameraLimitExceededEvent{
		UserID:      userID,
		CameraCount: count,
		CameraLimit: *tier.CameraLimit,
		Tier:        tierID,
	}
	payload, _ := json.Marshal(stored)
	eventID, _ := redis.WriteEvent(ctx, a.Redis, userID, "", "camera_limit_exceeded", string(payload))
	live := stored
	live.EventID = eventID
	withID, _ := json.Marshal(live)
	a.Redis.Publish(ctx, fmt.Sprintf("camera_limit_exceeded:%s", userID), withID)
	slog.Info("camera limit exceeded after tier change", "user_id", userID, "count", count, "limit", *tier.CameraLimit, "tier", tierID)
}

// migrateLegacyTier rewrites a sub row whose tier column still holds one
// of the pre-refactor hardcoded names ("starter"/"pro"/"enterprise") to
// the current Stripe price ID, and returns the updated in-memory record
// so the calling request sees the new value immediately without a second
// DB read.
//
// The migration is triggered transparently from App.effectiveTier on the
// first call for any legacy row, and happens exactly once per row — once
// the tier column carries a price ID, IsLegacyTierName returns false and
// this path isn't taken again.
//
// Failures (Stripe unreachable, malformed subscription, unknown price,
// DB write error) return nil so the caller falls through to the
// fail-closed free tier. Every failure path logs the details.
func (a *App) migrateLegacyTier(ctx context.Context, sub *db.SubscriptionRecord) *db.SubscriptionRecord {
	if !a.stripeConfigured() || sub == nil || sub.StripeSubscriptionID == nil {
		return nil
	}
	subID := *sub.StripeSubscriptionID

	stripe.Key = a.Config.StripeSecretKey

	stripeSub, err := subscription.Get(subID, nil)
	if err != nil || stripeSub == nil {
		slog.Warn("billing: legacy tier migration — stripe subscription fetch failed",
			"user_id", sub.UserID, "subscription_id", subID, "error", err)
		return nil
	}

	priceID, ok := a.stripeTierFromSubscription(stripeSub)
	if !ok {
		slog.Warn("billing: legacy tier migration — subscription has no line items",
			"user_id", sub.UserID, "subscription_id", subID)
		return nil
	}
	if _, known := a.Tiers.Get(priceID); !known {
		slog.Warn("billing: legacy tier migration — price ID not in tier cache; product metadata missing?",
			"user_id", sub.UserID, "subscription_id", subID, "price_id", priceID)
		return nil
	}

	if err := a.DB.UpdateSubscription(ctx, sub.UserID, &db.SubscriptionUpdate{
		Tier: &priceID,
	}); err != nil {
		slog.Error("billing: legacy tier migration — DB update failed",
			"user_id", sub.UserID, "subscription_id", subID, "price_id", priceID, "error", err)
		return nil
	}

	slog.Info("billing: legacy tier migrated",
		"user_id", sub.UserID,
		"subscription_id", subID,
		"from", sub.Tier,
		"to", priceID,
	)

	migrated := *sub
	migrated.Tier = priceID
	return &migrated
}

func strPtr(s string) *string { return &s }
