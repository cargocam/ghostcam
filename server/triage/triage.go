// Package triage classifies inbound support emails using the Claude
// Messages API. The only method that matters is Classifier.Classify;
// everything else is wiring.
//
// When ANTHROPIC_API_KEY is empty New returns a no-op classifier
// whose Classify always returns ErrDisabled — callers are expected to
// fall back to posting the raw email into Linear. This mirrors the
// dev-mode behavior of server/mailer.
package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ErrDisabled is returned by the no-op classifier when no API key is
// configured. Callers should treat it as a soft failure and fall back.
var ErrDisabled = errors.New("triage classifier disabled (no API key)")

// Result is the cleaned-up classification of a support email, shaped
// for direct use as a Linear issue.
type Result struct {
	Title       string   // suitable as Linear issue title
	Description string   // markdown body for Linear issue
	Priority    int      // Linear priority 1 (urgent) – 4 (low); 0 = no priority
	Category    string   // one of the whitelist below; "other" on fallback
	Tags        []string // opaque free-form tags for later filtering
}

// Category whitelist. Any model response outside this set is coerced
// to "other" so downstream consumers can rely on the value.
const (
	CategorySupport = "support_question"
	CategoryBug     = "bug_report"
	CategoryBilling = "billing"
	CategorySpam    = "spam"
	CategoryOther   = "other"
)

// validCategories is the whitelist accepted from the model. Lookups are
// O(n=5) — tiny, a map isn't worth it.
var validCategories = []string{
	CategorySupport,
	CategoryBug,
	CategoryBilling,
	CategorySpam,
	CategoryOther,
}

// Classifier is implemented by both the Anthropic-backed classifier
// and the no-op. Return (Result{}, ErrDisabled) when you mean
// "no classifier is wired; caller should use the raw email".
type Classifier interface {
	Classify(ctx context.Context, from, subject, body string) (Result, error)
}

// New constructs a classifier. An empty apiKey yields a no-op
// classifier that always returns ErrDisabled.
func New(apiKey string) Classifier {
	if apiKey == "" {
		slog.Info("triage disabled (no ANTHROPIC_API_KEY), raw-email fallback will be used")
		return noopClassifier{}
	}
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	slog.Info("triage enabled (Anthropic)", "model", anthropic.ModelClaudeHaiku4_5)
	return &anthropicClassifier{client: &client}
}

// noopClassifier is the disabled variant. Returns ErrDisabled.
type noopClassifier struct{}

func (noopClassifier) Classify(context.Context, string, string, string) (Result, error) {
	return Result{}, ErrDisabled
}

// anthropicClassifier calls the Messages API and parses the JSON
// response. The system prompt is static + prompt-cached, so repeated
// calls with different user inputs share the classifier's rubric in
// cache and only the user turn is billed at full rate.
type anthropicClassifier struct {
	client *anthropic.Client
}

// systemPrompt is the static rubric the model sees on every call.
// Kept in a const so it can be cached via CacheControlEphemeral and
// reviewed in one place without grepping.
const systemPrompt = `You are the triage step for a customer support inbox for Ghostcam, a Pi-based camera surveillance product. Classify a single inbound email and return ONLY a JSON object with this exact shape:

{
  "title": string,         // short Linear issue title, <= 80 chars, imperative voice
  "description": string,   // markdown body for the issue. Include a 1-2 sentence summary then a verbatim quote of the customer's most relevant text.
  "priority": integer,     // Linear priority: 1=urgent, 2=high, 3=medium, 4=low, 0=none
  "category": string,      // one of: support_question, bug_report, billing, spam, other
  "tags": [string]         // 0-4 short lowercase tags, no spaces
}

Rules:
- Respond with ONLY the JSON object. No prose before or after. No code fences.
- If the email is clearly spam/marketing/phishing, set category="spam", priority=4.
- Only use category="bug_report" when the customer describes a concrete malfunction (crash, wrong behavior, missing footage, etc.) — general questions are "support_question".
- Billing/pricing/subscription issues are "billing".
- Default priority is 3 unless the customer reports a site-down/data-loss class problem (then 1 or 2).`

// userTemplate is the per-message payload. Kept simple so the model
// treats it as data, not instructions.
const userTemplate = `From: %s
Subject: %s

Body:
%s`

// rawModelResponse is the JSON shape we expect the model to emit.
// Anything else is a parse failure and the caller falls back to raw.
type rawModelResponse struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    int      `json:"priority"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
}

// Classify sends a single Messages API call and parses the JSON
// response into a Result. Returns an error on transport or parse
// failure — callers should fall back to the raw email in that case.
func (c *anthropicClassifier) Classify(ctx context.Context, from, subject, body string) (Result, error) {
	userText := fmt.Sprintf(userTemplate, from, subject, body)

	msg, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userText)),
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("anthropic messages: %w", err)
	}

	raw := extractText(msg)
	if raw == "" {
		return Result{}, errors.New("anthropic: empty response")
	}

	return parseClassifierResponse(raw)
}

// extractText concatenates all text blocks in the response. In
// practice there's only one for this kind of call, but being
// defensive here costs nothing.
func extractText(msg *anthropic.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// parseClassifierResponse is the pure-function half of Classify,
// split out so tests can exercise it against canned JSON without
// touching the Anthropic SDK.
//
// Behavior:
//   - Unknown/blank category → "other".
//   - Priority clamped to [0,4]; default 3 when missing.
//   - Title/Description are returned as-is; caller is responsible for
//     any further sanitation.
func parseClassifierResponse(raw string) (Result, error) {
	// Some model variants occasionally wrap JSON in ```json fences
	// despite being told not to. Strip them best-effort before parsing.
	cleaned := stripFences(raw)

	var decoded rawModelResponse
	if err := json.Unmarshal([]byte(cleaned), &decoded); err != nil {
		return Result{}, fmt.Errorf("parse classifier json: %w", err)
	}

	cat := strings.ToLower(strings.TrimSpace(decoded.Category))
	if !isValidCategory(cat) {
		cat = CategoryOther
	}

	priority := decoded.Priority
	if priority < 0 || priority > 4 {
		priority = 3
	}
	if priority == 0 && cat != CategorySpam {
		// Treat missing priority on non-spam as "medium" so Linear
		// surfaces the ticket. Spam stays at 0/low.
		priority = 3
	}

	return Result{
		Title:       strings.TrimSpace(decoded.Title),
		Description: decoded.Description,
		Priority:    priority,
		Category:    cat,
		Tags:        decoded.Tags,
	}, nil
}

// stripFences removes leading/trailing ```json fences if the model
// ignored the "no code fences" instruction.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// drop first line (``` or ```json)
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func isValidCategory(c string) bool {
	for _, v := range validCategories {
		if v == c {
			return true
		}
	}
	return false
}
