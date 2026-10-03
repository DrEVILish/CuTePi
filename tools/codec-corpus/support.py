#!/usr/bin/env python3
"""support.py [DIR] [--readme README.md] [--json OUT] [--only SUBSTR]

The codec SUPPORT test (README "Codec support"). Every file of the support set
(make-support.sh) is run through the live CuTePi service with a 1 s fade in
and a 1 s fade out, and what the display actually presents is measured in the
kernel, outside the service. A codec+container is listed **Supported** only if:

  video  1080p60 — a new frame on >= 59 of every 60 refreshes during the
         fade in, steady play and the fade out, and the fades change the
         opacity >= 59 times a second (a smooth fade at the display rate);
  image  a 1080p still whose fades change the opacity >= 59 times a second;
         an animated image (GIF, APNG, WebP) also presents every one of its
         frames (all but one per measuring window, as 59 of 60 for video)
         through the fades and steady play, since GIF timing cannot express
         60 fps;
  alpha  (files named *alpha*, video or image) also: the picture reaches the
         display plane in a pixel format with an alpha channel, blended as
         straight (non-premultiplied) alpha ("pixel blend mode" Coverage),
         so the transparent areas show what lies beneath;
  audio  (the 1080p60 part does not apply) the HDMI stream runs without a
         break through steady play and the fade out.

Otherwise it is listed "<Pi model> - unsupported" with the measured figures.

Measurement (no /dev/dri access, the service stays DRM master): ftrace
kprobes on drm_mode_setplane (each frame the KMS wall latches; the legacy call
blocks until the flip), drm_mode_page_flip_ioctl (page flips, e.g. a GL wall),
and drm_mode_obj_set_property_ioctl (plane alpha writes), plus the
drm_vblank_event tracepoint for the exact refresh grid. Frames are assigned to
refreshes by the time their commit returned. Probes are removed afterwards.
For alpha files the plane's pixel format is read once from the DRM debugfs
atomic state and its blend mode through libdrm (opened only then, after the
service is master); that read takes the modeset locks, so it is made at the
end of steady play and the steady window stops just before it.

On the GPU compositor wall (the service runs with CUTEPI_WALL=gl, detected
from GET /api/debug/glwall), the plane is committed every refresh whatever is
shown, so the kernel trace cannot tell the cue's frames apart. The wall's
presenter counts instead, per cue layer and only for output frames actually
put on the plane: frames that showed a new frame of the cue, and frames whose
opacity for the cue differed from the previous one shown (both snapshotted as
the mixer selects its inputs). They are polled every 30 ms and counted per
presented frame in the same response, scaled by the presented rate over the
whole run, so the polling jitter cancels out. Alpha on the GPU wall is not
measured: the mixer always blends straight alpha.

Per file: upload, add a cue, set its fade in to 1 s, set the ESC fade to 1 s
(restored afterwards), play the cue, after 4 s trigger the fade out
(POST /api/fadeOut), then stop and delete the cue and the media. With
--readme, the table between the markers <!-- codec-support:start/end --> in
that file is replaced; results of files not run this time are kept.
"""
import datetime
import glob
import json
import os
import re
import sqlite3
import statistics as st
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import probe  # noqa: E402  (decoder used, hardware or software)

ARGS = sys.argv[1:]
ROOT = ARGS[0] if ARGS and not ARGS[0].startswith("--") else "/root/cutepi-testmedia/support"
def opt(name, default=None):
    return ARGS[ARGS.index(name) + 1] if name in ARGS else default
README = opt("--readme")
SECTION = opt("--section", "")   # README marker suffix, e.g. "gl" -> <!-- codec-support-gl:start -->
JSON_OUT = opt("--json")
ONLY = [o for o in (opt("--only") or "").split(",") if o]  # comma-separated name substrings
BASE = opt("--base", "http://127.0.0.1")
DB = opt("--db", "/root/cutepi/config/ctp.db")
T = "/sys/kernel/tracing"
FADE_S = 1.0
STEADY_S = 4.0
MIN_FPS = 59.0      # of 60 refreshes a second
MIN_STEPS = 59.0    # opacity changes a second during a fade


def model():
    try:
        return open("/proc/device-tree/model").read().strip("\x00\n ")
    except OSError:
        return "unknown board"


