# Infrastructure

Production infrastructure is managed as code with [Pulumi](https://www.pulumi.com/) (Go) under `infra/`. Pulumi provisions all backing services and wires their credentials into Fly secrets. Application deployment stays with `flyctl deploy`.

## Prerequisites

- [Pulumi CLI](https://www.pulumi.com/docs/install/) (`brew install pulumi`)
- [Fly CLI](https://fly.io/docs/hands-on/install-flyctl/) (`brew install flyctl`)
- `jq` (`brew install jq`) — used by resource provisioning commands
- A [Pulumi Cloud](https://app.pulumi.com/) account (free tier, stores state + encrypted secrets)
- Accounts with: [Fly.io](https://fly.io), [Neon](https://neon.tech), [Stripe](https://stripe.com), [Resend](https://resend.com)

## What Pulumi manages

| Resource | Provider | Notes |
|----------|----------|-------|
| Fly app, volume, dedicated IPv4, IPv6, TLS cert | `flyctl` | Dedicated IPv4 required for WebRTC UDP |
| Neon Postgres project | Neon REST API | Outputs `connection_uri` with role + password |
| Upstash Redis | `flyctl redis` | Free tier via Fly integration |
| Tigris S3 bucket | `flyctl storage` | Outputs access key + secret |
| Stripe products (3), prices (3), webhook endpoint, portal config | Stripe REST API | RetainOnDelete — `pulumi destroy` won't delete billing config |
| Resend sending domain, inbound webhook | Resend REST API | Outputs DNS records for domain verification |
| Fly secrets (23 env vars) | `flyctl secrets import` | `--stage` flag: no redeploy until next `flyctl deploy` |

## What Pulumi does NOT manage

- **Application deployment** — `flyctl deploy` (unchanged, runs in CI)
- **DNS records** — add A/AAAA records manually from `pulumi stack output ipv4` / `ipv6`
- **Database migrations** — auto-run on server startup (see `server/db/migrations.go`)
- **Resend domain verification** — after `pulumi up`, add the DNS records from `pulumi stack output resendDNSRecords` to your DNS provider

## From-scratch setup

### 1. Authenticate CLIs

```bash
pulumi login                     # Pulumi Cloud (stores state + secrets)
flyctl auth login                # Fly.io
```

### 2. Initialize the stack

```bash
cd infra
pulumi stack init prod
```

### 3. Set config values

Plain-text config is already in `Pulumi.prod.yaml`. Edit it if you need a different app name, region, or pricing.

Set the Fly API token as an environment variable (the Fly provider reads it directly):

```bash
export FLY_API_TOKEN=$(flyctl auth token)
```

Set encrypted secrets (required):

```bash
pulumi config set --secret neonApiKey          <your-neon-api-key>
pulumi config set --secret stripeSecretKey     sk_live_...
pulumi config set --secret adminEmail          admin@yourdomain.com
pulumi config set --secret adminPassword       <strong-password>
pulumi config set --secret resendApiKey         re_...
pulumi config set --secret githubWebhookSecret  <random-string>
```

Upstash Redis (optional — `flyctl redis create` is interactive, so the URL is provided manually):

```bash
flyctl redis create                          # follow interactive prompts
flyctl redis status <name>                   # copy the Private URL
pulumi config set --secret redisUrl redis://default:...@fly-NAME.upstash.io:6379
```

If omitted, the server runs without Redis (telemetry streams and SSE disabled).

Optional secrets (features degrade gracefully without these):

```bash
pulumi config set --secret githubToken      ghp_...       # private release asset downloads
pulumi config set --secret anthropicApiKey  sk-ant-...    # support email triage
pulumi config set --secret linearApiKey     lin_api_...   # support ticket creation
pulumi config set --secret linearTeamId     <uuid>        # Linear team for tickets
```

### 4. Preview and apply

```bash
pulumi preview     # dry-run — shows what will be created
pulumi up          # provisions everything
```

This creates (in order):
1. Fly app + volume + dedicated IPs + TLS cert
2. Neon Postgres project
3. Upstash Redis instance
4. Tigris S3 bucket
5. Stripe products, prices, webhook endpoint, portal config
6. Resend sending domain + inbound webhook
7. All 23 env vars piped into `flyctl secrets import --stage`

### 5. Configure DNS

After `pulumi up`, add these records at your DNS provider:

```bash
# Get the IPs
pulumi stack output ipv4    # → A record for ghostcam.app
pulumi stack output ipv6    # → AAAA record for ghostcam.app

# If Fly shows cert validation records:
pulumi stack output certValidationHostname   # → CNAME name
pulumi stack output certValidationTarget     # → CNAME value

# Resend domain verification (SPF, DKIM):
pulumi stack output resendDNSRecords         # → JSON array of DNS records
```

### 6. Deploy the application

```bash
flyctl deploy --remote-only
```

The server starts, runs migrations against the Neon database, and serves on the configured hostname.

## Day-to-day operations

### Changing config

```bash
cd infra

# Change a plain-text value
# Edit Pulumi.prod.yaml directly, then:
pulumi up

# Change a secret
pulumi config set --secret stripeSecretKey sk_live_new_key_...
pulumi up
```

`pulumi up` re-imports secrets with `--stage` — the new values take effect on the next `flyctl deploy`.

### Viewing current state

```bash
pulumi stack output                          # all outputs
pulumi stack output databaseUrl --show-secrets  # secret output
pulumi stack                                 # resource count, last update
```

### Tearing down

```bash
pulumi destroy
```

Resources with `RetainOnDelete` (Neon DB, Stripe config, Resend domain) are **not** destroyed — they must be cleaned up manually in their respective dashboards. This is intentional: you don't want `pulumi destroy` to drop your production database or delete Stripe products.

## CI integration

The `infra` job in `.github/workflows/ci.yml` runs on every push and PR:

- **PRs**: `pulumi preview` — dry-run diff posted as a PR comment (`comment-on-pr: true`)
- **Main push**: `pulumi up` — applies changes, then `deploy` job runs `flyctl deploy`

**Required GitHub Actions secrets:**

| Secret | Where to get it |
|--------|----------------|
| `PULUMI_ACCESS_TOKEN` | [Pulumi Cloud](https://app.pulumi.com/) → Access Tokens |
| `FLY_API_TOKEN` | `flyctl auth token` |

Neon, Stripe, and Resend API keys are read from Pulumi encrypted config (decrypted via
`PULUMI_ACCESS_TOKEN`), not from CI environment variables.

## Adopting existing infrastructure

If you already have a running Ghostcam instance provisioned manually, you can import existing resources into Pulumi state instead of recreating them:

```bash
cd infra
pulumi stack init prod

# Import the Fly app (resource type, logical name, physical ID)
pulumi import command:local:Command fly-app ghostcam

# For Command resources that wrap external services, set Create to a
# no-op that outputs the existing connection strings, then run:
pulumi up
pulumi preview   # should show no changes
```

The goal is `pulumi preview` showing zero diff — state matches reality. From that point, all changes go through `pulumi up`.

## Architecture notes

- **Command provider only** — all resources use `pulumi-command/local.Command` wrapping `flyctl` and `curl`+`jq`. Community Pulumi providers for Fly, Neon, and Stripe have unstable Go SDKs; the CLI/REST APIs are the first-class interfaces.
- **Separate module** — `infra/go.mod` is independent from the root `go.mod`. It only depends on `pulumi/sdk/v3` and `pulumi-command/sdk`.
- **Secrets via stdin** — `flyctl secrets import` reads from stdin, avoiding exposure in `/proc/*/cmdline`.
- **Infra vs deploy** — Pulumi manages long-lived infrastructure. `flyctl deploy` ships the application. This is the standard split: provisioning and deployment are separate concerns.
