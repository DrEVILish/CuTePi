#!/usr/bin/env python3
"""scanout.py PLANE [OUT.png]: what a display plane shows, read back from its framebuffer.

Root only, read-only: GETFB2 on the plane's FB_ID, then the buffer's bytes through MAP_DUMB (dumb buffers) or a
PRIME export and mmap (a decoder's or importer's buffer). It does not take DRM master (the service keeps it) and
must not run before the service has started (AGENTS.md). Prints how much of the picture is not black. The GPU wall
is plane 127 on the test Pi; `grep -A3 "crtc=pixelvalve" /sys/kernel/debug/dri/1/state` lists the planes in use.
Written after the GPU wall showed black for weeks while its frame counters looked healthy.

As a module (tools/codec-corpus/support.py): read_plane(plane) -> dict with "fourcc", "modifier", "width",
"height", "nonblack" (fraction of samples with picture in them) and, for formats it can decode, "raw"/"pix"/
"stride_px" for FFmpeg. Tiled buffers (HEVC's SAND) are judged on their luma bytes only (no PNG)."""
import ctypes, fcntl, mmap, os, struct, subprocess, sys

_d = ctypes.CDLL("libdrm.so.2")


class _Props(ctypes.Structure):
    _fields_ = [("count_props", ctypes.c_uint32), ("props", ctypes.POINTER(ctypes.c_uint32)),
                ("prop_values", ctypes.POINTER(ctypes.c_uint64))]


class _Prop(ctypes.Structure):
    _fields_ = [("prop_id", ctypes.c_uint32), ("flags", ctypes.c_uint32), ("name", ctypes.c_char * 32)]


class _FB2(ctypes.Structure):
    _fields_ = [("fb_id", ctypes.c_uint32), ("width", ctypes.c_uint32), ("height", ctypes.c_uint32),
                ("pixel_format", ctypes.c_uint32), ("modifier", ctypes.c_uint64), ("flags", ctypes.c_uint32),
                ("handles", ctypes.c_uint32 * 4), ("pitches", ctypes.c_uint32 * 4), ("offsets", ctypes.c_uint32 * 4)]


_d.drmModeObjectGetProperties.restype = ctypes.POINTER(_Props)
_d.drmModeObjectGetProperties.argtypes = [ctypes.c_int, ctypes.c_uint32, ctypes.c_uint32]
_d.drmModeGetProperty.restype = ctypes.POINTER(_Prop)
_d.drmModeGetFB2.restype = ctypes.POINTER(_FB2)
_d.drmModeGetFB2.argtypes = [ctypes.c_int, ctypes.c_uint32]
RGB = {"XB24": "rgb0", "AB24": "rgba", "XR24": "bgr0", "AR24": "bgra", "RG16": "rgb565le"}


def _mapped(fd, handle, size=None):
    """The buffer's bytes (size None: the whole dma-buf)."""
    try:
        if size is None:
            raise OSError
        res = fcntl.ioctl(fd, 0xC01064B3, struct.pack("<IIQ", handle, 0, 0))  # DRM_IOCTL_MODE_MAP_DUMB
        m = mmap.mmap(fd, size, mmap.MAP_SHARED, mmap.PROT_READ, offset=struct.unpack("<IIQ", res)[2])
    except OSError:
        pfd = ctypes.c_int(-1)
        if _d.drmPrimeHandleToFD(fd, handle, os.O_CLOEXEC, ctypes.byref(pfd)):
            raise OSError("cannot export handle %d" % handle)
        try:
            n = os.lseek(pfd.value, 0, os.SEEK_END)
            size = n if size is None else min(size, n)
            m = mmap.mmap(pfd.value, size, mmap.MAP_SHARED, mmap.PROT_READ)
        finally:
            os.close(pfd.value)
    raw = bytes(m[:size])
    m.close()
    return raw


def _luma_share(b):
    """Share of luma samples (every 16th byte) above black."""
    v = memoryview(b)[::16]
    return sum(1 for x in v if x > 20) / max(1, len(v))


def read_plane(plane):
    fd = os.open("/dev/dri/card1", os.O_RDWR)
    try:
        _d.drmSetClientCap(fd, 2, 1)
        _d.drmSetClientCap(fd, 3, 1)
        p = _d.drmModeObjectGetProperties(fd, plane, 0xeeeeeeee)
        fb = None
        for i in range(p.contents.count_props):
            if _d.drmModeGetProperty(fd, p.contents.props[i]).contents.name == b"FB_ID":
                fb = p.contents.prop_values[i]
        if not fb:
            return {"plane": plane, "fb": 0, "nonblack": 0.0, "fourcc": "-"}
        f = _d.drmModeGetFB2(fd, fb).contents
        w, h = f.width, f.height
        fourcc = struct.pack("<I", f.pixel_format).decode()
        out = {"plane": plane, "fb": fb, "width": w, "height": h, "fourcc": fourcc, "modifier": f.modifier}
        if f.modifier not in (0, 0xffffffffffffff):
            # Tiled (HEVC SAND): the luma bytes, wherever they sit, say whether there is a picture.
            raw = _mapped(fd, f.handles[0])
            end = f.offsets[1] if f.handles[1] == f.handles[0] and f.offsets[1] > f.offsets[0] else len(raw)
            out["nonblack"] = _luma_share(raw[f.offsets[0]:end])
            return out
        if fourcc in ("YU12", "NV12"):
            rows = [(0, h, w)] + ([(1, h // 2, w // 2), (2, h // 2, w // 2)] if fourcc == "YU12" else [(1, h // 2, w)])
            cache, raw = {}, b""
            for k, n, rowb in rows:
                hd = f.handles[k]
                if hd not in cache:
                    cache[hd] = _mapped(fd, hd)
                b, o, pitch = cache[hd], f.offsets[k], f.pitches[k]
                raw += b"".join(b[o + y * pitch:o + y * pitch + rowb] for y in range(n))
            out.update(raw=raw, pix="yuv420p" if fourcc == "YU12" else "nv12", stride_px=w,
                       nonblack=_luma_share(raw[:w * h]))
            return out
        if fourcc in RGB:
            pitch = f.pitches[0]
            raw = _mapped(fd, f.handles[0], pitch * h)
            bpp = 2 if fourcc == "RG16" else 4
            step = 16 * bpp
            nz = sum(1 for i in range(0, len(raw) - 3, step) if raw[i:i + 3] != b"\0\0\0"[:min(3, bpp)])
            out.update(raw=raw, pix=RGB[fourcc], stride_px=pitch // bpp, nonblack=nz / max(1, len(raw) // step))
            return out
        out["nonblack"] = None  # a format this tool does not read
        return out
    finally:
        os.close(fd)


def save_png(info, path):
    subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", info["pix"], "-s",
                    "%dx%d" % (info["stride_px"], info["height"]), "-i", "-", "-vf",
                    "crop=%d:%d:0:0,scale=960:-2" % (info["width"], info["height"]), path], input=info["raw"], check=True)


if __name__ == "__main__":
    info = read_plane(int(sys.argv[1]))
    nb = info.get("nonblack")
    print("plane %d fb %d %sx%s %s mod 0x%x: %s non-black" % (info["plane"], info["fb"], info.get("width"), info.get("height"),
          info["fourcc"], info.get("modifier", 0), "%.1f%%" % (nb * 100) if nb is not None else "not read"))
    if len(sys.argv) > 2 and "raw" in info:
        save_png(info, sys.argv[2])
