#!/bin/sh
# batman-wifi.sh up <mesh-addr/prefix> | down | status
#
# BATMAN-adv mesh over Wi-Fi ad-hoc (IBSS) between linked CuTePi units
# (DESIGN §6.8c). Runtime only: a reboot undoes it. The Pi 4's Wi-Fi
# (brcmfmac) has no 802.11s mesh mode, so batman-adv runs over IBSS; eth0 and
# its address are not touched. Every unit in the mesh needs the same SSID,
# frequency and BSSID. Settings (environment): MESH_IF (wlan0), MESH_SSID
# (cutepi-mesh), MESH_FREQ (2412 = channel 1, the quietest here), MESH_BSSID, MESH_COUNTRY (GB).
set -eu
IF=${MESH_IF:-wlan0}
SSID=${MESH_SSID:-cutepi-mesh}
FREQ=${MESH_FREQ:-2412}
BSSID=${MESH_BSSID:-02:c7:e5:00:00:01}

case "${1:-}" in
up)
	[ -n "${2:-}" ] || { echo "usage: $0 up <addr/prefix>, e.g. 10.77.0.73/24" >&2; exit 2; }
	nmcli dev set "$IF" managed no 2>/dev/null || true # NetworkManager would reconfigure it
	sleep 1 # wpa_supplicant lets go of the interface; switching type before that fails (-95)
	rfkill unblock wifi
	iw reg set "${MESH_COUNTRY:-GB}"
	modprobe batman-adv
	ip link set "$IF" down
	iw dev "$IF" set type ibss
	# batman-adv adds a 32-byte header; if the driver refuses 1532 the mesh
	# still works, bat0 fragments instead.
	ip link set "$IF" mtu 1532 2>/dev/null || true
	ip link set "$IF" up
	# brcmfmac refuses HT20 here and picks its own BSSID; units with the same
	# SSID and frequency still merge into one cell.
	iw dev "$IF" ibss join "$SSID" "$FREQ" fixed-freq "$BSSID"
	# Wi-Fi power save holds frames for the next beacon: 23-93 ms pings
	# between two Pis instead of a few ms.
	iw dev "$IF" set power_save off
	# A fresh bat0 on every `up`: one kept from before kept its translation
	# and ARP (DAT) tables, and with a changed address peers were still told
	# the old one (100% loss until those entries went).
	ip link del bat0 2>/dev/null || true
	ip link add bat0 type batadv
	# A fixed bat0 address (the radio's, locally administered): a new random
	# one on every `up` left peers' ARP pointing at the old one for ~40 s.
	mac=$(cat "/sys/class/net/$IF/address")
	ip link set bat0 address "02:${mac#*:}"
	ip link set "$IF" master bat0
	ip link set bat0 up
	ip addr replace "$2" dev bat0
	;;
down)
	ip link del bat0 2>/dev/null || true
	ip link set "$IF" down 2>/dev/null || true
	iw dev "$IF" set type managed 2>/dev/null || true
	nmcli dev set "$IF" managed yes 2>/dev/null || true
	;;
status)
	iw dev "$IF" info
	ip -br addr show dev bat0
	cat "/sys/class/net/$IF/batman_adv/iface_status" 2>/dev/null || true
	;;
*)
	echo "usage: $0 up <addr/prefix> | down | status" >&2
	exit 2
	;;
esac
