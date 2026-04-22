package main

import (
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// NeonOutputs holds the Postgres connection URI from Neon.
type NeonOutputs struct {
	DatabaseURL pulumi.StringOutput
}

func setupNeon(ctx *pulumi.Context, cfg *config.Config) (*NeonOutputs, error) {
	neonRegion := cfg.Get("neonRegion")
	if neonRegion == "" {
		neonRegion = "aws-us-west-2"
	}
	appName := cfg.Require("appName")

	// Idempotent: if a project with this name exists, return its connection URI.
	// Otherwise create a new one.
	cmd, err := local.NewCommand(ctx, "neon-project", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`EXISTING=$(curl -sf "https://console.neon.tech/api/v2/projects" `+
				`-H "Authorization: Bearer $NEON_API_KEY" `+
				`| jq -r '.projects[] | select(.name == "%s") | .id' | head -1); `+
				`if [ -n "$EXISTING" ]; then `+
				`curl -sf "https://console.neon.tech/api/v2/projects/$EXISTING/connection_uri" `+
				`-H "Authorization: Bearer $NEON_API_KEY" `+
				`| jq -r '.uri'; `+
				`else `+
				`curl -sf -X POST "https://console.neon.tech/api/v2/projects" `+
				`-H "Authorization: Bearer $NEON_API_KEY" `+
				`-H "Content-Type: application/json" `+
				`-d '{"project":{"name":"%s","region_id":"%s","pg_version":16}}' `+
				`| jq -r '.connection_uris[0].connection_uri'; `+
				`fi`,
			appName, appName, neonRegion,
		),
		Environment: pulumi.StringMap{
			"NEON_API_KEY": cfg.RequireSecret("neonApiKey"),
		},
	}, pulumi.RetainOnDelete(true))
	if err != nil {
		return nil, err
	}

	return &NeonOutputs{
		DatabaseURL: cmd.Stdout,
	}, nil
}