def output_path():
    """Which wall the service runs (from its systemd environment)."""
    env = ""
    for f in glob.glob("/etc/systemd/system/cutepi.service.d/*.conf"):
        env += open(f).read()
    if re.search(r"CUTEPI_WALL=gl", env):
        return "GPU compositor wall"
    if "kmssink" in env:
        return "KMS display planes"
    return "default sink"


# ---- service API ------------------------------------------------------------

def call(method, path, data=None, headers=None, form=None):
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers = dict(headers or {}, **{"Content-Type": "application/x-www-form-urlencoded"})
    req = urllib.request.Request(BASE + path, method=method, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def upload(path):
    b = "cutepi-support-boundary"
    with open(path, "rb") as f:
        body = f.read()
    data = b"\r\n".join([
        b"--" + b.encode(), b'Content-Disposition: form-data; name="onConflict"', b"", b"replace",
        b"--" + b.encode(),
        ('Content-Disposition: form-data; name="media"; filename="%s"' % os.path.basename(path)).encode(),
        b"Content-Type: application/octet-stream", b"", body, b"--" + b.encode() + b"--", b""])
    return call("POST", "/upload", data, {"Content-Type": "multipart/form-data; boundary=" + b, "HX-Request": "true"})


def cue_pos(filename):
    with sqlite3.connect("file:%s?mode=ro" % DB, uri=True) as c:
        row = c.execute("select max(c.cuePos) from cuesheet c join mediapool m on m.media_id = c.media_id "
                        "where m.filename = ?", (filename,)).fetchone()
    return row[0] if row else None


# ---- kernel trace -----------------------------------------------------------

PROBES = [
    "p:cutepi/setplane drm_mode_setplane plane=+0($arg2):u32 fb=+8($arg2):u32",
    "r:cutepi/setplane_ret drm_mode_setplane ret=$retval",
    "p:cutepi/flip drm_mode_page_flip_ioctl crtc=+0($arg2):u32 fb=+4($arg2):u32",
    "r:cutepi/flip_ret drm_mode_page_flip_ioctl ret=$retval",
    "p:cutepi/setprop drm_mode_obj_set_property_ioctl val=+0($arg2):u64 prop=+8($arg2):u32 obj=+12($arg2):u32",
    "r:cutepi/setprop_ret drm_mode_obj_set_property_ioctl ret=$retval",
]


def tw(p, v, mode="w"):
    # Raw write: Python's append mode seeks to the end on open, which tracefs
    # refuses (EINVAL); O_APPEND alone appends like the shell's >>.
    flags = os.O_WRONLY | (os.O_APPEND if mode == "a" else os.O_TRUNC)
    fd = os.open(os.path.join(T, p), flags)
    try:
        os.write(fd, v.encode())
    finally:
        os.close(fd)


def tr(p):
    with open(os.path.join(T, p)) as f:
        return f.read()


def probes_on():
    for p in PROBES:
        tw("kprobe_events", p + "\n", "a")
    tw("trace_clock", "mono")
    tw("buffer_size_kb", "16384")


def probes_off():
    try:
        tw("events/cutepi/enable", "0")
    except OSError:
        pass
    for p in PROBES:
        name = p.split()[0].split(":")[1]
        try:
            tw("kprobe_events", "-:%s\n" % name, "a")
        except OSError:
            pass
    tw("trace_clock", "local")


def trace_start():
    tw("trace", "")
    tw("events/cutepi/enable", "1")
    tw("events/drm/drm_vblank_event/enable", "1")


def trace_stop():
    tw("events/cutepi/enable", "0")
    tw("events/drm/drm_vblank_event/enable", "0")
    txt = tr("trace")
    tw("trace", "")
    return txt


RX = re.compile(r"-(\d+)\s+\[\d+\].*?\s(\d+\.\d+): (\w+): (.*)$")


def parse(txt):
    """frames: {object: [ret_time]}, props: {plane: [(ret_time)]}, vblank period/phase."""
    frames, props, vb, pend = {}, {}, [], {}
    for line in txt.splitlines():
        m = RX.search(line)
        if not m:
            continue
        tid, t, kind = m.group(1), float(m.group(2)), m.group(3)
        a = dict(re.findall(r"(\w+)=(\w+)", m.group(4)))
        if kind == "setplane":
            pend[tid] = ("plane", int(a["plane"]))
        elif kind == "flip":
            pend[tid] = ("crtc", int(a["crtc"]))
        elif kind == "setprop":
            pend[tid] = ("prop", int(a["obj"]))
        elif kind.endswith("_ret"):
            e = pend.pop(tid, None)
            if not e:
                continue
            if int(a.get("ret", "0"), 0) not in (0,):
                continue  # failed call: nothing presented or changed
            if e[0] == "prop":
                props.setdefault(e[1], []).append(t)
            else:
                frames.setdefault("%s %d" % e, []).append(t)
        elif kind == "drm_vblank_event":
            vb.append((int(a["seq"]), int(a["time"]) / 1e9))
    if len(vb) < 2:
        return frames, props, None, None
    s0 = vb[0][0]
    period = st.median([(b[1] - a[1]) / (b[0] - a[0]) for a, b in zip(vb, vb[1:]) if b[0] > a[0]])
    phase = st.median([t - (s - s0) * period for s, t in vb])
    return frames, props, period, phase


def window_fps(times, lo, hi, period, phase):
    """New frames presented per second in [lo, hi): refreshes with >=1 frame latched."""
    if hi <= lo:
        return None
    refs = {int(round((t - phase) / period)) for t in times if lo <= t < hi}
    return len(refs) / (hi - lo)


def rate(times, lo, hi):
    if hi <= lo:
        return None
    return sum(1 for t in times if lo <= t < hi) / (hi - lo)


# ---- what the file is, what the plane shows ---------------------------------

def source_info(path):
    """(frames per second, frame count) of the first video stream, from ffprobe."""
    try:
        out = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets",
                              "-show_entries", "stream=avg_frame_rate,r_frame_rate,nb_read_packets",
                              "-of", "json", path], capture_output=True, text=True, timeout=60).stdout
        stream = (json.loads(out).get("streams") or [{}])[0]
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return None, None
    fps = None
    for k in ("avg_frame_rate", "r_frame_rate"):
        n, _, d = stream.get(k, "0/0").partition("/")
        if d and int(d) and int(n):
            fps = int(n) / int(d)
            break
    n = stream.get("nb_read_packets")
    return fps, int(n) if n else None


