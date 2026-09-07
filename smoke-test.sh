#!/usr/bin/env bash
# smoke-test.sh — one-command hardware validation for CuTePi (Q19).
# Generates a short clip with ffmpeg, then exercises the live API:
# upload -> mediapool -> add cue -> play -> trim -> stop -> cleanup.
#
# Run against a running instance from the Pi itself:
#   ./smoke-test.sh [base_url]        # default http://localhost:3000
set -euo pipefail

BASE="${1:-http://localhost:3000}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
VID="$WORK/smoke.mp4"

say() { printf '\n== %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok()  { printf 'ok:  %s\n' "$*"; }

command -v curl  >/dev/null || die "curl not installed"
command -v ffmpeg >/dev/null || die "ffmpeg not installed"

say "preflight"
curl -fsS -o /dev/null "$BASE/" || die "cannot reach $BASE"

say "generate 3s test clip"
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc=size=640x360:rate=30:duration=3" \
  -f lavfi -i "sine=frequency=440:duration=3" \
  -shortest -c:v libx264 -pix_fmt yuv420p -c:a aac "$VID"
ok "smoke.mp4 generated"

# The cue row carries the filenameless title "smoke"; find its data-cue-pos.
find_smoke_pos() {
  curl -fsS "$BASE/api/cuesheet" | awk -v RS='<tr' '
    /class="cue/ && /smoke/ {
      for (i = 1; i <= NF; i++)
        if ($i ~ /^data-cue-pos="[0-9]+"/) { gsub(/[^0-9]/, "", $i); print $i; exit }
    }'
}

say "clear any leftover smoke cue/media from a previous run"
while pos="$(find_smoke_pos)" && [ -n "$pos" ]; do
  curl -fsS -o /dev/null -X DELETE "$BASE/api/cue/$pos" || break
done
curl -s -o /dev/null -X DELETE "$BASE/api/media/smoke.mp4" || true
ok "clean"

say "upload"
if curl -fsS "$BASE/mediapool" | grep -q 'smoke.mp4'; then
  # A tile already exists (e.g. an interrupted previous run); re-uploading
  # would just replace it, so reuse the registered asset.
  ok "already in mediapool (leftover), reusing"
else
  code="$(curl -s -o /dev/null -w '%{http_code}' -F "media=@$VID" "$BASE/upload")"
  [ "$code" = 200 ] || [ "$code" = 303 ] || die "upload returned HTTP $code"
  curl -fsS "$BASE/mediapool" | grep -q 'smoke.mp4' || die "smoke.mp4 missing from mediapool after upload"
  ok "uploaded -> $code"
fi
ok "visible in mediapool"

say "add cue"
curl -fsS -o /dev/null -X POST "$BASE/api/cue/add/smoke.mp4" || die "add cue failed"
POS="$(find_smoke_pos)"
[ -n "$POS" ] || die "could not locate smoke cue in cuesheet"
ok "cue $POS added"

say "play cue $POS"
curl -fsS -o /dev/null -X POST "$BASE/api/cue/$POS/play" || die "cue play failed"
sleep 1
# pipefail + `grep -q` would fail the pipeline when grep exits on match and
# curl gets EPIPE mid-body; capture to a file and grep that instead.
curl -fsS -o "$WORK/nowplaying.html" "$BASE/api/nowplaying" || die "nowplaying fetch failed"
grep -q 'smoke.mp4' "$WORK/nowplaying.html" || die "now playing is not smoke.mp4"
ok "smoke.mp4 now playing"

say "trim to 2s"
curl -fsS -o "$WORK/inspector.html" -X PUT --data "posStart=0" --data "posEnd=2" \
  "$BASE/api/cue/inspector/$POS" || die "trim PUT failed"
grep -q 'data-pos-end="2000"' "$WORK/inspector.html" || die "trim out not persisted"
ok "trim out = 2.000s"

say "stop"
curl -fsS -o /dev/null -X POST "$BASE/api/stop" || die "stop failed"
ok "stopped"

say "cleanup"
curl -fsS -o /dev/null -X DELETE "$BASE/api/cue/$POS" || die "cue delete failed"
curl -fsS -o /dev/null -X DELETE "$BASE/api/media/smoke.mp4" || die "media delete failed"
ok "clean"

printf '\nSMOKE TEST PASSED at %s\n' "$BASE"