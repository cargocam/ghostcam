#!/bin/sh
# Spike (#139): USB composite gadget bring-up on the Pi Zero 2 W via
# libcomposite/configfs. Composite = RNDIS + CDC-ECM + CDC-ACM + mass storage
# on the single UDC, aiming to enumerate driver-free on macOS/Windows/Linux.
#
# UNVALIDATED — this has not run on hardware. Comments mark the paths and
# values most likely to need per-kernel adjustment; verify them on the real Pi
# and record the truth in FINDINGS.md. This script is a spike artefact, NOT the
# production unit (that's #144, which will adopt the values this spike records).
#
# Usage:  sudo ./ghostcam-usb-gadget.sh up | down
#
# Prereqs on the Pi:
#   * /boot/firmware/config.txt: dtoverlay=dwc2,dr_mode=otg  (then reboot)
#   * mass-storage image built:  sudo ./make-onboarding-image.sh
set -eu

GADGET=/sys/kernel/config/usb_gadget/ghostcam
IMG=/opt/ghostcam/onboarding.img         # built by make-onboarding-image.sh

# usb0 address for the point-to-point link. The camera daemon serves the
# provisioning page here (GHOSTCAM_PROVISION_HTTP_ADDR=10.55.0.1:80).
USB0_ADDR=10.55.0.1/24

# Locally-administered, unicast MAC addresses (first octet: bit1 set = local,
# bit0 clear = unicast → 0x02). Host/dev must differ. Fixed here for the spike;
# #144 should derive them from the device serial so multiple Pis on one bench
# don't collide.
HOST_MAC=02:1a:11:00:00:01   # what the host end of the link presents
DEV_MAC=02:1a:11:00:00:02    # what the Pi end presents

log() { echo "[usb-gadget] $*"; }

