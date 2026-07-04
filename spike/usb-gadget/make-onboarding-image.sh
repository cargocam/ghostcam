#!/bin/sh
# Spike (#139): build the small read-only FAT image the mass-storage gadget
# function exposes. When the operator plugs the Pi into a laptop, this volume
# mounts like a USB stick; the redirect page points them at the on-device
# onboarding form (http://10.55.0.1) — the mass-storage-redirect fallback
# idea from #145, for hosts where mDNS/auto-open doesn't fire.
#
# Needs: dosfstools (mkfs.vfat) + mtools (mcopy). UNVALIDATED on hardware.
set -eu

OUT=/opt/ghostcam/onboarding.img
SIZE_KB=1024                     # 1 MiB — plenty for a couple of HTML files
LABEL=GHOSTCAM

mkdir -p "$(dirname "$OUT")"

# FAT12 on a 1 MiB image. Truncate creates a sparse file; mkfs.vfat formats it.
rm -f "$OUT"
dd if=/dev/zero of="$OUT" bs=1024 count="$SIZE_KB" status=none
mkfs.vfat -F 12 -n "$LABEL" "$OUT" >/dev/null

# Landing page: meta-refresh to the on-device form. Kept self-contained (no
# external assets — there's no internet over a USB cable).
tmp="$(mktemp -d)"
cat > "$tmp/index.html" <<'HTML'
<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=http://10.55.0.1/">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Set up your Ghostcam</title>
<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:30rem;margin:3rem auto;padding:0 1rem;line-height:1.5;text-align:center}a{color:#2563eb}</style>
</head><body>
<h1>Set up your Ghostcam</h1>
<p>If this page doesn't redirect automatically, open
<a href="http://10.55.0.1/">http://10.55.0.1/</a> in your browser to finish
onboarding the camera.</p>
</body></html>
HTML

# Some file managers show a README on the volume root; give one for context.
cat > "$tmp/README.txt" <<'TXT'
Ghostcam camera onboarding
==========================
Open index.html, or browse to http://10.55.0.1/ to enter your provision token.
This volume is read-only and served by the camera over the USB cable.
TXT

mcopy -i "$OUT" "$tmp/index.html" ::index.html
mcopy -i "$OUT" "$tmp/README.txt" ::README.txt
rm -rf "$tmp"

echo "[make-image] wrote $OUT ($SIZE_KB KiB, label $LABEL)"
