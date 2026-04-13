package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateIssueDisabledWithoutAPIKey(t *testing.T) {
	c := New(Config{})
	_, err := c.CreateIssue(context.Background(), CreateIssueInput{Title: "hi"})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

func TestCreateIssueErrorWithoutTeamID(t *testing.T) {
	c := New(Config{APIKey: "key"})
	_, err := c.CreateIssue(context.Background(), CreateIssueInput{Title: "hi"})
	if err == nil {
		t.Fatalf("expected error for missing team id")
	}
}

func TestCreateIssueSendsExpectedMutation(t *testing.T) {
	var seen struct {
		auth  string
		vars  map[string]any
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		seen.auth = r.Header.Get("Authorization")
		seen.vars = req.Variables

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data":{"issueCreate":{"success":true,"issue":{"id":"uuid-1","identifier":"SUP-1","url":"https://linear.app/x/SUP-1"}}}
		}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "lin_123", TeamID: "team-uuid", Endpoint: srv.URL})
	ref, err := c.CreateIssue(context.Background(), CreateIssueInput{
		Title:       "Crash on boot",
		Description: "body",
		Priority:    7, // should clamp to 4
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ref.Identifier != "SUP-1" || ref.URL != "https://linear.app/x/SUP-1" {
		t.Errorf("unexpected IssueRef: %+v", ref)
	}
	if seen.auth != "lin_123" {
		t.Errorf("Authorization = %q; want %q", seen.auth, "lin_123")
	}
	input, ok := seen.vars["input"].(map[string]any)
	if !ok {
		t.Fatalf("variables.input missing or wrong type: %+v", seen.vars)
	}
	if input["teamId"] != "team-uuid" {
		t.Errorf("teamId = %v; want team-uuid", input["teamId"])
	}
	if input["title"] != "Crash on boot" {
		t.Errorf("title = %v", input["title"])
	}
	// JSON numbers decode as float64
	if p, _ := input["priority"].(float64); int(p) != 4 {
		t.Errorf("priority = %v; want 4 (clamped)", input["priority"])
	}
}

func TestCreateIssueReportsGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errors":[{"message":"bad team id"}]}`))
	}))
	defer srv.Close()

	c := New(Config{APIKey: "lin_123", TeamID: "team-uuid", Endpoint: srv.URL})
	_, err := c.CreateIssue(context.Background(), CreateIssueInput{Title: "x"})
	if err == nil {
		t.Fatal("expected error from GraphQL errors array")
	}
}
