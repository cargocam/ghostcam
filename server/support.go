package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/linear"
	"github.com/cargocam/ghostcam/server/triage"
)

// Svix (what Resend uses for webhook signing) tolerates a 5-minute
// clock skew between sender and receiver. Anything older than that we
// reject as a replay.
const resendSigMaxAge = 5 * time.Minute

// triageInFlight caps concurrent async triage goroutines so a Resend
// burst can't spawn unbounded work. Over-cap deliveries are still
// persisted with status='received' and can be reprocessed later.
//
// TODO: add an admin endpoint that re-runs triage for 'received' rows.
var triageInFlight atomic.Int32

const maxTriageInFlight = 16

// resendInboundPayload is the subset of the Resend inbound webhook
// payload we consume. Resend dispatches a JSON envelope with a `type`
// discriminator; we only act on "email.received".
type resendInboundPayload struct {
	Type string `json:"type"`
	Data struct {
		From    string            `json:"from"`
		To      []string          `json:"to"`
		Subject string            `json:"subject"`
		Text    string            `json:"text"`
		HTML    string            `json:"html"`
		Headers map[string]string `json:"headers"`
	} `json:"data"`
}

// supportAck is the 202 response body when a webhook is accepted for
// async processing.
type supportAck struct {
	Status   string `json:"status"`    // "accepted" | "duplicate" | "queued_offline"
	TicketID string `json:"ticket_id"` // echo of the svix-id
}

