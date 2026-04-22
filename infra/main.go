// Package main is a Pulumi program that provisions all Ghostcam production
// infrastructure: Fly.io (compute, networking, TLS), Neon (Postgres), Upstash
// (Redis), Tigris (S3), Stripe (billing), and Resend (email). Application
// deployment remains with `flyctl deploy` — this program manages the backing
// resources and wires their credentials into Fly secrets.
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "ghostcam-infra")

		// 1. Fly.io — app, volume, dedicated IPs (WebRTC UDP), TLS cert
		flyOut, err := setupFly(ctx, cfg)
		if err != nil {
			return err
		}

		// 2. Neon Postgres — project with connection URI
		neonOut, err := setupNeon(ctx, cfg)
		if err != nil {
			return err
		}

		// 3. Upstash Redis — telemetry streams, SSE pub/sub
		redisOut, err := setupRedis(ctx, cfg, flyOut)
		if err != nil {
			return err
		}

		// 4. Tigris S3 — segment storage, firmware images
		storageOut, err := setupStorage(ctx, cfg, flyOut)
		if err != nil {
			return err
		}

		// 5. Stripe — products, prices, webhook endpoint, portal config
		stripeOut, err := setupStripe(ctx, cfg, flyOut)
		if err != nil {
			return err
		}

		// 6. Resend — sending domain, inbound webhook
		resendOut, err := setupResend(ctx, cfg, flyOut)
		if err != nil {
			return err
		}

		// 7. Wire all credentials into Fly secrets
		if err := setupSecrets(ctx, cfg, flyOut, neonOut, redisOut, storageOut, stripeOut, resendOut); err != nil {
			return err
		}

		// ── Stack outputs ─────────────────────────────────────────────
		ctx.Export("appName", flyOut.AppName)
		ctx.Export("ipv4", flyOut.IPv4)
		ctx.Export("ipv6", flyOut.IPv6)
		ctx.Export("hostname", flyOut.Hostname)
		ctx.Export("certValidationHostname", flyOut.CertValidationHostname)
		ctx.Export("certValidationTarget", flyOut.CertValidationTarget)
		ctx.Export("databaseUrl", pulumi.ToSecret(neonOut.DatabaseURL))
		ctx.Export("redisUrl", pulumi.ToSecret(redisOut.URL))
		ctx.Export("s3Bucket", storageOut.Bucket)
		ctx.Export("s3Endpoint", storageOut.Endpoint)
		ctx.Export("stripeWebhookUrl", pulumi.Sprintf("https://%s/api/v1/webhooks/stripe", cfg.Require("hostname")))
		ctx.Export("stripePortalConfigId", stripeOut.PortalConfigID)

		// Resend DNS records — add these to your DNS provider for domain verification.
		ctx.Export("resendDNSRecords", resendOut.DNSRecords)

		return nil
	})
}