def debugfs_formats():
    """{plane object id: fourcc} from the display's DRM debugfs atomic state."""
    for f in glob.glob("/sys/kernel/debug/dri/*/state"):
        try:
            txt = open(f).read()
        except OSError:
            continue
        if "pixelvalve" not in txt and "crtc=" not in txt:
            continue
        out, cur = {}, None
        for line in txt.splitlines():
            m = re.match(r"plane\[(\d+)\]", line)
            if m:
                cur = int(m.group(1))
            m = re.search(r"format=(\S+)", line)
            if m and cur is not None:
                out[cur] = m.group(1)
        if out:
            return out
    return {}


_drm = None


def blend_modes():
    """{plane object id: pixel blend mode name}, through libdrm (read only)."""
    global _drm
    import ctypes as C

    class OProps(C.Structure):
        _fields_ = [("count_props", C.c_uint32), ("props", C.POINTER(C.c_uint32)),
                    ("prop_values", C.POINTER(C.c_uint64))]

    class Enum(C.Structure):
        _fields_ = [("value", C.c_uint64), ("name", C.c_char * 32)]

    class Prop(C.Structure):
        _fields_ = [("prop_id", C.c_uint32), ("flags", C.c_uint32), ("name", C.c_char * 32),
                    ("count_values", C.c_int), ("values", C.POINTER(C.c_uint64)),
                    ("count_enums", C.c_int), ("enums", C.POINTER(Enum))]

    class PRes(C.Structure):
        _fields_ = [("count_planes", C.c_uint32), ("planes", C.POINTER(C.c_uint32))]

    if _drm is None:
        lib = C.CDLL("libdrm.so.2")
        lib.drmModeObjectGetProperties.restype = C.POINTER(OProps)
        lib.drmModeGetProperty.restype = C.POINTER(Prop)
        lib.drmModeGetPlaneResources.restype = C.POINTER(PRes)
        fd = None
        for dev in sorted(glob.glob("/dev/dri/card*")):
            try:
                f = os.open(dev, os.O_RDWR)
            except OSError:
                continue
            lib.drmSetClientCap(f, 2, 1)  # universal planes
            lib.drmSetClientCap(f, 3, 1)  # atomic: exposes the blend-mode property
            res = lib.drmModeGetPlaneResources(f)
            if res and res.contents.count_planes > 2:
                fd = f
                break
            os.close(f)
        _drm = (lib, fd)
    lib, fd = _drm
    if fd is None:
        return {}
    res = lib.drmModeGetPlaneResources(fd).contents
    out = {}
    for i in range(res.count_planes):
        pid = res.planes[i]
        op = lib.drmModeObjectGetProperties(fd, pid, 0xEEEEEEEE).contents
        for j in range(op.count_props):
            pr = lib.drmModeGetProperty(fd, op.props[j]).contents
            if pr.name == b"pixel blend mode":
                names = {pr.enums[k].value: pr.enums[k].name.decode() for k in range(pr.count_enums)}
                out[pid] = names.get(op.prop_values[j], str(op.prop_values[j]))
    return out


