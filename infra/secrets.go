package main

import (
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// coalesceSecret returns the config override if set, otherwise the resource output.
// This handles resources where the API only returns a secret on creation (e.g.
// Stripe webhook signing secret) — for existing resources the Command outputs
// "existing", so the real value must come from config.
func coalesceSecret(cfg *config.Config, key string, resourceOutput pulumi.StringOutput) pulumi.StringOutput {
	override := cfg.GetSecret(key)
	return pulumi.All(override, resourceOutput).ApplyT(func(args []interface{}) string {
		if v, ok := args[0].(string); ok && v != "" {
			return v
		}
		return args[1].(string)
	}).(pulumi.StringOutput)
}

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

	// For secrets that can't be re-read from the API on existing resources,
	// config overrides take precedence over the "existing" placeholder.
	s3AccessKey := coalesceSecret(cfg, "s3AccessKeyId", storage.AccessKeyID)
	s3SecretKey := coalesceSecret(cfg, "s3SecretAccessKey", storage.SecretAccessKey)
	stripeWHSecret := coalesceSecret(cfg, "stripeWebhookSecretOverride", stripe.WebhookSecret)
	resendWHSecret := coalesceSecret(cfg, "resendWebhookSecretOverride", resend.WebhookSecret)

	payload := pulumi.All(
		neon.DatabaseURL,
		redis.URL,
		storage.Bucket,
		storage.Endpoint,
		s3AccessKey,
		s3SecretKey,
		fly.IPv4,
		stripeWHSecret,
		stripe.PortalConfigID,
		resendWHSecret,
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
		// Filter out lines with empty values to avoid overwriting
		// existing Fly secrets with blanks.
		lines := []struct{ k, v string }{
			{"GHOSTCAM_DATABASE_URL", fmt.Sprint(args[0])},
			{"GHOSTCAM_REDIS_URL", fmt.Sprint(args[1])},
			{"GHOSTCAM_S3_BUCKET", fmt.Sprint(args[2])},
			{"GHOSTCAM_S3_ENDPOINT", fmt.Sprint(args[3])},
			{"GHOSTCAM_S3_REGION", "auto"},
			{"AWS_ACCESS_KEY_ID", fmt.Sprint(args[4])},
			{"AWS_SECRET_ACCESS_KEY", fmt.Sprint(args[5])},
			{"GHOSTCAM_PUBLIC_URL", fmt.Sprint(args[13])},
			{"GHOSTCAM_PUBLIC_IP", fmt.Sprint(args[6])},
			{"GHOSTCAM_SEGMENT_RETENTION_DAYS", retentionDays},
			{"GHOSTCAM_ADMIN_EMAIL", fmt.Sprint(args[11])},
			{"GHOSTCAM_ADMIN_PASSWORD", fmt.Sprint(args[12])},
			{"STRIPE_SECRET_KEY", fmt.Sprint(args[10])},
			{"STRIPE_WEBHOOK_SECRET", fmt.Sprint(args[7])},
			{"STRIPE_PORTAL_CONFIG_ID", fmt.Sprint(args[8])},
			{"GITHUB_WEBHOOK_SECRET", fmt.Sprint(args[17])},
			{"GITHUB_TOKEN", fmt.Sprint(args[18])},
			{"RESEND_API_KEY", fmt.Sprint(args[14])},
			{"RESEND_FROM_EMAIL", fmt.Sprint(args[15])},
			{"RESEND_REPLY_TO", fmt.Sprint(args[16])},
			{"RESEND_WEBHOOK_SECRET", fmt.Sprint(args[9])},
			{"ANTHROPIC_API_KEY", fmt.Sprint(args[19])},
			{"LINEAR_API_KEY", fmt.Sprint(args[20])},
			{"LINEAR_TEAM_ID", fmt.Sprint(args[21])},
		}

		var payload string
		for _, l := range lines {
			if l.v != "" && l.v != "existing" {
				payload += l.k + "=" + l.v + "\n"
			}
		}
		return payload
	}).(pulumi.StringOutput)

	// Pipe secrets into flyctl via stdin to avoid exposing values in /proc/cmdline.
	_, err := local.NewCommand(ctx, "fly-secrets", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`printf '%%s' '%s' | flyctl secrets import --app %s --stage`,
			payload, fly.AppName,
		),
	})
	return err
}