gadget_down() {
	[ -d "$GADGET" ] || { log "no gadget present"; return 0; }

	# Unbind from the UDC first — configfs won't let you remove a bound gadget.
	if [ -s "$GADGET/UDC" ]; then
		echo "" > "$GADGET/UDC" || true
	fi

	# configfs teardown is strictly rmdir/unlink (no rm -rf). Order: unlink
	# functions from configs, remove config strings, rmdir configs, rmdir
	# functions, rmdir strings, rmdir gadget.
	for l in "$GADGET"/configs/c.1/*; do
		[ -L "$l" ] && rm -f "$l"
	done
	# os_desc → config link
	[ -L "$GADGET/os_desc/c.1" ] && rm -f "$GADGET/os_desc/c.1"

	[ -d "$GADGET/configs/c.1/strings/0x409" ] && rmdir "$GADGET/configs/c.1/strings/0x409" || true
	[ -d "$GADGET/configs/c.1" ] && rmdir "$GADGET/configs/c.1" || true

	for f in "$GADGET"/functions/*; do
		[ -d "$f" ] && rmdir "$f" || true
	done
	[ -d "$GADGET/strings/0x409" ] && rmdir "$GADGET/strings/0x409" || true
	rmdir "$GADGET" || true
	log "gadget torn down"
}

gadget_up() {
	modprobe libcomposite

	# configfs is normally auto-mounted; mount it if a bare image didn't.
	if [ ! -d /sys/kernel/config/usb_gadget ]; then
		mount -t configfs none /sys/kernel/config 2>/dev/null || true
	fi

	# Idempotent: clear any prior instance before rebuilding.
	gadget_down

	[ -f "$IMG" ] || { log "ERROR: mass-storage image $IMG missing — run make-onboarding-image.sh"; exit 1; }

	mkdir -p "$GADGET"
	cd "$GADGET"

	echo 0x1d6b > idVendor        # Linux Foundation
	echo 0x0104 > idProduct       # Multifunction Composite Gadget
	echo 0x0100 > bcdDevice
	echo 0x0200 > bcdUSB

	# Composite/IAD device class so Windows treats the whole thing as one
	# multifunction device (0xEF Misc / 0x02 / 0x01).
	echo 0xEF > bDeviceClass
	echo 0x02 > bDeviceSubClass
	echo 0x01 > bDeviceProtocol

	mkdir -p strings/0x409
	echo "$(cat /sys/firmware/devicetree/base/serial-number 2>/dev/null | tr -d '\0' || echo ghostcam)" > strings/0x409/serialnumber
	echo "Ghostcam" > strings/0x409/manufacturer
	echo "Ghostcam Onboarding Gadget" > strings/0x409/product

	# --- Microsoft OS descriptors: make Windows auto-bind RNDIS, no prompt ---
	echo 1 > os_desc/use
	echo 0xcd > os_desc/b_vendor_code
	echo MSFT100 > os_desc/qw_sign

	# --- functions ---

	# RNDIS (Windows). Linked into the config FIRST (Windows binds interface 0).
	mkdir -p functions/rndis.usb0
	echo "$HOST_MAC" > functions/rndis.usb0/host_addr
	echo "$DEV_MAC"  > functions/rndis.usb0/dev_addr
	# Tie RNDIS to the MS OS descriptor. VERIFY these paths on the target
	# kernel — the interface subdir name (interface.rndis) is version-sensitive.
	if [ -d functions/rndis.usb0/os_desc/interface.rndis ]; then
		echo RNDIS   > functions/rndis.usb0/os_desc/interface.rndis/compatible_id
		echo 5162001 > functions/rndis.usb0/os_desc/interface.rndis/sub_compatible_id
	else
		log "WARN: functions/rndis.usb0/os_desc/interface.rndis missing — Windows may prompt for a driver; record in FINDINGS.md"
	fi

	# CDC-ECM (macOS / Linux).
	mkdir -p functions/ecm.usb0
	echo "$HOST_MAC" > functions/ecm.usb0/host_addr
	echo "$DEV_MAC"  > functions/ecm.usb0/dev_addr

	# CDC-ACM serial → /dev/ttyGS0 on the Pi.
	mkdir -p functions/acm.gs0

	# Mass storage: read-only, non-removable FAT volume.
	mkdir -p functions/mass_storage.0
	echo 1     > functions/mass_storage.0/lun.0/ro
	echo 0     > functions/mass_storage.0/lun.0/removable
	echo "$IMG" > functions/mass_storage.0/lun.0/file

	# --- configuration ---
	mkdir -p configs/c.1/strings/0x409
	echo "Ghostcam composite (RNDIS+ECM+ACM+MSD)" > configs/c.1/strings/0x409/configuration
	echo 250 > configs/c.1/MaxPower

	# Point the MS OS descriptor at this config.
	ln -s configs/c.1 os_desc/c.1

	# Link functions into the config. ORDER MATTERS: RNDIS first for Windows.
	ln -s functions/rndis.usb0    configs/c.1/
	ln -s functions/ecm.usb0      configs/c.1/
	ln -s functions/acm.gs0       configs/c.1/
	ln -s functions/mass_storage.0 configs/c.1/

	# Bind to the (only) UDC. Record its name in FINDINGS.md — #144's
	# abstraction seam parameterizes it (e.g. 3f980000.usb / 20980000.usb).
	udc="$(ls /sys/class/udc | head -1)"
	[ -n "$udc" ] || { log "ERROR: no UDC in /sys/class/udc — is dwc2 loaded in peripheral/otg mode?"; exit 1; }
	log "binding to UDC: $udc"
	echo "$udc" > UDC

	# Bring up the point-to-point link. Both rndis.usb0 and ecm.usb0 create
	# host-facing interfaces; on the Pi they present as usb0 (and possibly
	# usb1). Assign the address to usb0. Record actual interface naming.
	sleep 1
	if ip link show usb0 >/dev/null 2>&1; then
		ip addr add "$USB0_ADDR" dev usb0 2>/dev/null || true
		ip link set usb0 up
		log "usb0 up at $USB0_ADDR"
	else
		log "WARN: usb0 did not appear — check FINDINGS.md interface-naming note"
	fi

	log "gadget up. Plug the USB data port into a host and record enumeration in FINDINGS.md."
}

case "${1:-}" in
	up)   gadget_up ;;
	down) gadget_down ;;
	*)    echo "usage: $0 up|down" >&2; exit 2 ;;
esac
