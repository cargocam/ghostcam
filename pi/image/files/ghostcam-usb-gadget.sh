#!/bin/sh
# ghostcam-usb-gadget.sh — bring up the USB composite gadget and pin the
# gadget network link, so a laptop/phone plugged into the Pi's OTG port
# lands on the local provisioning page with no drivers and no server in
# the loop (Local-First Onboarding milestone, #139 spike + #144).
#
# Composite functions on one UDC (driver-free enumeration is the whole
# point — validate per host OS, see pi/GADGET-SPIKE.md):
#   - RNDIS + CDC-ECM  usb0 network (RNDIS for Windows, ECM for mac/Linux)
#   - Mass storage     read-only FAT volume holding the redirect .htm (#145)
#   - CDC-ACM          /dev/ttyGS0 serial rescue console (#149)
#
# Runs from ghostcam-usb-gadget.service, ordered Before=network-online
# so usb0 (10.55.0.1) exists before the camera daemon starts and binds
# its provisioning server there.
set -eu

GADGET=/sys/kernel/config/usb_gadget/ghostcam
USB_IP=10.55.0.1
USB_PREFIX=24
MSD_IMG=/var/ghostcam/usb-msd.img
MSD_SRC=/usr/local/share/ghostcam/usb-msd

# --- Platform seam (survives the CM4/CM5 move, #138) -------------------
# The UDC name and the dwc2 overlay are the board-specific bits. Auto-
# detect the single peripheral controller on the Zero 2 W; let the image
# env override it (GHOSTCAM_UDC) for boards where detection is wrong.
UDC="${GHOSTCAM_UDC:-}"
if [ -z "$UDC" ]; then
	UDC="$(ls /sys/class/udc 2>/dev/null | head -1 || true)"
fi

log() { echo "ghostcam-usb-gadget: $*"; }

build_msd_image() {
	# One tiny read-only FAT volume with a single redirect .htm the user
	# can double-click when mDNS (#142) is flaky. Rebuilt only if missing.
	[ -f "$MSD_IMG" ] && return 0
	command -v mkfs.vfat >/dev/null 2>&1 || { log "mkfs.vfat missing, skipping mass-storage"; return 1; }
	log "creating mass-storage volume $MSD_IMG"
	dd if=/dev/zero of="$MSD_IMG" bs=1M count=2 status=none
	mkfs.vfat -n GHOSTCAM "$MSD_IMG" >/dev/null
	mnt="$(mktemp -d)"
	if mount -o loop "$MSD_IMG" "$mnt" 2>/dev/null; then
		[ -f "$MSD_SRC/SETUP.HTM" ] && cp "$MSD_SRC/SETUP.HTM" "$mnt/SETUP.HTM"
		umount "$mnt"
	fi
	rmdir "$mnt"
}

up() {
	modprobe libcomposite 2>/dev/null || true

	if [ -z "$UDC" ]; then
		log "no UDC found (dwc2 loaded? OTG port in peripheral mode?) — skipping"
		return 0
	fi
	if [ -e "$GADGET/UDC" ] && [ -s "$GADGET/UDC" ]; then
		log "gadget already bound"
		return 0
	fi

	build_msd_image || true

	mkdir -p "$GADGET"
	cd "$GADGET"

	echo 0x1d6b > idVendor       # Linux Foundation
	echo 0x0104 > idProduct      # Multifunction Composite Gadget
	echo 0x0100 > bcdDevice
	echo 0x0200 > bcdUSB

	# IAD: composite class so Windows binds RNDIS alongside the rest.
	echo 0xEF > bDeviceClass
	echo 0x02 > bDeviceSubClass
	echo 0x01 > bDeviceProtocol

	mkdir -p strings/0x409
	serial="$(cut -c1-16 /var/ghostcam/identity_key.pub 2>/dev/null || true)"
	[ -n "$serial" ] || serial="ghostcam"
	echo "$serial"          > strings/0x409/serialnumber
	echo "Ghostcam"         > strings/0x409/manufacturer
	echo "Ghostcam Camera"  > strings/0x409/product

	mkdir -p configs/c.1/strings/0x409
	echo "Ghostcam composite" > configs/c.1/strings/0x409/configuration
	echo 250 > configs/c.1/MaxPower

	# RNDIS is declared first so Windows treats it as the primary NIC.
	mkdir -p functions/rndis.usb0
	mkdir -p functions/ecm.usb0
	mkdir -p functions/acm.GS0
	if [ -f "$MSD_IMG" ]; then
		mkdir -p functions/mass_storage.0
		echo "$MSD_IMG" > functions/mass_storage.0/lun.0/file
		echo 1 > functions/mass_storage.0/lun.0/ro
		echo 0 > functions/mass_storage.0/lun.0/removable
	fi

	# Microsoft OS descriptors so Windows auto-loads the RNDIS driver
	# instead of prompting. (mac/Linux ignore these and bind ECM.)
	echo 1       > os_desc/use
	echo 0xcd    > os_desc/b_vendor_code
	echo MSFT100 > os_desc/qw_sign
	mkdir -p functions/rndis.usb0/os_desc/interface.rndis
	echo RNDIS   > functions/rndis.usb0/os_desc/interface.rndis/compatible_id
	echo 5162001 > functions/rndis.usb0/os_desc/interface.rndis/sub_compatible_id

	ln -sf functions/rndis.usb0        configs/c.1/
	ln -sf functions/ecm.usb0          configs/c.1/
	ln -sf functions/acm.GS0           configs/c.1/
	[ -d functions/mass_storage.0 ] && ln -sf functions/mass_storage.0 configs/c.1/
	ln -sf configs/c.1 os_desc/

	log "binding gadget to UDC $UDC"
	echo "$UDC" > UDC

	# Pin the gadget link. usb0 is NM-unmanaged (see the conf.d drop-in),
	# so configure it directly here.
	sleep 1
	ip addr add "$USB_IP/$USB_PREFIX" dev usb0 2>/dev/null || true
	ip link set usb0 up
	log "usb0 up at $USB_IP/$USB_PREFIX"
}

down() {
	[ -d "$GADGET" ] || return 0
	echo "" > "$GADGET/UDC" 2>/dev/null || true
	ip link set usb0 down 2>/dev/null || true
}

case "${1:-up}" in
	up)   up ;;
	down) down ;;
	*)    echo "usage: $0 {up|down}" >&2; exit 2 ;;
esac
