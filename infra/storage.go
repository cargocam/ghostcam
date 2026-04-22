package main

import (
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// StorageOutputs holds S3/Tigris bucket credentials.
type StorageOutputs struct {
	Bucket         pulumi.StringOutput
	Endpoint       pulumi.StringOutput
	AccessKeyID    pulumi.StringOutput
	SecretAccessKey pulumi.StringOutput
}

func setupStorage(ctx *pulumi.Context, cfg *config.Config, fly *FlyOutputs) (*StorageOutputs, error) {
	appName := cfg.Require("appName")
	// Allow overriding the full bucket name for adopting existing infrastructure
	// (Fly auto-generates names like "broken-pond-7013").
	bucketName := cfg.Get("s3BucketName")
	if bucketName == "" {
		bucketSuffix := cfg.Get("s3BucketSuffix")
		if bucketSuffix == "" {
			bucketSuffix = "segments"
		}
		bucketName = appName + "-" + bucketSuffix
	}

	// Idempotent: if the bucket exists, output its name. Credentials for
	// existing buckets can't be re-read from flyctl, so they're provided
	// via Pulumi config when adopting existing infrastructure.
	//
	// For new buckets, flyctl storage create outputs the credentials.
	cmd, err := local.NewCommand(ctx, "tigris-bucket", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`if flyctl storage status %s >/dev/null 2>&1 || flyctl storage list 2>&1 | grep -q '%s'; then `+
				`echo "BUCKET_NAME: %s"; `+
				`echo "AWS_ENDPOINT_URL_S3: https://fly.storage.tigris.dev"; `+
				`echo "AWS_ACCESS_KEY_ID: ${TIGRIS_ACCESS_KEY_ID:-existing}"; `+
				`echo "AWS_SECRET_ACCESS_KEY: ${TIGRIS_SECRET_ACCESS_KEY:-existing}"; `+
				`else `+
				`flyctl storage create -n %s -o personal -y; `+
				`fi`,
			bucketName, bucketName, bucketName, bucketName,
		),
		Delete: pulumi.Sprintf(`flyctl storage destroy %s -y`, bucketName),
		Environment: pulumi.StringMap{
			"TIGRIS_ACCESS_KEY_ID":     cfg.GetSecret("s3AccessKeyId"),
			"TIGRIS_SECRET_ACCESS_KEY": cfg.GetSecret("s3SecretAccessKey"),
		},
	})
	if err != nil {
		return nil, err
	}

	parsed := cmd.Stdout.ApplyT(func(raw string) map[string]string {
		m := make(map[string]string)
		for _, line := range strings.Split(raw, "\n") {
			k, v, ok := strings.Cut(line, ": ")
			if ok {
				m[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		return m
	})

	bucket := parsed.ApplyT(func(v interface{}) string { return v.(map[string]string)["BUCKET_NAME"] }).(pulumi.StringOutput)
	endpoint := parsed.ApplyT(func(v interface{}) string { return v.(map[string]string)["AWS_ENDPOINT_URL_S3"] }).(pulumi.StringOutput)
	accessKey := parsed.ApplyT(func(v interface{}) string { return v.(map[string]string)["AWS_ACCESS_KEY_ID"] }).(pulumi.StringOutput)
	secretKey := parsed.ApplyT(func(v interface{}) string { return v.(map[string]string)["AWS_SECRET_ACCESS_KEY"] }).(pulumi.StringOutput)

	return &StorageOutputs{
		Bucket:         bucket,
		Endpoint:       endpoint,
		AccessKeyID:    accessKey,
		SecretAccessKey: secretKey,
	}, nil
}