def has_alpha(fourcc):
    """DRM fourccs with an alpha channel: AR24, AB24, RA24, BA24, AR30, AR15, ..."""
    return fourcc[:2] in ("AR", "AB", "RA", "BA")


def gl_wall():
    """True when the live service runs the GPU wall (its counters endpoint is on)."""
    try:
        st_, body = call("GET", "/api/debug/glwall")
        return st_ == 200 and json.loads(body).get("on") is True
    except Exception:
        return False


def gl_sample():
    """(time, shown_frames, shown_steps, presented) of the cue under test, or None.

    shown_frames: output frames on screen that showed a new frame of the cue;
    shown_steps: output frames on screen whose opacity for the cue differed
    from the previous one shown. Both are counted by the wall's presenter
    after the frame is on the plane, so a frame the mixer dropped or the
    presenter skipped is not credited (DESIGN 6.1.1)."""
    st_, body = call("GET", "/api/debug/glwall")
    if st_ != 200:
        return None
    d = json.loads(body)
    # The cue under test is the newest visible layer; an armed panic image
    # (parked) and cues fading out are also attached.
    cand = [l for l in d.get("layers") or [] if l.get("Visible") and not l.get("Parked")]
    if not cand:
        return None
    l = max(cand, key=lambda x: x.get("Seq", 0))
    return (time.monotonic(), l["ShownFrames"], l["ShownSteps"], d.get("presented", 0), l.get("Route", ""), l.get("Caps", ""))


def gl_per_refresh(samples, lo, hi, idx, hz):
    """Counter idx per presented output frame over [lo, hi), scaled to hz.

    Counted against the wall's own presented-frame counter, read in the same
    response, so the HTTP polling jitter (a 30 ms sample interval is +-1.5 %
    of a 1 s window) cancels out. hz is the presented rate over the whole
    run (a long window, so precise); a presenter missing refreshes shows
    there."""
    inside = [smp for smp in samples if lo <= smp[0] < hi]
    if len(inside) < 2:
        return None
    a, b = inside[0], inside[-1]
    if b[3] <= a[3]:
        return None
    return (b[idx] - a[idx]) / (b[3] - a[3]) * hz


def gl_rate(samples, lo, hi, idx):
    """Counter idx (1 frames shown, 2 steps shown, 3 presented) per second over [lo, hi), from the samples nearest the bounds."""
    inside = [smp for smp in samples if lo <= smp[0] < hi]
    if len(inside) < 2:
        return None
    a, b = inside[0], inside[-1]
    if b[0] <= a[0]:
        return None
    return (b[idx] - a[idx]) / (b[0] - a[0])


def audio_running():
    for f in glob.glob("/proc/asound/card*/pcm*p/sub*/status"):
        try:
            if "RUNNING" in open(f).read():
                return True
        except OSError:
            pass
    return False


# ---- one file ---------------------------------------------------------------

