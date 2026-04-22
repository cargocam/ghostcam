package main

import (
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// ResendOutputs holds the inbound webhook signing secret and DNS records
// needed for domain verification.
type ResendOutputs struct {
	WebhookSecret pulumi.StringOutput
	DNSRecords    pulumi.StringOutput
}

func setupResend(ctx *pulumi.Context, cfg *config.Config, fly *FlyOutputs) (*ResendOutputs, error) {
	hostname := cfg.Require("hostname")
	apiKey := cfg.RequireSecret("resendApiKey")

	// ── Domain (idempotent: check if already registered) ──────────────
	domain, err := local.NewCommand(ctx, "resend-domain", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`EXISTING=$(curl -sf https://api.resend.com/domains `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`| jq -r '.data[] | select(.name == "%s") | .id' | head -1); `+
				`if [ -n "$EXISTING" ]; then `+
				`curl -sf "https://api.resend.com/domains/$EXISTING" `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`| jq -c '.records // []'; `+
				`else `+
				`curl -sf -X POST https://api.resend.com/domains `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`-H "Content-Type: application/json" `+
				`-d '{"name":"%s"}' `+
				`| jq -c '.records // []'; `+
				`fi`,
			hostname, hostname,
		),
		Environment: pulumi.StringMap{"RESEND_API_KEY": apiKey},
	}, pulumi.RetainOnDelete(true))
	if err != nil {
		return nil, err
	}

	// ── Inbound webhook (idempotent: check if endpoint already exists) ─
	webhook, err := local.NewCommand(ctx, "resend-webhook", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`EXISTING=$(curl -sf https://api.resend.com/webhooks `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`| jq -r '.data[] | select(.endpoint == "https://%s/api/v1/webhooks/resend") | .id' | head -1); `+
				`if [ -n "$EXISTING" ]; then echo "existing"; else `+
				`curl -sf -X POST https://api.resend.com/webhooks `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`-H "Content-Type: application/json" `+
				`-d '{"endpoint":"https://%s/api/v1/webhooks/resend","events":["email.received"]}' `+
				`| jq -r '.signing_secret'; fi`,
			hostname, hostname,
		),
		Environment: pulumi.StringMap{"RESEND_API_KEY": apiKey},
	}, pulumi.RetainOnDelete(true))
	if err != nil {
		return nil, err
	}

	return &ResendOutputs{
		WebhookSecret: webhook.Stdout,
		DNSRecords:    domain.Stdout,
	}, nil
}
