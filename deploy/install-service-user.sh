#!/bin/sh
# Run CuTePi as the unprivileged "cutepi" system user (DESIGN §7).
#
# Idempotent; run as root from the repo checkout (/opt/cutepi):
#   sudo deploy/install-service-user.sh [old-data-dir]
#
# - creates the cutepi system user (home /var/lib/cutepi; groups video,
#   render, audio)
# - installs the polkit rules (deploy/50-cutepi.rules) and the unit
#   (cutepi.service; the old one is kept as cutepi.service.pre-user)
# - copies the data from old-data-dir (default /root/cutepi: where a root
#   service kept it) to /var/lib/cutepi, unless that already holds a DB.
#   The old copy is left in place: delete it once the move is confirmed.
# - restarts the service.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
old=${1:-/root/cutepi}
new=/var/lib/cutepi

if [ "$(id -u)" -ne 0 ]; then
	echo "run as root" >&2
	exit 1
fi

if ! id cutepi >/dev/null 2>&1; then
	useradd --system --user-group --home-dir "$new" --shell /usr/sbin/nologin \
		--groups video,render,audio cutepi
fi
usermod -a -G video,render,audio cutepi
install -d -m 750 -o cutepi -g cutepi "$new"

install -m 644 "$here/deploy/50-cutepi.rules" /etc/polkit-1/rules.d/50-cutepi.rules

if [ ! -e "$new/config/ctp.db" ] && [ -d "$old" ]; then
	systemctl stop cutepi 2>/dev/null || true
	cp -a "$old/." "$new/"
	echo "copied $old to $new (the original is kept)"
fi
chown -R cutepi:cutepi "$new"

unit=/etc/systemd/system/cutepi.service
if [ -f "$unit" ] && [ ! -f "$unit.pre-user" ]; then
	cp -p "$unit" "$unit.pre-user"
fi
install -m 644 "$here/cutepi.service" "$unit"
systemctl daemon-reload
systemctl enable cutepi >/dev/null 2>&1 || true
systemctl restart cutepi
systemctl --no-pager --lines=0 status cutepi