def run(path, mdl):
    name = os.path.basename(path)
    kind = os.path.basename(os.path.dirname(path))
    stem, ext = os.path.splitext(name)
    res = {"file": name, "codec": stem, "container": ext.lstrip(".").upper(), "kind": kind, "status": "",
           "tested": datetime.date.today().isoformat()}
    secs, decs, derr = probe.decode(path)
    res["decoders"] = decs
    res["hardware"] = any(d.startswith("v4l2") for d in decs)
    is_alpha = "alpha" in stem
    src_fps, src_frames = source_info(path) if kind != "audio" else (None, None)
    # Animated: more than one frame, or named so (this ffmpeg cannot read
    # animated WebP, so the support set names its rate: "*_anim_30fps.*").
    animated = kind == "image" and ((src_frames or 0) > 1 or "anim" in stem)
    m = re.search(r"_(\d+)fps", stem)
    if m:
        src_fps = float(m.group(1))
    if animated:
        res["source_fps"] = round(src_fps, 2) if src_fps else None
    st_, body = upload(path)
    if st_ != 200:
        res["error"] = "import refused: " + (body.strip().splitlines() or [""])[-1][:140]
        res["status"] = mdl + " - unsupported"
        return res
    try:
        call("POST", "/api/cue/add/" + urllib.parse.quote(name))
        pos = cue_pos(name)
        if not pos:
            raise RuntimeError("cue not created")
        call("PUT", "/api/cue/%d/edit/fadeIn" % pos, form={"val": "1s"})
        if kind == "audio":
            call("POST", "/api/cue/%d/play" % pos)
            t0 = time.monotonic()
            samples = []
            while time.monotonic() - t0 < 1.0 + STEADY_S:
                samples.append(audio_running())
                time.sleep(0.05)
            call("POST", "/api/fadeOut")
            t1 = time.monotonic()
            while time.monotonic() - t1 < FADE_S * 0.9:
                samples.append(audio_running())
                time.sleep(0.05)
            run_ok = all(samples[5:]) if len(samples) > 5 else False  # allow 250 ms to open the device
            res["audio_running"] = "%d/%d samples" % (sum(samples), len(samples))
            res["ok"] = run_ok
            res["status"] = "Supported" if run_ok else mdl + " - unsupported"
            return res
        if GL:
            # GPU wall: the plane is committed every refresh whatever is
            # shown, so the cue's own frames and opacity changes are read
            # from the wall's per-layer counters of presented output frames
            # (/api/debug/glwall), sampled every 30 ms.
            t_play = time.monotonic()
            call("POST", "/api/cue/%d/play" % pos)
            samples = []
            # Play returns once the cue has prerolled (seconds for a heavy
            # file); the steady window must not shrink by that.
            t_due = time.monotonic() + FADE_S + STEADY_S   # when the fade out is triggered
            t_fo = None
            while time.monotonic() < t_due + FADE_S + 0.3:
                if t_fo is None and time.monotonic() >= t_due:
                    call("POST", "/api/fadeOut")
                    t_fo = time.monotonic()
                smp = gl_sample()
                if smp:
                    samples.append(smp)
                time.sleep(0.03)
            if t_fo is None:
                t_fo = t_due
            if not samples:
                raise RuntimeError("no wall layer appeared")
            t_first = next((smp[0] for smp in samples if smp[1] >= 1), samples[0][0])
            res["first_frame_ms"] = round((t_first - t_play) * 1000)
            # The fade in starts with the first frame shown; both fade windows
            # leave 50 ms at each end, so no frame before or after a fade is
            # counted as a missing step.
            # The fade out reaches the screen one wall latency (~90 ms) after
            # it is asked for: the wall steps it per output frame from each
            # frame's own time, and a frame is shown that long after it. Its
            # window starts at the first opacity step seen on screen, as the
            # fade in's starts at the first frame shown.
            st_fo = next((smp[2] for smp in samples if smp[0] >= t_fo), None)
            t_fo_seen = next((smp[0] for smp in samples if smp[0] >= t_fo and st_fo is not None and smp[2] > st_fo), t_fo)
            res["fade_out_delay_ms"] = round((t_fo_seen - t_fo) * 1000)
            win = {"fade_in": (t_first + 0.05, t_first + FADE_S - 0.05), "steady": (t_first + FADE_S + 0.3, t_fo - 0.1),
                   "fade_out": (t_fo_seen + 0.05, t_fo_seen + FADE_S - 0.05)}
            hz = gl_rate(samples, t_first, samples[-1][0] + 0.001, 3)
            res["refresh_hz"] = round(hz, 2) if hz is not None else None
            for w, (lo, hi) in win.items():
                fps = gl_per_refresh(samples, lo, hi, 1, hz or 0)
                res[w + "_fps"] = round(fps, 1) if fps is not None else None
                if w != "steady":
                    st = gl_per_refresh(samples, lo, hi, 2, hz or 0)
                    res[w + "_steps"] = round(st, 1) if st is not None else None
            plane = None
        else:
            trace_start()
            t_play = time.monotonic()
            call("POST", "/api/cue/%d/play" % pos)
            time.sleep(FADE_S + STEADY_S)
            t_read = time.monotonic()
            if is_alpha:
                formats, blends = debugfs_formats(), blend_modes()
            t_fo = time.monotonic()
            call("POST", "/api/fadeOut")
            time.sleep(FADE_S + 0.6)
            txt = trace_stop()
            frames, props, period, phase = parse(txt)
            if not period:
                raise RuntimeError("no vblank events traced")
            res["refresh_hz"] = round(1 / period, 2)
            # The cue's output: the plane (or CRTC flip stream) with the most
            # frames after the play started.
            cand = {k: [t for t in v if t >= t_play] for k, v in frames.items()}
            cand = {k: v for k, v in cand.items() if v}
            if not cand:
                raise RuntimeError("no frames presented")
            key, times = max(cand.items(), key=lambda kv: len(kv[1]))
            t_first = times[0]
            res["first_frame_ms"] = round((t_first - t_play) * 1000)
            plane = int(key.split()[1]) if key.startswith("plane") else None
            steps = props.get(plane, []) if plane is not None else []
            win = {"fade_in": (t_first, t_first + FADE_S), "steady": (t_first + FADE_S + 0.3, t_read - 0.02),
                   "fade_out": (t_fo + 0.05, t_fo + FADE_S - 0.05)}
            for w, (lo, hi) in win.items():
                fps = window_fps(times, lo, hi, period, phase)
                res[w + "_fps"] = round(fps, 1) if fps is not None else None
                if w != "steady":
                    # Opacity changes: plane alpha writes on the KMS wall; on a
                    # GL wall the mixer applies the fade to every output frame.
                    s = rate(steps, lo, hi) if plane is not None else fps
                    res[w + "_steps"] = round(s, 1) if s is not None else None
        smooth = all((res[w + "_steps"] or 0) >= MIN_STEPS for w in ("fade_in", "fade_out"))
        if kind == "image" and not animated:
            ok = smooth
        else:
            # Video: a new frame every refresh. Animated images: every frame of
            # the file, at its own rate (GIF cannot express 60 fps).
            # Animated images: every frame of the file, less one per window (as
            # 59 of 60 for video): at 25 fps a 0.9 s window holds ~22 frames,
            # so one frame either side moves the rate by 1.1 fps.
            need = MIN_FPS if not animated else (src_fps or 60) - 1 / (FADE_S - 0.1)
            res["need_fps"] = round(need, 1)
            ok = smooth and all((res[w + "_fps"] or 0) >= need for w in win)
        if is_alpha and GL:
            # The mixer blends every layer with straight alpha over the layers
            # beneath, so what decides is whether the cue's frames keep their
            # alpha on the way: only the RGBA upload route does (the ISP and
            # the hardware decoders' formats carry none).
            route = samples[-1][4] if samples else ""
            m = re.search(r"format=\(string\)(\w+)", samples[-1][5] if samples else "")
            res["plane_format"] = "GL layer, %s route%s" % (route or "?", ", " + m.group(1) if m else "")
            res["blend_mode"] = "straight"
            res["alpha_ok"] = route == "alpha"
            ok = ok and res["alpha_ok"]
        elif is_alpha:
            fourcc = formats.get(plane, "?") if plane is not None else "not a plane"
            blend = blends.get(plane, "?") if plane is not None else "-"
            res["plane_format"], res["blend_mode"] = fourcc, blend
            res["alpha_ok"] = has_alpha(fourcc) and blend == "Coverage"
            ok = ok and res["alpha_ok"]
        res["ok"] = ok
        res["status"] = "Supported" if ok else mdl + " - unsupported"
        return res
    finally:
        call("POST", "/api/stop")
        time.sleep(0.3)
        pos = cue_pos(name)
        if pos:
            call("DELETE", "/api/cue/%d" % pos)
        call("DELETE", "/api/media/" + urllib.parse.quote(name))


