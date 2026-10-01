#!/usr/bin/env python3
"""probe.py [DIR] [--json OUT] — decode every file of the codec corpus the way
CuTePi does and report, per file: the codecs (ffprobe), the decoder
GStreamer picked (hardware V4L2 or software), and how fast it decodes.

Decoding uses playbin (the same decodebin autoplugging and decoder ranks as
the service, including the v4l2jpegdec and openjpegdec demotions) into fake sinks with
sync=false, so the speed is the decoder's capacity, not the display's. A
video's speed is reported as decoded frames per second against its own frame
rate; audio as a multiple of real time; an image as milliseconds to decode.

Only what the Pi already has is used (python3, gst-launch-1.0, and ffprobe,
which the service itself uses to read media). Nothing is played on the display or the audio device.
"""
import json
import os
import re
import subprocess
import sys
import time

ROOT = sys.argv[1] if len(sys.argv) > 1 and not sys.argv[1].startswith("--") else "/root/cutepi-testmedia"
JSON_OUT = sys.argv[sys.argv.index("--json") + 1] if "--json" in sys.argv else None
# The same decoder demotions as the service (gsp decoderRankOverrides).
ENV = dict(os.environ, GST_PLUGIN_FEATURE_RANK="v4l2jpegdec:0,openjpegdec:0", GST_DEBUG="0")
# State-change debug lines name every element in the pipeline, decodebin's
# internals included (-v output does not).
DECODE_ENV = dict(ENV, GST_DEBUG="GST_STATES:4", GST_DEBUG_NO_COLOR="1")
NOT_DECODERS = {"decodebin", "uridecodebin", "decodebin3", "uridecodebin3", "parsebin"}


def discover(path):
    """Codec, size and rate facts from ffprobe (as the service's importer reads them)."""
    r = subprocess.run(["ffprobe", "-v", "error", "-print_format", "json", "-show_streams", "-show_format", path],
                       capture_output=True, text=True, timeout=60)
    info = {"video": None, "audio": None, "duration": None, "error": None}
    try:
        d = json.loads(r.stdout or "{}")
    except ValueError:
        d = {}
    if r.returncode != 0:
        info["error"] = (r.stderr.strip().splitlines() or ["ffprobe failed"])[-1][:160]
    if d.get("format", {}).get("duration"):
        info["duration"] = float(d["format"]["duration"])
    for st in d.get("streams", []):
        if st.get("codec_type") == "video" and not info["video"]:
            num, _, den = (st.get("avg_frame_rate") or "0/0").partition("/")
            fps = int(num) / int(den) if den and int(den) else None
            pf = st.get("pix_fmt", "")
            info["video"] = {
                "codec": (st.get("codec_name", "?") + (" " + st["profile"] if st.get("profile") else "")).strip(),
                "width": st.get("width"), "height": st.get("height"), "fps": fps,
                "interlaced": st.get("field_order", "progressive") not in ("progressive", "unknown"),
                "depth": 10 if "10" in pf else 12 if "12" in pf else 16 if ("48" in pf or "16" in pf) else 8,
                "pix_fmt": pf,
            }
        elif st.get("codec_type") == "audio" and not info["audio"]:
            info["audio"] = {"codec": st.get("codec_name", "?"), "channels": st.get("channels"),
                             "rate": int(st.get("sample_rate", 0) or 0)}
    return info


def decode(path):
    """Decode the whole file as fast as possible; return (seconds, decoders, error)."""
    uri = "file://" + os.path.abspath(path)
    cmd = ["gst-launch-1.0", "playbin", f"uri={uri}",
           "video-sink=fakevideosink sync=false", "audio-sink=fakesink sync=false", "flags=0x3"]
    t = time.monotonic()
    r = subprocess.run(cmd, capture_output=True, text=True, env=DECODE_ENV, timeout=600)
    secs = time.monotonic() - t
    out = r.stdout + r.stderr
    names = set()
    for m in re.finditer(r"<([a-z][a-z0-9_-]*?)(\d+)>", out):
        el = m[1].rstrip("-")  # names like avdec_ac3-0
        if el in NOT_DECODERS:
            continue
        if el.endswith("dec") or el.startswith("avdec_") or el in ("imagefreeze",):
            names.add(el)
    err = None
    if r.returncode != 0:
        e = [l for l in out.splitlines() if l.startswith("ERROR") or l.startswith("Missing element")]
        err = (e[0] if e else "exit %d" % r.returncode)[:160]
    return secs, sorted(n for n in names if n != "imagefreeze"), err


def kind(path):
    return os.path.basename(os.path.dirname(path))


def main():
    rows = []
    files = sorted(os.path.join(dp, f) for dp, _, fs in os.walk(ROOT) for f in fs)
    for path in files:
        k = kind(path)
        info = discover(path)
        secs, decs, err = decode(path)
        hw = any(d.startswith("v4l2") for d in decs)
        row = {**info, "file": os.path.basename(path), "kind": k, "size": os.path.getsize(path), "decoders": decs,
               "hardware": hw, "seconds": round(secs, 3), "error": err or info["error"]}
        v, a, dur = info["video"], info["audio"], info["duration"]
        speed = ""
        if err:
            speed = "FAILED"
        elif k == "image":
            speed = "%d ms" % round(secs * 1000)
        elif k == "video" and v and v["fps"] and dur:
            fps = v["fps"] * dur / secs
            row["decode_fps"] = round(fps, 1)
            speed = "%.0f fps (%s at %.4g fps)" % (fps, "OK" if fps >= v["fps"] * 1.05 else "BELOW", v["fps"])
        elif dur:
            row["realtime"] = round(dur / secs, 1)
            speed = "%.0f× real time" % (dur / secs)
        row["speed"] = speed
        rows.append(row)
        print(f"{row['file']}: {', '.join(decs) or '-'} {'HW' if hw else 'SW'} {speed} {row['error'] or ''}", file=sys.stderr, flush=True)

    print("| File | Codecs | Format | Decoder | HW | Decode speed | Result |")
    print("|---|---|---|---|---|---|---|")
    for r in rows:
        v, a = r["video"], r["audio"]
        codecs = " + ".join(x for x in [v and v["codec"], a and a["codec"]] if x) or "-"
        fmt = []
        if v and v["width"]:
            fmt.append("%dx%d" % (v["width"], v["height"]))
            if v["fps"]:
                fmt.append("@%.4g%s" % (v["fps"], "i" if v["interlaced"] else ""))
            if v["depth"] and v["depth"] > 8:
                fmt.append(" %d-bit" % v["depth"])
        if a and a["rate"]:
            fmt.append(" %d Hz %dch" % (a["rate"], a["channels"] or 0))
        print("| %s | %s | %s | %s | %s | %s | %s |" % (
            r["file"], codecs, "".join(fmt).strip() or "-", ", ".join(r["decoders"]) or "-",
            "yes" if r["hardware"] else "no", r["speed"] if not r["error"] else "-",
            "ok" if not r["error"] else "**fails**: " + r["error"].replace("|", "/")))
    if JSON_OUT:
        with open(JSON_OUT, "w") as f:
            json.dump(rows, f, indent=1)


if __name__ == "__main__":
    main()
