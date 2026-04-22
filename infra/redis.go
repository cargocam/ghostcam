package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// RedisOutputs holds the Upstash Redis connection URL.
type RedisOutputs struct {
	URL pulumi.StringOutput
}

func setupRedis(ctx *pulumi.Context, cfg *config.Config, fly *FlyOutputs) (*RedisOutputs, error) {
	// Upstash Redis is provisioned once via `flyctl redis create` (interactive).
	// The private URL is then stored as Pulumi config:
	//
	//   flyctl redis create                       # follow prompts
	//   flyctl redis status <name>                # copy Private URL
	//   pulumi config set --secret redisUrl redis://default:...@fly-NAME.upstash.io:6379
	//
	// If empty, the server runs without Redis (telemetry/SSE disabled).
	redisURL := cfg.GetSecret("redisUrl")

	return &RedisOutputs{
		URL: pulumi.ToSecret(redisURL).(pulumi.StringOutput),
	}, nil
}
