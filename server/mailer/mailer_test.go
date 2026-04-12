package mailer

import (
	"strings"
	"testing"
)

func TestTemplatesRender(t *testing.T) {
	c := New(Config{
		PublicURL: "https://cam.example.com",
	})

	tests := []struct {
		name     string
		template string
		data     any
		wantHTML []string // substrings expected in the HTML
		wantText []string // substrings expected in the text
	}{
		{
			name:     "verify_email",
			template: "verify_email",
			data: VerifyEmailData{
				DisplayName: "Alice",
				Link:        "https://cam.example.com/verify-email?token=abc123",
			},
			wantHTML: []string{"Alice", "abc123", "Verify"},
			wantText: []string{"Alice", "abc123"},
		},
		{
			name:     "password_reset",
			template: "password_reset",
			data: PasswordResetData{
				DisplayName: "Bob",
				Link:        "https://cam.example.com/reset-password?token=xyz",
			},
			wantHTML: []string{"Bob", "xyz", "Reset"},
			wantText: []string{"Bob", "xyz"},
		},
		{
			name:     "password_changed",
			template: "password_changed",
			data: PasswordChangedData{
				DisplayName: "Charlie",
			},
			wantHTML: []string{"Charlie", "changed"},
			wantText: []string{"Charlie", "changed"},
		},
		{
			name:     "email_change_confirm",
			template: "email_change_confirm",
			data: EmailChangeConfirmData{
				DisplayName: "Dave",
				NewEmail:    "dave@new.com",
				Link:        "https://cam.example.com/email-change-confirm?token=tok",
			},
			wantHTML: []string{"Dave", "dave@new.com", "tok"},
			wantText: []string{"Dave", "dave@new.com", "tok"},
		},
		{
			name:     "email_changed_notice",
			template: "email_changed_notice",
			data: EmailChangedNoticeData{
				DisplayName: "Eve",
				NewEmail:    "eve@new.com",
			},
			wantHTML: []string{"Eve", "eve@new.com"},
			wantText: []string{"Eve", "eve@new.com"},
		},
		{
			name:     "login_otp",
			template: "login_otp",
			data: LoginOTPData{
				Code: "017329",
			},
			wantHTML: []string{"017329"},
			wantText: []string{"017329"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html, text, err := c.renderTemplate(tt.template, tt.data)
			if err != nil {
				t.Fatalf("renderTemplate(%q): %v", tt.template, err)
			}
			if html == "" {
				t.Error("HTML output is empty")
			}
			if text == "" {
				t.Error("text output is empty")
			}
			for _, want := range tt.wantHTML {
				if !strings.Contains(html, want) {
					t.Errorf("HTML missing %q", want)
				}
			}
			for _, want := range tt.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("text missing %q", want)
				}
			}
		})
	}
}

func TestNewDisabledClient(t *testing.T) {
	c := New(Config{})
	if c.api != nil {
		t.Error("expected nil API client when no key is set")
	}
}
