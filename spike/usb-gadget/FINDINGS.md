# USB gadget spike — findings (#139)

Fill this in while running the spike on real hardware. The recorded values are
exactly what feature **#144** (the production gadget layer) parameterizes, and
the modem-conflict answer decides whether #144 can target the Zero 2 W at all.

_Run date:_ ______  _Runner:_ ______  _Pi model:_ ______  _OS image:_ ______

## 0. The blocking question — modem vs gadget on the single USB bus

- [ ] With `dtoverlay=dwc2,dr_mode=otg`, does the SIM7600 still enumerate
      (`mmcli -L`, `/dev/ttyUSB*`, `cdc-wdm0`) when NO laptop is attached? …
- [ ] With a laptop attached, does the Pi switch to device mode and the gadget
      bind? …
- [ ] Are the modem and gadget ever usable **at the same time**, or strictly
      mutually exclusive? …
- [ ] Conclusion: is USB onboarding viable on Zero 2 W, or should #144 target
      Pi 4/5 / CM4-CM5 (#138)? …

## 1. Platform values #144 must parameterize

| Value | Observed |
|-------|----------|
| UDC name (`ls /sys/class/udc`) | |
| `config.txt` deltas required (`dtoverlay=dwc2` ? `dr_mode=` ?) | |
| `cmdline.txt` / `modules-load=dwc2` required, or `modprobe libcomposite` at runtime enough? | |
| usb0 interface naming (usb0 only? usb0 + usb1 with both RNDIS+ECM?) | |
| RNDIS os_desc path (`functions/rndis.usb0/os_desc/interface.rndis/` present?) | |

## 2. Per-host enumeration (driver-free?)

| Host OS | Net iface appeared? | Driver prompt? | Which function bound (RNDIS/ECM)? | Mass storage mounted? | Serial device? | Notes |
|---------|--------------------|----------------|-----------------------------------|-----------------------|----------------|-------|
| macOS   | | | | | | |
| Windows 10/11 | | | | | | |
| Linux   | | | | | | |

## 3. Function coexistence

- [ ] Do RNDIS + ECM coexist cleanly, or does one break the other on some host? …
- [ ] Does mass storage + serial + net all enumerate together on the single UDC? …
- [ ] `/dev/ttyGS0` present on the Pi and matching serial device on the host? …

## 4. IAD / descriptor quirks

- [ ] bDeviceClass 0xEF/0x02/0x01 accepted by all three hosts? …
- [ ] MS OS descriptors actually suppress the Windows driver prompt? …
- [ ] Any host that rejected the composite descriptor outright? …

## 5. Managed Windows / AV / EDR

- [ ] Any endpoint-protection flag on a corporate-managed laptop (BadUSB-style
      heuristic on RNDIS + mass storage + serial)? …

## 6. DHCP / reachability

- [ ] Did the host get an address on the 10.55.0.0/24 link (static? needs a
      DHCP server on usb0 — #144 scope)? …
- [ ] Could the host reach `http://10.55.0.1/` (camera daemon's provisioning
      page, with `GHOSTCAM_PROVISION_HTTP_ADDR=10.55.0.1:80` set)? …

## 7. Recommendation to #144

_Short verdict: proceed on Zero 2 W / target Pi 4-5 / defer to CM4-CM5, and the
minimal function set that actually enumerated driver-free everywhere._
