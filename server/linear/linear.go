// Package linear is a minimal GraphQL client for Linear's public API.
// Only the subset we use (issueCreate) is wired — no generated SDK.
//
// When APIKey is empty, the client runs as a no-op: CreateIssue returns
// ErrDisabled without contacting Linear. This mirrors the dev-mode
// behavior of server/mailer so local dev works with no external deps.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// DefaultEndpoint is Linear's GraphQL endpoint.
const DefaultEndpoint = "https://api.linear.app/graphql"

// ErrDisabled is returned by CreateIssue when the client has no API key.
// Callers should treat this as a soft failure and fall back to logging
// the issue locally.
var ErrDisabled = errors.New("linear client disabled (no API key)")

// Config configures the Linear client. An empty APIKey disables real
// calls and turns CreateIssue into a logging no-op.
type Config struct {
	APIKey   string
	TeamID   string // Linear team UUID — required when APIKey is set
	Endpoint string // optional override for tests; defaults to DefaultEndpoint
}

// Client is a thin HTTP wrapper around Linear's GraphQL endpoint.
type Client struct {
	apiKey     string
	teamID     string
	endpoint   string
	httpClient *http.Client
}

// CreateIssueInput is the subset of Linear's IssueCreateInput we populate.
type CreateIssueInput struct {
	Title       string
	Description string // markdown
	// Priority uses Linear's scheme: 0 (no priority), 1 (urgent),
	// 2 (high), 3 (medium), 4 (low). We clamp to [0,4].
	Priority int
}

// IssueRef identifies an issue after creation.
type IssueRef struct {
	ID         string // Linear UUID
	Identifier string // human-friendly, e.g. "ENG-123"
	URL        string // web URL
}

// New constructs a client. When cfg.APIKey is empty the client is
// disabled; CreateIssue logs + returns ErrDisabled.
func New(cfg Config) *Client {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	c := &Client{
		apiKey:     cfg.APIKey,
		teamID:     cfg.TeamID,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
	if cfg.APIKey == "" {
		slog.Info("linear disabled (no LINEAR_API_KEY), CreateIssue will log only")
	} else if cfg.TeamID == "" {
		slog.Warn("linear enabled but LINEAR_TEAM_ID is empty; CreateIssue will fail")
	} else {
		slog.Info("linear enabled", "team_id", cfg.TeamID)
	}
	return c
}

// Enabled reports whether the client has credentials to actually call Linear.
func (c *Client) Enabled() bool {
	return c.apiKey != "" && c.teamID != ""
}

// issueCreateMutation is the GraphQL mutation. Returning `identifier`
// + `url` saves us a follow-up round-trip to build the link the UI
// (and Slack, eventually) needs.
const issueCreateMutation = `
mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { id identifier url }
  }
}`

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type issueCreateResponse struct {
	Data struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"issueCreate"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// CreateIssue posts an issueCreate mutation. Returns ErrDisabled if the
// client has no API key so callers can gracefully no-op in dev.
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (IssueRef, error) {
	if c.apiKey == "" {
		slog.Info("linear dev no-op: CreateIssue",
			"title", in.Title, "priority", in.Priority)
		return IssueRef{}, ErrDisabled
	}
	if c.teamID == "" {
		return IssueRef{}, errors.New("linear: team ID not configured")
	}

	priority := in.Priority
	if priority < 0 {
		priority = 0
	}
	if priority > 4 {
		priority = 4
	}

	body := graphqlRequest{
		Query: issueCreateMutation,
		Variables: map[string]any{
			"input": map[string]any{
				"teamId":      c.teamID,
				"title":       in.Title,
				"description": in.Description,
				"priority":    priority,
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return IssueRef{}, fmt.Errorf("marshal linear request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return IssueRef{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return IssueRef{}, fmt.Errorf("linear http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return IssueRef{}, fmt.Errorf("linear read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return IssueRef{}, fmt.Errorf("linear status %d: %s", resp.StatusCode, string(raw))
	}

	var decoded issueCreateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return IssueRef{}, fmt.Errorf("linear decode: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return IssueRef{}, fmt.Errorf("linear graphql error: %s", decoded.Errors[0].Message)
	}
	if !decoded.Data.IssueCreate.Success {
		return IssueRef{}, errors.New("linear issueCreate returned success=false")
	}
	issue := decoded.Data.IssueCreate.Issue
	slog.Info("linear issue created",
		"identifier", issue.Identifier, "url", issue.URL)
	return IssueRef{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL}, nil
}
