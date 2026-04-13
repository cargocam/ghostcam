package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/triage"
)

// makeSvixSig builds a valid svix-signature header for the given key
// and signed content, using the same construction as Svix/Resend.
func makeSvixSig(key []byte, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte{'.'})
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyResendSignature(t *testing.T) {
	key := []byte("super-secret-key-bytes")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	id := "msg_2abc"
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"email.received","data":{"from":"a@b"}}`)
	goodSig := makeSvixSig(key, id, ts, body)

	tests := []struct {
		name       string
		id, ts, sig string
		body       []byte
		secret     string
		now        time.Time
		want       bool
	}{
		{"valid signature", id, ts, goodSig, body, secret, now, true},
		{"valid with multiple signatures", id, ts, "v1,bogus " + goodSig, body, secret, now, true},
		{"missing id", "", ts, goodSig, body, secret, now, false},
		{"missing timestamp", id, "", goodSig, body, secret, now, false},
		{"missing signature", id, ts, "", body, secret, now, false},
		{"missing secret", id, ts, goodSig, body, "", now, false},
		{"non-integer timestamp", id, "not-a-number", goodSig, body, secret, now, false},
		{"stale timestamp", id, strconv.FormatInt(now.Unix()-400, 10), goodSig, body, secret, now, false},
		{"future timestamp", id, strconv.FormatInt(now.Unix()+400, 10), goodSig, body, secret, now, false},
		{"secret missing whsec prefix", id, ts, goodSig, body, base64.StdEncoding.EncodeToString(key), now, false},
		{"tampered body", id, ts, goodSig, []byte(`{"type":"email.tampered"}`), secret, now, false},
		{"wrong secret", id, ts, goodSig, body, "whsec_" + base64.StdEncoding.EncodeToString([]byte("other")), now, false},
		{"no v1 token", id, ts, "v0," + strings.TrimPrefix(goodSig, "v1,"), body, secret, now, false},
		{"garbage signature", id, ts, "v1,notbase64!!!!", body, secret, now, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyResendSignature(tc.id, tc.ts, tc.sig, tc.body, tc.secret, tc.now)
			if got != tc.want {
				t.Errorf("verifyResendSignature = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRawFallback(t *testing.T) {
	tests := []struct {
		name          string
		in            db.SupportTicket
		wantTitle     string
		wantPriority  int
		wantCategory  string
		descContains  []string
	}{
		{
			name: "normal subject + body",
			in: db.SupportTicket{
				FromEmail: "alice@example.com",
				Subject:   "Camera keeps rebooting",
				BodyText:  "It reboots every 5 minutes.\nHelp!",
			},
			wantTitle:    "Camera keeps rebooting",
			wantPriority: 3,
			wantCategory: triage.CategoryOther,
			descContains: []string{
				"**From:** alice@example.com",
				"> It reboots every 5 minutes.",
				"> Help!",
			},
		},
		{
			name: "empty subject falls back to from-email title",
			in: db.SupportTicket{
				FromEmail: "bob@example.com",
				Subject:   "",
				BodyText:  "question",
			},
			wantTitle:    "Support email from bob@example.com",
			wantPriority: 3,
			wantCategory: triage.CategoryOther,
			descContains: []string{"**From:** bob@example.com", "> question"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRawFallback(tc.in)
			if got.Title != tc.wantTitle {
				t.Errorf("Title = %q; want %q", got.Title, tc.wantTitle)
			}
			if got.Priority != tc.wantPriority {
				t.Errorf("Priority = %d; want %d", got.Priority, tc.wantPriority)
			}
			if got.Category != tc.wantCategory {
				t.Errorf("Category = %q; want %q", got.Category, tc.wantCategory)
			}
			for _, want := range tc.descContains {
				if !strings.Contains(got.Description, want) {
					t.Errorf("Description missing %q; got:\n%s", want, got.Description)
				}
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"<p>Hello <b>world</b></p>", "Hello world"},
		{"<div>line1<br/>line2</div>", "line1 line2"},
		{"  plain text  ", "plain text"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := stripHTML(tc.in); got != tc.want {
			t.Errorf("stripHTML(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
