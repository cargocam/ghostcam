//go:build linux && !synthetic

package main

// Bluetooth-based provisioning: advertise a custom GATT service and wait
// for a JSON payload written to the provision characteristic. Same payload
// shape as the QR code (common.QRPayload) — the BT path is a transport
// alternative, not a protocol fork.
//
// Spike validated browser → Pi delivery in spike/bt-onboarding/ (see
// README there for the wire details and architectural rationale).
//
// Runs alongside ScanQR — provisioning.go races both, first payload wins,
// loser's context is cancelled. Shared 5-minute timeout.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cargocam/ghostcam/common"
	"tinygo.org/x/bluetooth"
)

// GATT identifiers (matches spike/bt-onboarding/main.go).
var (
	btServiceUUID = bluetooth.NewUUID([16]byte{
		0x1c, 0x95, 0xd5, 0xb0, 0xc5, 0xe0, 0x4d, 0x59,
		0x9c, 0x11, 0x3e, 0x8d, 0x2d, 0x6f, 0x8a, 0x01,
	})
	btProvisionCharUUID = bluetooth.NewUUID([16]byte{
		0x1c, 0x95, 0xd5, 0xb1, 0xc5, 0xe0, 0x4d, 0x59,
		0x9c, 0x11, 0x3e, 0x8d, 0x2d, 0x6f, 0x8a, 0x01,
	})
	btStatusCharUUID = bluetooth.NewUUID([16]byte{
		0x1c, 0x95, 0xd5, 0xb2, 0xc5, 0xe0, 0x4d, 0x59,
		0x9c, 0x11, 0x3e, 0x8d, 0x2d, 0x6f, 0x8a, 0x01,
	})
)

const btScanTimeout = 5 * time.Minute

// ScanBT advertises a GATT peripheral named "Ghostcam-<deviceIDPrefix>"
// and waits for a provisioning payload written to the provision
// characteristic. Returns (nil, nil) on timeout — same semantics as
// ScanQR — so the provisioning.go race can treat both paths uniformly.
//
// The peripheral stops advertising when this function returns (success,
// timeout, or context cancel) so a successful onboard doesn't leave a
// rogue advertiser broadcasting after the daemon proceeds to capture.
func ScanBT(ctx context.Context, deviceIDPrefix string) (*common.QRPayload, error) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		// hci0 might be down/rfkill-blocked. Not a hard error — QR path
		// can still succeed. Caller treats nil,err as "BT unavailable".
		slog.Debug("BT adapter unavailable, skipping BLE provisioning", "err", err)
		return nil, nil
	}

	payloadCh := make(chan *common.QRPayload, 1)
	var statusChar bluetooth.Characteristic
	var provisionChar bluetooth.Characteristic

	svc := &bluetooth.Service{
		UUID: btServiceUUID,
		Characteristics: []bluetooth.CharacteristicConfig{
			{
				Handle: &provisionChar,
				UUID:   btProvisionCharUUID,
				Flags: bluetooth.CharacteristicWritePermission |
					bluetooth.CharacteristicWriteWithoutResponsePermission,
				WriteEvent: func(_ bluetooth.Connection, _ int, value []byte) {
					slog.Info("BT provision payload received", "bytes", len(value))
					var p common.QRPayload
					if err := json.Unmarshal(value, &p); err != nil {
						slog.Warn("BT payload not valid JSON", "err", err)
						_, _ = statusChar.Write([]byte("error: invalid JSON"))
						return
					}
					if p.Server == "" || p.Token == "" {
						slog.Warn("BT payload missing server or token")
						_, _ = statusChar.Write([]byte("error: missing server or token"))
						return
					}
					_, _ = statusChar.Write([]byte("payload accepted, provisioning"))
					select {
					case payloadCh <- &p:
					default:
						// Already have a winner; drop subsequent writes.
					}
				},
			},
			{
				Handle: &statusChar,
				UUID:   btStatusCharUUID,
				Flags:  bluetooth.CharacteristicReadPermission | bluetooth.CharacteristicNotifyPermission,
				Value:  []byte("waiting for provisioning payload"),
			},
		},
	}
	if err := adapter.AddService(svc); err != nil {
		return nil, fmt.Errorf("add GATT service: %w", err)
	}

	name := "Ghostcam-" + deviceIDPrefix
	adv := adapter.DefaultAdvertisement()
	if err := adv.Configure(bluetooth.AdvertisementOptions{
		LocalName:    name,
		ServiceUUIDs: []bluetooth.UUID{btServiceUUID},
		Interval:     bluetooth.NewDuration(100 * time.Millisecond),
	}); err != nil {
		return nil, fmt.Errorf("configure advertisement: %w", err)
	}
	if err := adv.Start(); err != nil {
		return nil, fmt.Errorf("start advertising: %w", err)
	}
	defer func() { _ = adv.Stop() }()

	slog.Info("BT onboarding peripheral advertising",
		"name", name,
		"service_uuid", btServiceUUID.String(),
		"timeout", btScanTimeout)

	scanCtx, cancel := context.WithTimeout(ctx, btScanTimeout)
	defer cancel()

	select {
	case p := <-payloadCh:
		slog.Info("BT provisioning payload accepted", "server", p.Server)
		return p, nil
	case <-scanCtx.Done():
		if !errors.Is(ctx.Err(), nil) {
			return nil, ctx.Err()
		}
		slog.Info("BT scan timed out, no payload received")
		return nil, nil
	}
}
