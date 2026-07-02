# USB Composite Gadget — Bring-up & Validation (#139 / #144)

Local-first onboarding over USB: plug a laptop/phone into the Pi's OTG
port, land on the camera's local provisioning page, no drivers, no server
in the loop. This doc is the **#139 spike checklist** + the record of
platform-specific values the abstraction seam parameterizes.

> **Status: written from documented dwc2/libcomposite patterns, NOT yet
> validated on hardware.** Everything below the "Validation checklist" must
> be confirmed on a real Pi Zero 2 W with mac + Windows + Linux hosts
> before this leaves draft. The systemd units all carry
> `ConditionPathExists=/sys/class/udc` and the bring-up script no-ops when
> no UDC is present, so shipping this in an image is inert on any board
> that doesn't enumerate a peripheral controller.

## What ships here

| File | Role |
|------|------|
| `image/files/ghostcam-usb-gadget.sh` | configfs composite bring-up (RNDIS+ECM+mass-storage+ACM) + pins `usb0` to `10.55.0.1` |
| `image/files/ghostcam-usb-gadget.service` | oneshot, `Before=network-online.target`, brings the gadget up before the camera daemon |
| `image/files/ghostcam-usb-dhcp.service` + `ghostcam-usb-dnsmasq.conf` | dedicated dnsmasq on `usb0` → host gets `10.55.0.2`, gateway/DNS = camera |
| `image/files/ghostcam-usb0-unmanaged.conf` | NetworkManager keyfile drop-in so NM ignores `usb0` |
| `image/files/ghostcam-usb-modules.conf` | `modules-load.d` for `dwc2` |
| `image/files/usb-msd/SETUP.HTM` | the single file on the read-only mass-storage volume; meta-refresh to `http://10.55.0.1/` (#145) |
| `config.txt` | `dtoverlay=dwc2,dr_mode=peripheral` appended by the image layer |
| `systemd/ghostcam-camera.service` | `AmbientCapabilities=CAP_NET_BIND_SERVICE` so the non-root daemon binds `:80` |

The daemon side (the actual provisioning page + form handler) is
`camera/internal/bt/local_http.go` (`ScanLocalHTTP`), wired as a third
racer in `raceQRandBT()`. The image env sets
`GHOSTCAM_PROVISION_HTTP_ADDR=10.55.0.1:80` so it binds on the gadget link.

## Platform-specific values (the abstraction seam, #138)

These are the only board-specific bits; a CM4/CM5 variant overrides them
without touching the logic:

| Value | Zero 2 W | How it's set |
|-------|----------|--------------|
| UDC name | auto-detected (`ls /sys/class/udc`, single controller) | `GHOSTCAM_UDC` env override in `ghostcam.env` |
| dwc2 overlay | `dtoverlay=dwc2,dr_mode=peripheral` | `config.txt` append in `layer/ghostcam.yaml` |
| `modules-load` | `dwc2` | `modules-load.d/ghostcam-usb.conf` |
| gadget link IP | `10.55.0.1/24`, host `10.55.0.2` | constants in `ghostcam-usb-gadget.sh` + dnsmasq conf |

## Validation checklist (do this on hardware before un-drafting)

1. **UDC comes up.** After flashing + boot: `ls /sys/class/udc` is
   non-empty; `systemctl status ghostcam-usb-gadget` is active; `ip addr
   show usb0` shows `10.55.0.1`.
2. **`dr_mode=peripheral` doesn't break the modem.** The SIM7600 hat's USB
   path must still enumerate with the OTG port forced to peripheral. **This
   is the highest-risk interaction** — if it conflicts, fall back to
   `dr_mode=otg` and confirm the gadget still binds, or gate the overlay to
   non-cellular builds.
3. **Driver-free enumeration, per host OS** (the crux — see risk below):
   - **macOS**: binds ECM, `ghostcam.local` or `http://10.55.0.1` opens the page; no driver prompt.
   - **Windows**: binds RNDIS via the MS OS descriptors, no driver prompt; if mDNS fails, the mass-storage `SETUP.HTM` double-click works. **Check managed/AV/EDR machines don't flag the composite device.**
   - **Linux**: binds ECM (or RNDIS), DHCP lease from our dnsmasq, page opens.
4. **DHCP hands `10.55.0.2`** and gateway/DNS `10.55.0.1`; the catch-all
   `address=/#/10.55.0.1` makes any hostname resolve to the page.
5. **`/dev/ttyGS0` exists** (ACM function present) — serial rescue console
   is #149, this just proves the function coexists.
6. **End-to-end**: paste a provision token into the page over USB → camera
   enrolls → appears server-side as the same deterministic `device_id`.

## Known risk — RNDIS + ECM coexistence

The single-config RNDIS+ECM+IAD approach here is the most commonly
documented cross-OS pattern, but **some hosts get confused by two NICs in
one config**. If validation shows flaky binding, the fallback is the
**two-config** method (config 1 = RNDIS-only for Windows, config 2 =
ECM-only for mac/Linux; host picks). Record which approach actually
enumerated cleanly on all three OSes here once tested.

## Depends on

- `camera/internal/bt/local_http.go` (#141) — the provisioning server this
  link feeds. Mergeable independently; a no-op until an addr is set.
- Ordering: `ghostcam-usb-gadget` (`Before=network-online`) → camera daemon
  (`After=network-online`) so `10.55.0.1` exists before the server binds.
