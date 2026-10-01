#!/usr/bin/env python3
"""service.py [DIR] [--base URL] [--json OUT] — run every codec-corpus file
through the real CuTePi service end to end and report, per file:

  1. import: POST /upload (as the web UI does) — accepted, or the refusal;
  2. play:   POST /api/play/<file>;
  3. output, checked from outside the process after 1.5 s:
       video/image — a display plane on the HDMI output with alpha > 0
                     (KMS wall; read through libdrm, the service stays master);
       audio       — an HDMI PCM stream in state RUNNING (/proc/asound);
  4. clean up: POST /api/stop, DELETE /api/media/<file>.

Run on the test server with the service running and nothing playing. Every
file it uploads is deleted again; restore the baseline afterwards anyway.
"""
import ctypes as C
import glob
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = sys.argv[1] if len(sys.argv) > 1 and not sys.argv[1].startswith("--") else "/root/cutepi-testmedia"
BASE = sys.argv[sys.argv.index("--base") + 1] if "--base" in sys.argv else "http://127.0.0.1"
JSON_OUT = sys.argv[sys.argv.index("--json") + 1] if "--json" in sys.argv else None

drm = C.CDLL("libdrm.so.2")


class OProps(C.Structure):
    _fields_ = [("count_props", C.c_uint32), ("props", C.POINTER(C.c_uint32)), ("prop_values", C.POINTER(C.c_uint64))]


class Prop(C.Structure):
    _fields_ = [("prop_id", C.c_uint32), ("flags", C.c_uint32), ("name", C.c_char * 32)]


class PRes(C.Structure):
    _fields_ = [("count_planes", C.c_uint32), ("planes", C.POINTER(C.c_uint32))]


drm.drmModeObjectGetProperties.restype = C.POINTER(OProps)
drm.drmModeGetProperty.restype = C.POINTER(Prop)
drm.drmModeGetPlaneResources.restype = C.POINTER(PRes)
_names = {}


def _card():
    for p in sorted(glob.glob("/dev/dri/card*")):
        try:
            fd = os.open(p, os.O_RDWR)
        except OSError:
            continue
        drm.drmSetClientCap(fd, 2, 1)  # universal planes
        drm.drmSetClientCap(fd, 3, 1)  # atomic (exposes alpha/zpos)
        res = drm.drmModeGetPlaneResources(fd)
        if res and res.contents.count_planes > 2:
            return fd, [res.contents.planes[i] for i in range(res.contents.count_planes)]
        os.close(fd)
    return None, []


FD, PLANES = _card()


def _props(pid):
    op = drm.drmModeObjectGetProperties(FD, pid, 0xEEEEEEEE).contents
    out = {}
    for i in range(op.count_props):
        k = op.props[i]
        if k not in _names:
            _names[k] = drm.drmModeGetProperty(FD, k).contents.name.decode()
        out[_names[k]] = op.prop_values[i]
    return out


def visible_planes():
    """Overlay planes on screen with alpha > 0, ignoring the parked panic image (zpos 17)."""
    vis = []
    for pid in PLANES:
        p = _props(pid)
        if p.get("type") == 0 and p.get("CRTC_ID") and p.get("alpha", 0) > 0 and p.get("zpos") != 17:
            vis.append("%dx%d" % (p["CRTC_W"], p["CRTC_H"]))
    return vis


def audio_running():
    for f in glob.glob("/proc/asound/card*/pcm*p/sub*/status"):
        try:
            if "RUNNING" in open(f).read():
                return True
        except OSError:
            pass
    return False


def call(method, path, data=None, headers=None, timeout=120):
    req = urllib.request.Request(BASE + path, method=method, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def upload(path):
    """multipart/form-data upload, field "media", onConflict=rename."""
    boundary = "cutepi-corpus-boundary"
    name = os.path.basename(path)
    with open(path, "rb") as f:
        body = f.read()
    parts = [
        b"--" + boundary.encode(), b'Content-Disposition: form-data; name="onConflict"', b"", b"rename",
        b"--" + boundary.encode(),
        ('Content-Disposition: form-data; name="media"; filename="%s"' % name).encode(),
        b"Content-Type: application/octet-stream", b"", body,
        b"--" + boundary.encode() + b"--", b"",
    ]
    data = b"\r\n".join(parts)
    return call("POST", "/upload", data, {"Content-Type": "multipart/form-data; boundary=" + boundary,
                                           "HX-Request": "true"})


def main():
    if FD is None:
        sys.exit("no DRM device with planes found")
    rows = []
    call("POST", "/api/stop")
    for path in sorted(os.path.join(dp, f) for dp, _, fs in os.walk(ROOT) for f in fs):
        name = os.path.basename(path)
        kind = os.path.basename(os.path.dirname(path))
        row = {"file": name, "kind": kind, "import": "", "play": "", "output": "", "ok": False}
        st, body = upload(path)
        if st != 200:
            row["import"] = "refused %d: %s" % (st, body.strip().splitlines()[-1][:120] if body.strip() else "")
        else:
            row["import"] = "ok"
            st, body = call("POST", "/api/play/" + urllib.parse.quote(name))
            row["play"] = "ok" if st == 200 else "error %d: %s" % (st, body.strip()[:120])
            time.sleep(1.5)
            if kind == "audio":
                on = audio_running()
                row["output"] = "HDMI audio running" if on else "no audio stream"
            else:
                vis = visible_planes()
                on = bool(vis)
                row["output"] = ("on screen " + ", ".join(vis)) if on else "nothing on screen"
            row["ok"] = st == 200 and on
            call("POST", "/api/stop")
            time.sleep(0.4)
            call("DELETE", "/api/media/" + urllib.parse.quote(name))
        rows.append(row)
        print("%s: import %s | play %s | %s" % (name, row["import"], row["play"] or "-", row["output"] or "-"),
              file=sys.stderr, flush=True)
    print("| File | Import | Play | Output | Result |")
    print("|---|---|---|---|---|")
    for r in rows:
        print("| %s | %s | %s | %s | %s |" % (r["file"], r["import"].replace("|", "/"), r["play"] or "-",
                                             r["output"] or "-", "ok" if r["ok"] else "**fails**"))
    print("\n%d of %d files import and play." % (sum(r["ok"] for r in rows), len(rows)))
    if JSON_OUT:
        with open(JSON_OUT, "w") as f:
            json.dump(rows, f, indent=1)


if __name__ == "__main__":
    main()
