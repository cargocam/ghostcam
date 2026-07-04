//go:build linux && !synthetic

package bt

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
	"github.com/godbus/dbus/v5"
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
	// Best-effort: power on the controller before advertising. This is the
	// fix for "the Pi never shows up as an onboardable device": on a headless
	// image BlueZ often leaves hci0 Powered=false (firstboot's `rfkill
	// unblock` clears the kill switch but not the D-Bus Powered property), and
	// tinygo's Enable()/Advertisement.Start() — unlike Scan/Connect — never
	// power or even check it, so advertising silently no-ops and the
	// peripheral never appears. We power it on directly over D-Bus because the
	// tinygo adapter's bus handle is unexported.
	//
	// Chosen in the daemon, not the image, on purpose: it ships in the
	// firmware .deb and so reaches already-deployed cameras over OTA, which a
	// main.conf `AutoEnable` change (fresh-image-only) can't. The image sets
	// AutoEnable=true too, as a pre-daemon complement.
	//
	// All the BT paths log at Warn (not Debug): an un-onboardable camera can't
	// phone home, so journald → /boot/firmware/diag.log is the only forensic
	// trail for *why* BT didn't come up, and it must survive a non-verbose
	// log level.
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		slog.Warn("BT onboarding: system bus unavailable, skipping BLE channel (QR/HTTP still active)", "err", err)
		return nil, nil
	}
	defer func() { _ = bus.Close() }()

	// Power-on failure is deliberately non-fatal: if the adapter is already up
	// (e.g. AutoEnable powered it but our D-Bus write is policy-denied),
	// advertising can still succeed, so we log and press on with Enable()
	// rather than abandon the BLE channel — the pre-power-on behaviour.
	if err := ensureAdapterPowered(ctx, bus); err != nil {
		slog.Warn("BT onboarding: adapter power-on incomplete, advertising anyway (works if already up)", "err", err)
	}

	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		// hci0 might be down/rfkill-blocked. Not a hard error — QR path
		// can still succeed. Caller treats nil,err as "BT unavailable".
		slog.Warn("BT onboarding: adapter Enable() failed, skipping BLE channel", "err", err)
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
		slog.Warn("BT onboarding: AddService (GATT) failed", "err", err)
		return nil, fmt.Errorf("add GATT service: %w", err)
	}

	name := "Ghostcam-" + deviceIDPrefix
	adv := adapter.DefaultAdvertisement()
	if err := adv.Configure(bluetooth.AdvertisementOptions{
		LocalName:    name,
		ServiceUUIDs: []bluetooth.UUID{btServiceUUID},
		Interval:     bluetooth.NewDuration(100 * time.Millisecond),
	}); err != nil {
		slog.Warn("BT onboarding: advertisement Configure failed", "name", name, "err", err)
		return nil, fmt.Errorf("configure advertisement: %w", err)
	}
	if err := adv.Start(); err != nil {
		slog.Warn("BT onboarding: advertisement Start failed", "name", name, "err", err)
		return nil, fmt.Errorf("start advertising: %w", err)
	}
	defer func() { _ = adv.Stop() }()

	slog.Info("BT onboarding peripheral advertising",
		"name", name,
		"service_uuid", btServiceUUID.String(),
		"timeout", btScanTimeout)

	// Confirm advertising actually went live at the controller. Start()
	// returning nil only means BlueZ accepted RegisterAdvertisement over
	// D-Bus — it does NOT guarantee the controller is broadcasting. Reading
	// LEAdvertisingManager1.ActiveInstances back tells us whether an
	// advertisement is genuinely on the air (>=1) or silently dropped (0),
	// which is the exact ambiguity behind "the Pi doesn't show up."
	logAdvertisingState(bus)

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

// btAdapterPath is the BlueZ D-Bus object for the default controller. It
// matches tinygo's defaultAdapter ("hci0") so both operate on the same
// device.
const btAdapterPath = dbus.ObjectPath("/org/bluez/hci0")

