#!/bin/bash
# CuTePi install / deploy script.
#
# Builds the binary and installs a systemd service so CuTePi runs (and restarts
# automatically on failure / boot), with all data under WORKING_DIR. Re-running
# this after a code change rebuilds and restarts the service.
#
# Usage:
#   ./install.sh                 # build + install assets + (re)start the service
#   ./install.sh --no-restart    # install only, do not touch the running unit
#
# Overrides (env):
#   CUTEPI_PREFIX   install root (default /opt/cutepi) - binary + templates +
#                   public land here, and the unit's WorkingDirectory points at it
#   CUTEPI_DATA     stdWORKING_DIR / data tree (default ${PREFIX}/data)
#   CUTEPI_PORT     port the service listens on (default 3001)
set -euo pipefail

PREFIX="${CUTEPI_PREFIX:-/opt/cutepi}"
DATA_DIR="${CUTEPI_DATA:-${PREFIX}/data}"
PORT="${CUTEPI_PORT:-3001}"
SERVICE_NAME="${CUTEPI_SERVICE:-cutepi}"
RESTART=1
[ "${1:-}" = "--no-restart" ] && RESTART=0

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Building CuTePi"
( cd "$REPO_DIR" && go build -o "${PREFIX}/.cutepi-build" . )
mkdir -p "$PREFIX" "$DATA_DIR"

echo "==> Installing binary + runtime assets to ${PREFIX}"
mv "${PREFIX}/.cutepi-build" "${PREFIX}/cutepi"
# The app reads ./templates and ./public relative to its cwd, so the unit's
# WorkingDirectory depends on these living alongside the binary.
cp -r "$REPO_DIR/templates" "$PREFIX/"
rm -rf "${PREFIX}/public"
cp -r "$REPO_DIR/public" "$PREFIX/"

echo "==> Installing systemd unit cutepi.service"
cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<UNIT
[Unit]
Description=CuTePi - Cue-based media playback controller
After=network-online.target sound.target
Wants=network-online.target

[Service]
WorkingDirectory=${PREFIX}
Environment=WORKING_DIR=${DATA_DIR}
Environment=PORT=${PORT}
# So the UI's Restart button runs: systemctl restart ${SERVICE_NAME}, whatever the
# unit is named, not just the default cutepi.
Environment=CUTEPI_SERVICE=${SERVICE_NAME}
Type=simple
ExecStart=${PREFIX}/cutepi
Restart=on-failure
RestartSec=3
KillSignal=SIGTERM
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload 2>/dev/null || true

if [ "$RESTART" = "1" ]; then
    echo "==> Enabling and (re)starting ${SERVICE_NAME}"
    systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1 || true
    systemctl restart "${SERVICE_NAME}" || true
else
    echo "==> --no-restart: unit installed, not started (systemctl start ${SERVICE_NAME} to run it)"
fi

echo "==> Done. Control UI: http://<host>:${PORT}/  (data under ${DATA_DIR})"
echo "==> Restart after code changes: ./install.sh   (or: systemctl restart ${SERVICE_NAME})"