package triage

import (
	"strings"
	"testing"
)

func TestParseClassifierResponse(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantErr      bool
		wantCategory string
		wantPriority int
		wantTitle    string
	}{
		{
			name:         "valid bug_report",
			raw:          `{"title":"App crashes on login","description":"...","priority":2,"category":"bug_report","tags":["crash","login"]}`,
			wantCategory: "bug_report",
			wantPriority: 2,
			wantTitle:    "App crashes on login",
		},
		{
			name:         "unknown category coerced to other",
			raw:          `{"title":"hi","description":".","priority":3,"category":"urgent_request","tags":[]}`,
			wantCategory: "other",
			wantPriority: 3,
			wantTitle:    "hi",
		},
		{
			name:         "priority out of range clamps to 3",
			raw:          `{"title":"hi","description":".","priority":99,"category":"support_question","tags":[]}`,
			wantCategory: "support_question",
			wantPriority: 3,
			wantTitle:    "hi",
		},
		{
			name:         "priority zero on non-spam promotes to 3",
			raw:          `{"title":"hi","description":".","priority":0,"category":"support_question","tags":[]}`,
			wantCategory: "support_question",
			wantPriority: 3,
			wantTitle:    "hi",
		},
		{
			name:         "priority zero on spam preserved",
			raw:          `{"title":"win","description":".","priority":0,"category":"spam","tags":[]}`,
			wantCategory: "spam",
			wantPriority: 0,
			wantTitle:    "win",
		},
		{
			name:         "category trimmed + lowercased",
			raw:          `{"title":"hi","description":".","priority":3,"category":"  BILLING  ","tags":[]}`,
			wantCategory: "billing",
			wantPriority: 3,
			wantTitle:    "hi",
		},
		{
			name:         "fenced json still parses",
			raw:          "```json\n{\"title\":\"hi\",\"description\":\".\",\"priority\":3,\"category\":\"support_question\",\"tags\":[]}\n```",
			wantCategory: "support_question",
			wantPriority: 3,
			wantTitle:    "hi",
		},
		{
			name:    "not json at all errors",
			raw:     "sorry, I can't do that",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClassifierResponse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Category != tc.wantCategory {
				t.Errorf("Category = %q; want %q", got.Category, tc.wantCategory)
			}
			if got.Priority != tc.wantPriority {
				t.Errorf("Priority = %d; want %d", got.Priority, tc.wantPriority)
			}
			if got.Title != tc.wantTitle {
				t.Errorf("Title = %q; want %q", got.Title, tc.wantTitle)
			}
		})
	}
}

func TestNoopClassifierReturnsDisabled(t *testing.T) {
	c := New("")
	_, err := c.Classify(nil, "a@b", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected ErrDisabled; got %v", err)
	}
}

func TestStripFences(t *testing.T) {
	tests := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{`{"a":1}`, `{"a":1}`},
		{"   {\"a\":1}   ", `{"a":1}`},
	}
	for _, tc := range tests {
		if got := stripFences(tc.in); got != tc.want {
			t.Errorf("stripFences(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