# ---- README table -----------------------------------------------------------

def fmt_row(r):
    def f(v):
        return "-" if v is None else ("%g" % v)
    dec = ", ".join(d for d in r.get("decoders", []) if not d.startswith(("avdec_aac", "avdec_ac3", "opusdec",
                                                                          "mpg123", "flacdec", "vorbisdec"))
                    or r["kind"] == "audio") or "-"
    if r.get("error"):
        meas = r["error"]
    elif r["kind"] == "audio":
        meas = "HDMI stream running " + r.get("audio_running", "-")
    elif r["kind"] == "image" and not r.get("source_fps"):
        meas = "fade in %s steps/s, fade out %s steps/s" % (f(r.get("fade_in_steps")), f(r.get("fade_out_steps")))
    else:
        meas = "steady %s fps; fade in %s fps (%s steps/s); fade out %s fps (%s steps/s)" % (
            f(r.get("steady_fps")), f(r.get("fade_in_fps")), f(r.get("fade_in_steps")),
            f(r.get("fade_out_fps")), f(r.get("fade_out_steps")))
        if r.get("source_fps"):
            meas = "animated, %s fps file; " % f(r["source_fps"]) + meas
    if r.get("plane_format") and not r.get("error"):
        meas += "; alpha: plane %s, blend %s" % (r["plane_format"], r["blend_mode"])
    status = "**Supported**" if r["status"] == "Supported" else r["status"]
    return "| %s | %s | %s | %s | %s | %s | %s |" % (
        r["kind"], r["codec"], r["container"], dec, "yes" if r.get("hardware") else "no", meas, status)


