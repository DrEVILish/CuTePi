#!/usr/bin/env python3
"""sandcheck.py FILE [FPS]: is the plane wall's HEVC picture bit-exact (8-bit NV12 or 10-bit P030)?

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


def unpack10(raw, n):
    """P030: three 10-bit samples in each little-endian 32-bit word (bits 0-9, 10-19, 20-29)."""
    out = []
    for i in range(0, len(raw) - 3, 4):
        w = raw[i] | raw[i + 1] << 8 | raw[i + 2] << 16 | raw[i + 3] << 24
        out += (w & 1023, w >> 10 & 1023, w >> 20 & 1023)
    return out[:n]


def psnr(a, b):
    se = sum((x - y) ** 2 for x, y in zip(a, b))
    return 99.0 if se == 0 else 10 * math.log10(255 * 255 * len(a) / se)


def main():
    path = sys.argv[1]
    fps = float(sys.argv[2]) if len(sys.argv) > 2 else 60.0
    meta = dict(zip(*[iter(open("/dev/shm/pw_meta.txt").read().split())] * 2))
    w, h, ah = int(meta["w"]), int(meta["h"]), int(meta["ah"])
    p030 = meta.get("fourcc") == "P030"
    pts = int(meta["pts_ns"]) / 1e9
    n = round(pts * fps)
    print("dumped frame %s (%s), pts %.3f s -> about clip frame %d" % (meta["frame"], meta.get("fourcc", "NV12"), pts, n))
    a, b = max(0, n - 30), n + 5
    if p030:
        # Rows of 10-bit samples, packed three to a word: untile by bytes, then unpack each row.
        wb = (w + 2) // 3 * 4
        yb = untile(open("/dev/shm/pw_Y.raw", "rb").read(), wb, h, ah)
        ub = untile(open("/dev/shm/pw_UV.raw", "rb").read(), wb, h // 2, ah)
        y = [v for r in range(h) for v in unpack10(yb[r * wb:(r + 1) * wb], w)]
        uv = [v for r in range(h // 2) for v in unpack10(ub[r * wb:(r + 1) * wb], w)]
        raw = subprocess.run(["ffmpeg", "-v", "error", "-c:v", "hevc", "-i", path, "-vf",
                              "select=between(n\\,%d\\,%d)" % (a, b), "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "p010le", "-"],
                             capture_output=True, check=True).stdout
        fs = w * h * 3
        frames = []
        for i in range(len(raw) // fs):
            fr = raw[i * fs:(i + 1) * fs]
            # Convert a frame in full only when its first rows match (pure Python is slow).
            head = [(fr[k] | fr[k + 1] << 8) >> 6 for k in range(0, 2 * 4 * w, 2)]
            if head != y[:4 * w]:
                frames.append(([None], [None]))
                continue
            vals = [(fr[k] | fr[k + 1] << 8) >> 6 for k in range(0, fs, 2)]
            frames.append((vals[:w * h], vals[w * h:]))
    else:
        y = untile(open("/dev/shm/pw_Y.raw", "rb").read(), w, h, ah)
        uv = untile(open("/dev/shm/pw_UV.raw", "rb").read(), w, h // 2, ah)
        raw = subprocess.run(["ffmpeg", "-v", "error", "-c:v", "hevc", "-i", path, "-vf",
                              "select=between(n\\,%d\\,%d)" % (a, b), "-vsync", "0", "-f", "rawvideo", "-pix_fmt", "nv12", "-"],
                             capture_output=True, check=True).stdout
        fs = w * h * 3 // 2
        frames = [(raw[i * fs:i * fs + w * h], raw[i * fs + w * h:(i + 1) * fs]) for i in range(len(raw) // fs)]
    best = None
    for i, (fy, fuv) in enumerate(frames):
        if list(y) == list(fy):
            exact_uv = list(uv) == list(fuv)
            print("  clip frame %d: Y bit-exact, UV %s" % (a + i, "bit-exact" if exact_uv else "differs"))
            if exact_uv:
                best = a + i
    if best is None and not any(list(y) == list(fy) for fy, _ in frames):
        print("  no clip frame in %d..%d has this Y" % (a, b))
    print("RESULT: %s" % ("bit-exact with clip frame %d" % best if best is not None else "NO bit-exact match"))
    return 0 if best is not None else 1


if __name__ == "__main__":
    sys.exit(main())
