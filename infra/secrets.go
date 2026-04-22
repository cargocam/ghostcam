package main

import (
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// setupSecrets wires all env vars into Fly via `fly secrets import --stage`.
// --stage sets secrets without triggering a redeploy; the next `flyctl deploy`
// picks them up.
func setupSecrets(
	ctx *pulumi.Context,
	cfg *config.Config,
	fly *FlyOutputs,
	neon *NeonOutputs,
	redis *RedisOutputs,
	storage *StorageOutputs,
	stripe *StripeOutputs,
	resend *ResendOutputs,
) error {
	retentionDays := cfg.Get("segmentRetentionDays")
	if retentionDays == "" {
		retentionDays = "30"
	}

	// Collect all values — some from infrastructure outputs, some from config.
	payload := pulumi.All(
		neon.DatabaseURL,
		redis.URL,
		storage.Bucket,
		storage.Endpoint,
		storage.AccessKeyID,
		storage.SecretAccessKey,
		fly.IPv4,
		stripe.WebhookSecret,
		stripe.PortalConfigID,
		resend.WebhookSecret,
		cfg.RequireSecret("stripeSecretKey"),
		cfg.RequireSecret("adminEmail"),
		cfg.RequireSecret("adminPassword"),
		cfg.Require("publicUrl"),
		cfg.RequireSecret("resendApiKey"),
		cfg.Require("resendFromEmail"),
		cfg.Get("resendReplyTo"),
		cfg.RequireSecret("githubWebhookSecret"),
		cfg.GetSecret("githubToken"),
		cfg.GetSecret("anthropicApiKey"),
		cfg.GetSecret("linearApiKey"),
		cfg.Get("linearTeamId"),
	).ApplyT(func(args []interface{}) string {
		return fmt.Sprintf(
			"GHOSTCAM_DATABASE_URL=%s\n"+
				"GHOSTCAM_REDIS_URL=%s\n"+
				"GHOSTCAM_S3_BUCKET=%s\n"+
				"GHOSTCAM_S3_ENDPOINT=%s\n"+
				"GHOSTCAM_S3_REGION=auto\n"+
				"AWS_ACCESS_KEY_ID=%s\n"+
				"AWS_SECRET_ACCESS_KEY=%s\n"+
				"GHOSTCAM_PUBLIC_URL=%s\n"+
				"GHOSTCAM_PUBLIC_IP=%s\n"+
				"GHOSTCAM_SEGMENT_RETENTION_DAYS=%s\n"+
				"GHOSTCAM_ADMIN_EMAIL=%s\n"+
				"GHOSTCAM_ADMIN_PASSWORD=%s\n"+
				"STRIPE_SECRET_KEY=%s\n"+
				"STRIPE_WEBHOOK_SECRET=%s\n"+
				"STRIPE_PORTAL_CONFIG_ID=%s\n"+
				"GITHUB_WEBHOOK_SECRET=%s\n"+
				"GITHUB_TOKEN=%s\n"+
				"RESEND_API_KEY=%s\n"+
				"RESEND_FROM_EMAIL=%s\n"+
				"RESEND_REPLY_TO=%s\n"+
				"RESEND_WEBHOOK_SECRET=%s\n"+
				"ANTHROPIC_API_KEY=%s\n"+
				"LINEAR_API_KEY=%s\n"+
				"LINEAR_TEAM_ID=%s",
			args[0],  // DATABASE_URL
			args[1],  // REDIS_URL
			args[2],  // S3_BUCKET
			args[3],  // S3_ENDPOINT
			args[4],  // ACCESS_KEY_ID
			args[5],  // SECRET_ACCESS_KEY
			args[13], // PUBLIC_URL
			args[6],  // PUBLIC_IP (IPv4)
			retentionDays,
			args[11], // ADMIN_EMAIL
			args[12], // ADMIN_PASSWORD
			args[10], // STRIPE_SECRET_KEY
			args[7],  // STRIPE_WEBHOOK_SECRET
			args[8],  // STRIPE_PORTAL_CONFIG_ID
			args[17], // GITHUB_WEBHOOK_SECRET
			args[18], // GITHUB_TOKEN
			args[14], // RESEND_API_KEY
			args[15], // RESEND_FROM_EMAIL
			args[16], // RESEND_REPLY_TO
			args[9],  // RESEND_WEBHOOK_SECRET
			args[19], // ANTHROPIC_API_KEY
			args[20], // LINEAR_API_KEY
			args[21], // LINEAR_TEAM_ID
		)
	}).(pulumi.StringOutput)

	// Skip secrets import if secrets are already managed externally
	// (e.g. during initial adoption of existing infrastructure).
	// Set `pulumi config set ghostcam-infra:skipSecrets true` to skip.
	if cfg.Get("skipSecrets") == "true" {
		return nil
	}

	// Pipe secrets into flyctl via stdin to avoid exposing values in /proc/cmdline.
	_, err := local.NewCommand(ctx, "fly-secrets", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`printf '%%s' '%s' | flyctl secrets import --app %s --stage`,
			payload, fly.AppName,
		),
	})
	return err
}
