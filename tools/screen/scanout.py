#!/usr/bin/env python3
"""scanout.py PLANE OUT.png: what a display plane shows, read back from its framebuffer.

Root only, read-only: GETFB2 on the plane's FB_ID, MAP_DUMB on the handle, mmap. It does not
take DRM master (the service keeps it) and must not run before the service has started (AGENTS.md).
Prints how many of the frame's 4 KiB samples are non-black. The GPU wall is plane 127 on the
test Pi; `grep -A3 "crtc=pixelvalve" /sys/kernel/debug/dri/1/state` lists the planes in use.
Written after the GPU wall showed black for weeks while its frame counters looked healthy."""
import ctypes, fcntl, mmap, os, struct, subprocess, sys
plane, out = int(sys.argv[1]), sys.argv[2]
d = ctypes.CDLL("libdrm.so.2")
class Props(ctypes.Structure):
    _fields_ = [("count_props", ctypes.c_uint32), ("props", ctypes.POINTER(ctypes.c_uint32)), ("prop_values", ctypes.POINTER(ctypes.c_uint64))]
class Prop(ctypes.Structure):
    _fields_ = [("prop_id", ctypes.c_uint32), ("flags", ctypes.c_uint32), ("name", ctypes.c_char * 32)]
class FB2(ctypes.Structure):
    _fields_ = [("fb_id", ctypes.c_uint32), ("width", ctypes.c_uint32), ("height", ctypes.c_uint32), ("pixel_format", ctypes.c_uint32),
                ("modifier", ctypes.c_uint64), ("flags", ctypes.c_uint32), ("handles", ctypes.c_uint32 * 4), ("pitches", ctypes.c_uint32 * 4), ("offsets", ctypes.c_uint32 * 4)]
d.drmModeObjectGetProperties.restype = ctypes.POINTER(Props)
d.drmModeGetProperty.restype = ctypes.POINTER(Prop)
d.drmModeGetFB2.restype = ctypes.POINTER(FB2)
d.drmModeGetFB2.argtypes = [ctypes.c_int, ctypes.c_uint32]
d.drmModeObjectGetProperties.argtypes = [ctypes.c_int, ctypes.c_uint32, ctypes.c_uint32]
fd = os.open("/dev/dri/card1", os.O_RDWR)
d.drmSetClientCap(fd, 2, 1)
d.drmSetClientCap(fd, 3, 1)
p = d.drmModeObjectGetProperties(fd, plane, 0xeeeeeeee)
fb = None
for i in range(p.contents.count_props):
    if d.drmModeGetProperty(fd, p.contents.props[i]).contents.name == b"FB_ID":
        fb = p.contents.prop_values[i]
f = d.drmModeGetFB2(fd, fb).contents
def mapped(handle, size):
    """The buffer's bytes: a dumb buffer through MAP_DUMB, anything else (a decoder's or importer's buffer)
    through a PRIME export of the handle and an mmap of the dma-buf."""
    try:
        res = fcntl.ioctl(fd, 0xC01064B3, struct.pack("<IIQ", handle, 0, 0))  # DRM_IOCTL_MODE_MAP_DUMB
        m = mmap.mmap(fd, size, mmap.MAP_SHARED, mmap.PROT_READ, offset=struct.unpack("<IIQ", res)[2])
    except OSError:
        pfd = ctypes.c_int(-1)
        if d.drmPrimeHandleToFD(fd, handle, os.O_CLOEXEC, ctypes.byref(pfd)):
            raise
        m = mmap.mmap(pfd.value, size, mmap.MAP_SHARED, mmap.PROT_READ)
    raw = bytes(m[:size]); m.close()
    return raw

fmt = struct.pack("<I", f.pixel_format).decode()
if f.modifier not in (0, 0xffffffffffffff):
    sys.exit("plane %d fb %d %dx%d %s: modifier 0x%x (tiled, e.g. HEVC SAND) not read; see tools/codec-corpus/sandcheck.py" % (plane, fb, w := f.width, f.height, fmt, f.modifier))
w, h = f.width, f.height
if fmt in ("YU12", "NV12"):
    # Planes may share one buffer (offsets) or not: read each plane's rows and pack them tightly.
    rows = [(0, h, w)] + ([(1, h // 2, w // 2), (2, h // 2, w // 2)] if fmt == "YU12" else [(1, h // 2, w)])
    cache, raw = {}, b""
    for p_, n, rowb in rows:
        hd = f.handles[p_]
        if hd not in cache:
            cache[hd] = mapped(hd, f.offsets[p_] + f.pitches[p_] * n if hd != f.handles[0] else
                               max(f.offsets[k] + f.pitches[k] * (h if k == 0 else h // 2) for k in range(len(rows)) if f.handles[k] == hd))
        b, o, pitch = cache[hd], f.offsets[p_], f.pitches[p_]
        raw += b"".join(b[o + y * pitch:o + y * pitch + rowb] for y in range(n))
    pix, sw = ("yuv420p" if fmt == "YU12" else "nv12"), w
    luma = raw[:w * h]
    nz = sum(1 for i in range(0, len(luma), 1024) if luma[i] > 20)
    total = len(luma) // 1024
else:
    pitch = f.pitches[0]
    raw = mapped(f.handles[0], pitch * h)
    pix = {"XB24": "rgb0", "AB24": "rgba", "XR24": "bgr0", "AR24": "bgra", "RG16": "rgb565le"}[fmt]
    sw = pitch // (2 if fmt == "RG16" else 4)
    nz = sum(1 for i in range(0, len(raw), 4096) if raw[i:i+3] != b"\0\0\0")
    total = len(raw) // 4096
subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", pix, "-s", "%dx%d" % (sw, h),
                "-i", "-", "-vf", "crop=%d:%d:0:0,scale=960:-2" % (w, h), out], input=raw, check=True)
print("plane %d fb %d %dx%d %s: %d of %d samples non-black" % (plane, fb, w, h, fmt, nz, total))