def write_readme(results, mdl, path_label):
    tag = "codec-support" + ("-" + SECTION if SECTION else "")
    start, end = "<!-- %s:start -->" % tag, "<!-- %s:end -->" % tag
    text = open(README).read() if os.path.exists(README) else ""
    old = {}
    if start in text:
        m = re.search(re.escape(start) + r".*?<!-- " + re.escape(tag) + r":data (.*?) -->", text, re.S)
        if m:
            try:
                old = {r["file"] + r["kind"]: r for r in json.loads(m.group(1))}
            except ValueError:
                old = {}
    for r in results:
        old[r["file"] + r["kind"]] = r
    rows = sorted(old.values(), key=lambda r: ({"video": 0, "image": 1, "audio": 2}.get(r["kind"], 3), r["codec"], r["container"]))
    n_ok = sum(1 for r in rows if r["status"] == "Supported")
    body = [start, "",
            "Tested on **%s**, output: %s, %s. %d of %d codec/container combinations supported." % (
                mdl, path_label, max(r["tested"] for r in rows), n_ok, len(rows)),
            "",
            "| Type | Codec | Container | Video decoder | Hardware | Measured (1 s fade in, steady, 1 s fade out) | Status |",
            "|---|---|---|---|---|---|---|"]
    body += [fmt_row(r) for r in rows]
    body += ["", "<!-- %s:data %s -->" % (tag, json.dumps(rows, separators=(",", ":"))), end]
    block = "\n".join(body)
    if start in text and end in text:
        text = text[:text.index(start)] + block + text[text.index(end) + len(end):]
    else:
        text = text.rstrip() + "\n\n" + block + "\n"
    with open(README, "w") as f:
        f.write(text)


GL = False


def main():
    global GL
    mdl = model()
    GL = gl_wall()
    path_label = "GPU compositor wall" if GL else output_path()
    # Only the test files: video/, image/ and audio/ (helper files such as the
    # alpha mask live beside them).
    files = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(ROOT) for f in fs
                   if os.path.basename(dp) in ("video", "image", "audio"))
    if ONLY:
        files = [f for f in files if any(o in os.path.basename(f) for o in ONLY)]
    _, s = call("GET", "/api/settings")
    esc_before = json.loads(s).get("escFadeMs", 1000)
    results = []
    try:
        call("POST", "/api/settings", json.dumps({"escFadeMs": int(FADE_S * 1000)}).encode(), {"Content-Type": "application/json"})
        if not GL:
            probes_on()
        for path in files:
            try:
                r = run(path, mdl)
            except Exception as e:  # keep going; record the failure
                r = {"file": os.path.basename(path), "codec": os.path.splitext(os.path.basename(path))[0],
                     "container": os.path.splitext(path)[1].lstrip(".").upper(),
                     "kind": os.path.basename(os.path.dirname(path)), "status": mdl + " - unsupported",
                     "error": "test error: %s" % e, "tested": datetime.date.today().isoformat()}
            results.append(r)
            print(fmt_row(r), file=sys.stderr, flush=True)
    finally:
        if not GL:
            probes_off()
        call("POST", "/api/settings", json.dumps({"escFadeMs": esc_before}).encode(), {"Content-Type": "application/json"})
    if JSON_OUT:
        with open(JSON_OUT, "w") as f:
            json.dump(results, f, indent=1)
    if README:
        write_readme(results, mdl, path_label)
    print("%d of %d supported" % (sum(r["status"] == "Supported" for r in results), len(results)))


if __name__ == "__main__":
    main()
