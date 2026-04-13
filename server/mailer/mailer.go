// Package mailer provides transactional email sending via Resend.
// When the API key is empty, all sends are logged but not dispatched,
// so dev/local environments work without any Resend configuration.
package mailer

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/resend/resend-go/v2"
)

// Config holds the Resend configuration. An empty APIKey disables
// real sends — the client logs what it would have sent instead.
type Config struct {
	APIKey    string
	From      string // e.g. "Ghostcam <noreply@ghostcam.app>"
	ReplyTo   string // optional
	PublicURL string // used to construct links in templates
}

// Client is the mailer. Use New to construct one.
type Client struct {
	api       *resend.Client // nil when disabled
	from      string
	replyTo   string
	publicURL string
}

// New creates a mailer client. If cfg.APIKey is empty the client operates
// in dev mode — every send logs the template name, recipient, and full
// rendered body so developers can copy links and codes from the terminal.
func New(cfg Config) *Client {
	c := &Client{
		from:      cfg.From,
		replyTo:   cfg.ReplyTo,
		publicURL: cfg.PublicURL,
	}
	if cfg.APIKey != "" {
		c.api = resend.NewClient(cfg.APIKey)
		slog.Info("mailer enabled (Resend)", "from", cfg.From)
	} else {
		slog.Info("mailer disabled (no RESEND_API_KEY), emails will be logged")
	}
	return c
}

// send dispatches a single email. In dev mode (no API key) it logs the
// full rendered body so developers can extract links and OTP codes.
func (c *Client) send(ctx context.Context, to, subject, html, text string) error {
	if c.api == nil {
		slog.Info("mail sent (dev no-op)",
			"to", to,
			"subject", subject,
			"text_body", text,
		)
		return nil
	}

	params := &resend.SendEmailRequest{
		From:    c.from,
		To:      []string{to},
		Subject: subject,
		Html:    html,
		Text:    text,
	}
	if c.replyTo != "" {
		params.ReplyTo = c.replyTo
	}

	_, err := c.api.Emails.SendWithContext(ctx, params)
	if err != nil {
		slog.Error("mail send failed", "to", to, "subject", subject, "error", err)
		return fmt.Errorf("send email to %s: %w", to, err)
	}
	slog.Info("mail sent", "to", to, "subject", subject)
	return nil
}

// renderTemplate renders both HTML and text templates for the given name,
// returning the rendered strings.
func (c *Client) renderTemplate(name string, data any) (html, text string, err error) {
	var htmlBuf, textBuf bytes.Buffer

	htmlTpl := htmlTemplates.Lookup(name + ".html")
	if htmlTpl == nil {
		return "", "", fmt.Errorf("html template %q not found", name)
	}
	if err := htmlTpl.Execute(&htmlBuf, data); err != nil {
		return "", "", fmt.Errorf("render html template %q: %w", name, err)
	}

	textTpl := textTemplates.Lookup(name + ".txt")
	if textTpl == nil {
		return "", "", fmt.Errorf("text template %q not found", name)
	}
	if err := textTpl.Execute(&textBuf, data); err != nil {
		return "", "", fmt.Errorf("render text template %q: %w", name, err)
	}

	return htmlBuf.String(), textBuf.String(), nil
}

// --- Typed send methods ---

// VerifyEmailData is the template data for the verify-email email.
type VerifyEmailData struct {
	DisplayName string
	Link        string
}

// SendVerifyEmail sends an email-verification link.
func (c *Client) SendVerifyEmail(ctx context.Context, to string, data VerifyEmailData) error {
	data.Link = c.publicURL + "/verify-email?token=" + data.Link
	html, text, err := c.renderTemplate("verify_email", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Verify your email", html, text)
}

// PasswordResetData is the template data for password-reset emails.
type PasswordResetData struct {
	DisplayName string
	Link        string
}

// SendPasswordReset sends a password-reset link.
func (c *Client) SendPasswordReset(ctx context.Context, to string, data PasswordResetData) error {
	data.Link = c.publicURL + "/reset-password?token=" + data.Link
	html, text, err := c.renderTemplate("password_reset", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Reset your password", html, text)
}

// PasswordChangedData is the template data for password-changed notifications.
type PasswordChangedData struct {
	DisplayName string
}

// SendPasswordChanged sends a notification that the password was changed.
func (c *Client) SendPasswordChanged(ctx context.Context, to string, data PasswordChangedData) error {
	html, text, err := c.renderTemplate("password_changed", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Your password was changed", html, text)
}

// EmailChangeConfirmData is the template data for email-change confirmation.
type EmailChangeConfirmData struct {
	DisplayName string
	NewEmail    string
	Link        string
}

// SendEmailChangeConfirm sends a confirmation link to the new email address.
func (c *Client) SendEmailChangeConfirm(ctx context.Context, to string, data EmailChangeConfirmData) error {
	data.Link = c.publicURL + "/email-change-confirm?token=" + data.Link
	html, text, err := c.renderTemplate("email_change_confirm", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Confirm your new email", html, text)
}

// EmailChangedNoticeData is the template data for notifying the old address.
type EmailChangedNoticeData struct {
	DisplayName string
	NewEmail    string
}

// SendEmailChangedNotice notifies the old email that the address was changed.
func (c *Client) SendEmailChangedNotice(ctx context.Context, to string, data EmailChangedNoticeData) error {
	html, text, err := c.renderTemplate("email_changed_notice", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Your email address was changed", html, text)
}

// InviteData is the template data for the invite email.
type InviteData struct {
	DisplayName string
	Link        string
}

// SendInvite sends an invitation email with a set-password link.
func (c *Client) SendInvite(ctx context.Context, to string, data InviteData) error {
	data.Link = c.publicURL + "/reset-password?token=" + data.Link
	html, text, err := c.renderTemplate("invite", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "You've been invited to Ghostcam", html, text)
}

// LoginOTPData is the template data for login OTP codes.
type LoginOTPData struct {
	Code string
}

// SendLoginOTP sends a 6-digit OTP code for passwordless login.
func (c *Client) SendLoginOTP(ctx context.Context, to string, data LoginOTPData) error {
	html, text, err := c.renderTemplate("login_otp", data)
	if err != nil {
		return err
	}
	return c.send(ctx, to, "Your login code", html, text)
}