// ensureAdapterPowered brings hci0 up to the point where advertising can
// succeed: adapter present, Powered=true, and the LE advertising manager
// exposed. It powers the adapter on if needed and polls until ready (or the
// deadline). Idempotent when the adapter is already up.
//
// It tolerates the adapter not existing yet: on a cold boot the BT
// firmware/driver can enumerate a second or two after the daemon enters
// provisioning, so a GetProperty error is treated as "not ready yet, keep
// waiting" rather than a hard failure — otherwise a transient enumeration lag
// would abandon BLE for the entire 5-minute onboarding window.
//
// Returns an error only when the adapter never becomes ready within the
// deadline (missing hardware/firmware, or a D-Bus policy that denies the
// Powered write). The caller treats that as non-fatal and still attempts to
// advertise, in case the adapter is already up.
func ensureAdapterPowered(ctx context.Context, bus *dbus.Conn) error {
	obj := bus.Object("org.bluez", btAdapterPath)

	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()

	setIssued := false
	for {
		powered, err := readBoolProp(obj, "org.bluez.Adapter1", "Powered")
		switch {
		case err != nil:
			// hci0 not present/readable yet — wait for it to enumerate.
		case !powered:
			// Issue the power-on once, then wait for BlueZ to bring it up
			// (powering is asynchronous). Re-issuing every tick would just
			// spam D-Bus and the log.
			if !setIssued {
				slog.Info("BT adapter is powered off; powering on before advertising")
				if serr := obj.SetProperty("org.bluez.Adapter1.Powered", dbus.MakeVariant(true)); serr != nil {
					return fmt.Errorf("set adapter Powered=true: %w", serr)
				}
				setIssued = true
			}
		default:
			// Powered — but BlueZ exposes LEAdvertisingManager1 slightly
			// after Powered flips, and RegisterAdvertisement against a
			// not-yet-ready manager fails. Only declare ready once we can
			// read the manager, which closes that race.
			if _, aerr := obj.GetProperty("org.bluez.LEAdvertisingManager1.SupportedInstances"); aerr == nil {
				slog.Info("BT adapter powered and advertising manager ready")
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("adapter did not become ready (powered + advertising manager) within timeout")
		case <-poll.C:
		}
	}
}

// logAdvertisingState reads back the controller + advertising-manager state
// after we've asked BlueZ to advertise, and logs it as a single diagnostic
// line. Best-effort observability only — read failures are folded into the
// logged value, never propagated, because a diagnostic read must not affect
// the onboarding flow.
//
// The load-bearing field is active_instances: BlueZ's count of live LE
// advertisements. 0 after a successful Start() means the advertisement was
// registered but never made it on the air (controller quirk, HCI error,
// out of advertising slots) — the silent-failure mode that looks exactly
// like "the Pi doesn't show up." >=1 means we're genuinely broadcasting and
// the problem is on the scanning/client side.
func logAdvertisingState(bus *dbus.Conn) {
	obj := bus.Object("org.bluez", btAdapterPath)
	get := func(iface, prop string) any {
		v, err := obj.GetProperty(iface + "." + prop)
		if err != nil {
			return fmt.Sprintf("<err: %v>", err)
		}
		return v.Value()
	}

	slog.Info("BT onboarding advertising state",
		"powered", get("org.bluez.Adapter1", "Powered"),
		"discoverable", get("org.bluez.Adapter1", "Discoverable"),
		"alias", get("org.bluez.Adapter1", "Alias"),
		"active_instances", get("org.bluez.LEAdvertisingManager1", "ActiveInstances"),
		"supported_instances", get("org.bluez.LEAdvertisingManager1", "SupportedInstances"),
	)
}

// readBoolProp reads a boolean D-Bus property, returning the read error (so
// callers can distinguish "property is false" from "couldn't read it — object
// absent / access denied"). A non-bool value coerces to false.
func readBoolProp(obj dbus.BusObject, iface, prop string) (bool, error) {
	v, err := obj.GetProperty(iface + "." + prop)
	if err != nil {
		return false, err
	}
	b, _ := v.Value().(bool)
	return b, nil
}
