#!/usr/bin/env python3
"""sandcheck.py FILE [FPS]: is the plane wall's HEVC picture bit-exact?

Compares the frame the plane wall presenter dumped (CUTEPI_PLANEWALL_DUMP=N in
the service's environment: /dev/shm/pw_Y.raw from the HEVC decoder's own
buffer, /dev/shm/pw_UV.raw from the GPU-gathered UV buffer, /dev/shm/pw_meta.txt)
with FFmpeg's software decode of FILE around the dumped frame's timestamp.

Both dumps are SAND128: 128-byte-wide columns, each ALIGN(h,16) lines tall
(the UV buffer uses the first half of each column). They are untiled to linear
NV12 and compared with the reference frames around n = round(pts * fps) (the
clip's timestamps can be a few frames off): the frame with the same Y must
have the same UV too."""
import math
import subprocess
import sys


def untile(raw, w, rows, colh):
    """SAND128 (columns of colh lines) to linear rows of w bytes."""
    out = bytearray(w * rows)
    for c in range((w + 127) // 128):
        base = c * 128 * colh
        cw = min(128, w - c * 128)
        for r in range(rows):
            s = base + r * 128
            out[r * w + c * 128:r * w + c * 128 + cw] = raw[s:s + cw]
    return bytes(out)


def psnr(a, b):
    se = sum((x - y) ** 2 for x, y in zip(a, b))
    return 99.0 if se == 0 else 10 * math.log10(255 * 255 * len(a) / se)


def main():
    path = sys.argv[1]
    fps = float(sys.argv[2]) if len(sys.argv) > 2 else 60.0
    meta = dict(zip(*[iter(open("/dev/shm/pw_meta.txt").read().split())] * 2))
    w, h, ah = int(meta["w"]), int(meta["h"]), int(meta["ah"])
    pts = int(meta["pts_ns"]) / 1e9
    y = untile(open("/dev/shm/pw_Y.raw", "rb").read(), w, h, ah)
    uv = untile(open("/dev/shm/pw_UV.raw", "rb").read(), w, h // 2, ah)
    n = round(pts * fps)
    print("dumped frame %s, pts %.3f s -> about clip frame %d" % (meta["frame"], pts, n))
    # Timestamps can be offset by a few frames (edit lists, B-frame delay):
    # decode a window around n once and find the frame whose Y matches.
    a, b = max(0, n - 30), n + 5
    raw = subprocess.run(["ffmpeg", "-v", "error", "-c:v", "hevc", "-i", path, "-vf",
                          "select=between(n\\,%d\\,%d)" % (a, b), "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "nv12", "-"],
                         capture_output=True, check=True).stdout
    fs = w * h * 3 // 2
    best = None
    for i in range(len(raw) // fs):
        fr = raw[i * fs:(i + 1) * fs]
        if y == fr[:w * h]:
            exact_uv = uv == fr[w * h:fs]
            print("  clip frame %d: Y bit-exact, UV %s" % (a + i, "bit-exact" if exact_uv else
                                                          "differs (PSNR %.2f dB)" % psnr(uv, fr[w * h:fs])))
            if exact_uv:
                best = a + i
    if best is None and not any(y == raw[i * fs:i * fs + w * h] for i in range(len(raw) // fs)):
        print("  no clip frame in %d..%d has this Y" % (a, b))
    print("RESULT: %s" % ("bit-exact with clip frame %d" % best if best is not None else "NO bit-exact match"))
    return 0 if best is not None else 1


if __name__ == "__main__":
    sys.exit(main())
