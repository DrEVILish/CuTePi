#!/bin/sh
# Adds CuTePi's firmware settings to the Raspberry Pi 4's config.txt (DESIGN §12.16):
#   install-firmware-config.sh              codec clocks held at stock maximum (deploy/firmware/codec-clocks.txt)
#   install-firmware-config.sh --overclock  the above plus the test Pi's overclock (deploy/firmware/overclock.txt)
#   install-firmware-config.sh --remove     takes CuTePi's block out again
# The block goes at the end of config.txt between markers (so a run replaces it), under [all]; the old file is kept
# as config.txt.cutepi-bak. Takes effect at the next boot: this script never reboots.
# Pi 4 only: the Pi 5 has no H.264 decoder block and other clock names.
set -e
dir="$(cd "$(dirname "$0")" && pwd)"
cfg="${CUTEPI_FIRMWARE_CONFIG:-/boot/firmware/config.txt}"
begin="# >>> CuTePi firmware settings (deploy/install-firmware-config.sh) >>>"
end="# <<< CuTePi firmware settings <<<"
mode=clocks
case "$1" in
  "") ;;
  --overclock) mode=overclock ;;
  --remove) mode=remove ;;
  *) echo "usage: $0 [--overclock | --remove]" >&2; exit 2 ;;
esac
if [ -z "$CUTEPI_FIRMWARE_CONFIG" ]; then
  model="$(tr -d '\0' 2>/dev/null < /proc/device-tree/model || true)"
  case "$model" in
    "Raspberry Pi 4"*) ;;
    *) echo "not a Raspberry Pi 4 (${model:-unknown}): nothing changed" >&2; exit 1 ;;
  esac
fi
[ -f "$cfg" ] || { echo "$cfg not found" >&2; exit 1; }
cp -p "$cfg" "$cfg.cutepi-bak"
tmp="$(mktemp)"
# Everything but an earlier CuTePi block.
awk -v b="$begin" -v e="$end" '$0 == b { skip = 1; next } $0 == e { skip = 0; next } !skip' "$cfg" > "$tmp"
if [ "$mode" != remove ]; then
  {
    echo "$begin"
    echo "[all]"
    cat "$dir/firmware/codec-clocks.txt"
    [ "$mode" = overclock ] && cat "$dir/firmware/overclock.txt"
    echo "$end"
  } >> "$tmp"
fi
cat "$tmp" > "$cfg"
rm -f "$tmp"
case "$mode" in
  remove) echo "CuTePi's firmware settings removed from $cfg (backup: $cfg.cutepi-bak). Reboot to apply." ;;
  *) echo "CuTePi's firmware settings ($mode) written to $cfg (backup: $cfg.cutepi-bak). Reboot to apply." ;;
esac
