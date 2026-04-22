package main

import (
	"strings"

	"github.com/dirien/pulumi-fly/sdk/go/fly"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// FlyOutputs holds the outputs from Fly.io resource provisioning.
type FlyOutputs struct {
	AppName                pulumi.StringOutput
	IPv4                   pulumi.StringOutput
	IPv6                   pulumi.StringOutput
	Hostname               pulumi.StringOutput
	CertValidationHostname pulumi.StringOutput
	CertValidationTarget   pulumi.StringOutput
}

func setupFly(ctx *pulumi.Context, cfg *config.Config) (*FlyOutputs, error) {
	appName := cfg.Require("appName")
	region := cfg.Require("region")
	hostname := cfg.Require("hostname")

	app, err := fly.NewApp(ctx, "ghostcam", &fly.AppArgs{
		Name: pulumi.String(appName),
		Org:  pulumi.String("personal"),
	})
	if err != nil {
		return nil, err
	}

	_, err = fly.NewVolume(ctx, "ghostcam-data", &fly.VolumeArgs{
		App:    app.Name,
		Name:   pulumi.String("ghostcam_data"),
		Region: pulumi.String(region),
		Size:   pulumi.Int(1),
	})
	if err != nil {
		return nil, err
	}

	// Dedicated IPv4 — required for WebRTC UDP (shared IPv4 is TCP-only).
	ipv4, err := fly.NewIp(ctx, "ghostcam-ipv4", &fly.IpArgs{
		App:  app.Name,
		Type: pulumi.String("v4"),
	})
	if err != nil {
		return nil, err
	}

	ipv6, err := fly.NewIp(ctx, "ghostcam-ipv6", &fly.IpArgs{
		App:  app.Name,
		Type: pulumi.String("v6"),
	})
	if err != nil {
		return nil, err
	}

	out := &FlyOutputs{
		AppName:                app.Name,
		IPv4:                   ipv4.Address,
		IPv6:                   ipv6.Address,
		Hostname:               pulumi.String(hostname).ToStringOutput(),
		CertValidationHostname: pulumi.String("").ToStringOutput(),
		CertValidationTarget:   pulumi.String("").ToStringOutput(),
	}

	// TLS certificate — only for custom domains. Fly.dev subdomains have auto-TLS.
	if !strings.HasSuffix(hostname, ".fly.dev") {
		cert, err := fly.NewCert(ctx, "ghostcam-cert", &fly.CertArgs{
			App:      app.Name,
			Hostname: pulumi.String(hostname),
		})
		if err != nil {
			return nil, err
		}
		out.Hostname = cert.Hostname
		out.CertValidationHostname = cert.DnsValidationHostname
		out.CertValidationTarget = cert.DnsValidationTarget
	}

	return out, nil
}