// ResendInboundWebhook handles POST /api/v1/webhooks/resend.
//
// Modeled on GithubWebhook: verify the signature synchronously, parse
// + dedupe, then hand off to an async goroutine and return 202. The
// DB insert is the idempotency boundary — redelivery of the same
// svix-id is a no-op.
func (a *App) ResendInboundWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20)) // 2 MiB — email bodies only
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	svixID := r.Header.Get("svix-id")
	svixTimestamp := r.Header.Get("svix-timestamp")
	svixSignature := r.Header.Get("svix-signature")

	if a.Config.ResendWebhookSecret != "" {
		if !verifyResendSignature(svixID, svixTimestamp, svixSignature, body, a.Config.ResendWebhookSecret, time.Now()) {
			slog.Warn("resend webhook signature verification failed")
			http.Error(w, "", http.StatusForbidden)
			return
		}
	} else if a.Config.PublicURL != "" {
		slog.Error("resend webhook rejected: RESEND_WEBHOOK_SECRET not configured")
		http.Error(w, "", http.StatusForbidden)
		return
	}

	var payload resendInboundPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	// Resend fires several event types from the same webhook URL.
	// Accept non-inbound events silently so GitHub-style retries don't
	// loop on a type we simply don't care about.
	if payload.Type != "email.received" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if svixID == "" {
		// No dedupe key → reject. We require svix-id for both
		// signature verification and our ON CONFLICT guard.
		http.Error(w, "missing svix-id", http.StatusBadRequest)
		return
	}

	bodyText := payload.Data.Text
	if bodyText == "" {
		bodyText = stripHTML(payload.Data.HTML)
	}

	subject := payload.Data.Subject
	if subject == "" {
		subject = "(no subject)"
	}

	ticket := db.SupportTicket{
		ID:         svixID,
		FromEmail:  payload.Data.From,
		Subject:    subject,
		BodyText:   bodyText,
		ReceivedAt: time.Now().Unix(),
	}

	inserted, err := a.DB.InsertSupportTicket(r.Context(), ticket)
	if err != nil {
		slog.Error("resend webhook: insert failed", "svix_id", svixID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if !inserted {
		slog.Info("resend webhook: duplicate delivery, acking", "svix_id", svixID)
		writeJSON(w, http.StatusOK, supportAck{Status: "duplicate", TicketID: svixID})
		return
	}

	// Cap concurrent triage runs. Over-cap deliveries still get a
	// persisted row — they'll need manual retry (TODO noted above).
	if triageInFlight.Load() >= maxTriageInFlight {
		slog.Warn("resend webhook: triage queue saturated, deferring",
			"svix_id", svixID, "in_flight", triageInFlight.Load())
		writeJSON(w, http.StatusAccepted, supportAck{Status: "queued_offline", TicketID: svixID})
		return
	}

	triageInFlight.Add(1)
	go func(t db.SupportTicket) {
		defer triageInFlight.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		a.runTriagePipeline(ctx, t)
	}(ticket)

	writeJSON(w, http.StatusAccepted, supportAck{Status: "accepted", TicketID: svixID})
}

// runTriagePipeline runs classify → Linear → DB update. Every step is
// best-effort: if the classifier is disabled or fails we fall back to
// posting the raw email into Linear, and if Linear is disabled or
// fails we still record the result (classified or raw) against the
// ticket row with status='failed'.
func (a *App) runTriagePipeline(ctx context.Context, t db.SupportTicket) {
	result, triageErr := a.Triage.Classify(ctx, t.FromEmail, t.Subject, t.BodyText)
	if triageErr != nil {
		if !errors.Is(triageErr, triage.ErrDisabled) {
			slog.Warn("triage: classify failed, using raw fallback",
				"svix_id", t.ID, "error", triageErr)
		}
		result = buildRawFallback(t)
	}

	issue, linearErr := a.Linear.CreateIssue(ctx, linear.CreateIssueInput{
		Title:       result.Title,
		Description: result.Description,
		Priority:    result.Priority,
	})
	if linearErr != nil {
		// Mark the row as failed so an operator can see it. If the
		// error is ErrDisabled (no LINEAR_API_KEY) we still mark
		// failed — in that mode this pipeline is logging-only by
		// design and nothing has been "routed" anywhere.
		slog.Warn("triage: linear create failed",
			"svix_id", t.ID, "error", linearErr)
		if err := a.DB.UpdateTicketFailed(ctx, t.ID, linearErr.Error()); err != nil {
			slog.Error("triage: mark failed UPDATE failed",
				"svix_id", t.ID, "error", err)
		}
		return
	}

	if err := a.DB.UpdateTicketRouted(ctx, t.ID, result.Category, result.Priority, result.Title, issue.URL); err != nil {
		slog.Error("triage: routed UPDATE failed",
			"svix_id", t.ID, "error", err)
		return
	}
	slog.Info("triage: ticket routed",
		"svix_id", t.ID,
		"category", result.Category,
		"priority", result.Priority,
		"linear", issue.URL)
}

// buildRawFallback constructs a Result directly from the email when
// the AI classifier is unavailable. Title is the subject; body is the
// verbatim email text wrapped in a blockquote so the Linear issue
// reads sensibly. Priority defaults to 3 (medium) — enough to surface
// in the triage queue without pinging anyone.
func buildRawFallback(t db.SupportTicket) triage.Result {
	title := strings.TrimSpace(t.Subject)
	if title == "" {
		title = "Support email from " + t.FromEmail
	}
	desc := fmt.Sprintf("**From:** %s\n\n> %s",
		t.FromEmail,
		strings.ReplaceAll(strings.TrimSpace(t.BodyText), "\n", "\n> "))
	return triage.Result{
		Title:       title,
		Description: desc,
		Priority:    3,
		Category:    triage.CategoryOther,
	}
}

// verifyResendSignature implements Svix-compatible webhook signature
// verification. Resend ships webhooks via Svix, which signs payloads
// as `svix-id.svix-timestamp.body` with HMAC-SHA256 and a secret of
// the form `whsec_<base64>`. The header `svix-signature` contains one
// or more space-separated `v1,<base64-signature>` tokens; at least
// one must match, constant-time.
//
// Rejects:
//   - missing svix-id / timestamp / signature
//   - non-integer timestamp
//   - timestamp drift > resendSigMaxAge from `now`
//   - secret that doesn't start with `whsec_`
//   - signature header with no `v1,...` tokens
//   - no token matching the computed HMAC
func verifyResendSignature(id, timestamp, signature string, body []byte, secret string, now time.Time) bool {
	if id == "" || timestamp == "" || signature == "" || secret == "" {
		return false
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	drift := now.Unix() - ts
	if drift < 0 {
		drift = -drift
	}
	if time.Duration(drift)*time.Second > resendSigMaxAge {
		return false
	}

	key, err := decodeSvixSecret(secret)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte{'.'})
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	want := mac.Sum(nil)

	for _, tok := range strings.Split(signature, " ") {
		parts := strings.SplitN(tok, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// decodeSvixSecret strips the whsec_ prefix and base64-decodes the
// remainder. Returns the raw HMAC key. Secrets that lack the prefix
// are rejected — we don't want to be permissive here and silently
// accept a malformed config.
func decodeSvixSecret(secret string) ([]byte, error) {
	const prefix = "whsec_"
	if !strings.HasPrefix(secret, prefix) {
		return nil, errors.New("svix secret missing whsec_ prefix")
	}
	return base64.StdEncoding.DecodeString(secret[len(prefix):])
}

// stripHTML is a last-resort fallback when an email carries no plain
// text part. Not a full parser — just replaces tags with spaces and
// collapses whitespace so Linear isn't staring at raw markup.
// Classifier sees the same cleaned string, which is what we want.
func stripHTML(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	inTag := false
	for _, r := range in {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ') // preserve word boundary at tag start
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// collapse runs of whitespace
	return strings.Join(strings.Fields(b.String()), " ")
}
