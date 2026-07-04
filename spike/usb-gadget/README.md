# Spike: USB composite gadget bring-up on Pi Zero 2 W (#139)

**Status: UNVALIDATED DRAFT.** None of this has run on hardware yet. It is a
starting point for the spike — a best-effort configfs bring-up plus a findings
template to fill in on a real Pi. Do **not** wire any of this into the
production image (`pi/image/layer/ghostcam.yaml`) until the findings below are
recorded; that's issue **#144**, which this spike gates.

## Goal (from #139)

Prove a **composite USB gadget** via `dwc2` + `libcomposite`/configfs with all
of these functions on the single UDC at once, enumerating **driver-free** on
macOS, Windows, and Linux:

- **USB Ethernet** — CDC-ECM (mac/Linux) **and** RNDIS (Windows) in one gadget.
- **Mass storage** — a small read-only FAT volume (carries an onboarding
  redirect page; see `make-onboarding-image.sh`).
- **Serial** — CDC-ACM (`/dev/ttyGS0`), for the eventual serial rescue console.

Downstream, the gadget brings up `usb0` at `10.55.0.1`; the camera daemon
already serves the offline provisioning page there when
`GHOSTCAM_PROVISION_HTTP_ADDR=10.55.0.1:80` is set (that half shipped in #157 /
`camera/internal/bt/local_http.go`). This spike only proves the **link layer**.

## ⚠️ The blocking hardware question (record this first)

On the **Pi Zero 2 W** — the only device profile we currently build — the
SIM7600 cellular modem is a **USB device** (`idVendor 1e0e`, `/dev/ttyUSB*`,
`cdc-wdm0`). The Zero 2 W has a **single** USB data controller (`dwc2`). USB
gadget/peripheral mode and hosting the modem are **mutually exclusive on that
one port**: you cannot be a USB *host* (for the modem) and a USB *device* (for
a laptop) simultaneously.

So the spike must answer, before #144 can proceed:

1. Does `dtoverlay=dwc2,dr_mode=otg` let the port **auto-switch** role — host
   when the modem is attached (via the HAT), device when a laptop is plugged
   in — or does forcing gadget mode kill the modem outright?
2. On the actual Waveshare SIM7600 HAT, is the modem wired to the Pi's USB data
   lines (test pads / bridge) such that it's on the same `dwc2` controller?
3. Is USB onboarding therefore only viable **before** the modem is provisioned,
   or only on Pi 4/5 (modem on a USB-A host port, gadget on the USB-C OTG
   port)?

If the answer is "mutually exclusive on Zero 2 W," USB onboarding likely
belongs on a Pi 4/5 profile (or the CM4/CM5 migration, #138), and #144 should
target that. **Record the finding in `FINDINGS.md`.**

## Files

| File | Purpose |
|------|---------|
| `ghostcam-usb-gadget.sh` | configfs bring-up (`up`) + teardown (`down`). Run manually as root during the spike. |
| `make-onboarding-image.sh` | Builds the read-only FAT mass-storage image (needs `dosfstools` + `mtools`). |
| `ghostcam-usb-gadget.service` | **Example** systemd unit for #144 to adopt — ordered `Before=network-online.target`. Not enabled here. |
| `FINDINGS.md` | Template. Fill in on hardware; its recorded values are what #144's abstraction seam parameterizes. |

## How to run the spike (on a Pi Zero 2 W)

1. Add to `/boot/firmware/config.txt` and reboot:
   ```
   dtoverlay=dwc2,dr_mode=otg
   ```
   (Try `otg` first — it's the least-invasive for the modem question above.
   If the gadget won't bind, retry with `dr_mode=peripheral` and record that
   the modem stopped enumerating.)
2. Build the mass-storage image: `sudo ./make-onboarding-image.sh`
3. Bring the gadget up: `sudo ./ghostcam-usb-gadget.sh up`
4. Plug the Pi's USB **data** port into a macOS, then Windows, then Linux host.
   For each, record in `FINDINGS.md`: did a network interface appear? Did a
   driver prompt show? Did the mass-storage volume mount? Did `/dev/ttyGS0`
   appear on the Pi and a matching serial device on the host?
5. Tear down: `sudo ./ghostcam-usb-gadget.sh down`

## Known quirks to watch for (and record)

- **RNDIS + ECM in one config**: both create a host-side network interface.
  macOS/Linux should bind ECM and ignore RNDIS; Windows should bind RNDIS (via
  the Microsoft OS descriptors this script sets) and may show ECM as an unknown
  device. Confirm what actually happens — this is the crux of "driver-free
  everywhere."
- **Function order matters**: RNDIS is linked into the config first because
  Windows binds the first interface; keep it first.
- **MS OS descriptors**: `os_desc` + `compatible_id=RNDIS` /
  `sub_compatible_id=5162001` is what stops Windows prompting for a driver.
  Verify the `functions/rndis.usb0/os_desc/interface.rndis/` paths exist on the
  running kernel (they're version-sensitive).
- **Managed Windows AV/EDR**: a composite gadget that presents RNDIS + mass
  storage + serial can trip endpoint-protection heuristics (looks like a
  "BadUSB"). Record any flags on a corporate-managed laptop.
