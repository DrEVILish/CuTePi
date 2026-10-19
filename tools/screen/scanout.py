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
w, h, pitch, fmt = f.width, f.height, f.pitches[0], struct.pack("<I", f.pixel_format).decode()
arg = struct.pack("<IIQ", f.handles[0], 0, 0)            # drm_mode_map_dumb {handle, pad, offset}
DRM_IOCTL_MODE_MAP_DUMB = 0xC01064B3
res = fcntl.ioctl(fd, DRM_IOCTL_MODE_MAP_DUMB, arg)
off = struct.unpack("<IIQ", res)[2]
m = mmap.mmap(fd, pitch * h, mmap.MAP_SHARED, mmap.PROT_READ, offset=off)
raw = bytes(m[:pitch * h]); m.close()
pix = {"XB24": "rgb0", "AB24": "rgba", "XR24": "bgr0", "AR24": "bgra", "RG16": "rgb565le"}[fmt]
subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", pix, "-s", "%dx%d" % (pitch // (2 if fmt == "RG16" else 4), h),
                "-i", "-", "-vf", "crop=%d:%d:0:0,scale=960:-1" % (w, h), out], input=raw, check=True)
nz = sum(1 for i in range(0, len(raw), 4096) if raw[i:i+3] != b"\0\0\0")
print("plane %d fb %d %dx%d %s: %d of %d samples non-black" % (plane, fb, w, h, fmt, nz, len(raw) // 4096))
