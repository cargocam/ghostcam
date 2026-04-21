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
	bucketSuffix := cfg.Get("s3BucketSuffix")
	if bucketSuffix == "" {
		bucketSuffix = "segments"
	}
	bucketName := appName + "-" + bucketSuffix

	// Tigris S3-compatible object storage via Fly's integration.
	// flyctl storage create outputs KEY: VALUE lines — we capture all stdout
	// and parse it in Go.
	cmd, err := local.NewCommand(ctx, "tigris-bucket", &local.CommandArgs{
		Create: pulumi.Sprintf(
			`flyctl storage create -n %s -o personal -y`,
			bucketName,
		),
		Delete: pulumi.Sprintf(`flyctl storage destroy %s -y`, bucketName),
	})
	if err != nil {
		return nil, err
	}

	// Parse KEY: VALUE lines from flyctl output.
	// Example output:
	//   AWS_ACCESS_KEY_ID: tid_...
	//   AWS_ENDPOINT_URL_S3: https://fly.storage.tigris.dev
	//   AWS_SECRET_ACCESS_KEY: tsec_...
	//   BUCKET_NAME: ghostcam-test-segments
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
