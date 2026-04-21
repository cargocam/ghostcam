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

	// ── Domain ────────────────────────────────────────────────────────
	// Registers the sending domain with Resend and returns the DNS records
	// needed for SPF/DKIM verification. These are exported as a stack output
	// so the operator can add them to their DNS provider.
	domain, err := local.NewCommand(ctx, "resend-domain", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`curl -sf -X POST https://api.resend.com/domains `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`-H "Content-Type: application/json" `+
				`-d '{"name":"%s"}' `+
				`| jq -c '.records'`,
			hostname,
		),
		Environment: pulumi.StringMap{"RESEND_API_KEY": apiKey},
	}, pulumi.RetainOnDelete(true))
	if err != nil {
		return nil, err
	}

	// ── Inbound webhook ───────────────────────────────────────────────
	// Receives support emails forwarded by Resend. The signing secret (whsec_...)
	// is returned by the API and wired into Fly secrets.
	webhook, err := local.NewCommand(ctx, "resend-webhook", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`curl -sf -X POST https://api.resend.com/webhooks `+
				`-H "Authorization: Bearer $RESEND_API_KEY" `+
				`-H "Content-Type: application/json" `+
				`-d '{"endpoint":"https://%s/api/v1/webhooks/resend","events":["email.received"]}' `+
				`| jq -r '.signing_secret'`,
			hostname,
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
