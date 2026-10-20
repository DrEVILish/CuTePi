#!/usr/bin/env python3
"""support_merge.py PASS.json... --readme README.md [--section NAME]

Merges several support.py passes (--json) into one README table. Single runs vary (a background job, a cold
read of a large file), so a file is listed **Supported** only if it passed in a majority of the passes; its row
shows that pass's measurement (the passing run with the lowest steady rate, else the failing run with the median
steady rate) and "passed N of M"."""
import json
import os
import statistics
import sys

args = sys.argv[1:]
files = [a for a in args if a.endswith(".json")]
rest = [a for a in args if not a.endswith(".json")]
sys.argv = [sys.argv[0]] + rest  # support.py reads --readme / --section from argv at import
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import support  # noqa: E402

runs = {}
for f in files:
    for r in json.load(open(f)):
        runs.setdefault((r["file"], r["kind"]), []).append(r)
merged = []
for key, rs in runs.items():
    ok = [r for r in rs if r["status"] == "Supported"]
    if len(ok) * 2 > len(rs):
        pick = min(ok, key=lambda r: r.get("steady_fps") or 0)
    else:
        bad = sorted([r for r in rs if r["status"] != "Supported"], key=lambda r: r.get("steady_fps") or 0)
        pick = dict(bad[len(bad) // 2])
        pick["status"] = bad[0]["status"]
    pick = dict(pick)
    pick["passes"] = "%d of %d" % (len(ok), len(rs))
    pick.pop("samples", None)
    merged.append(pick)
mdl = support.model()
support.write_readme(merged, mdl, "plane wall, CuTePi presenter, %d passes" % len(files))
print("%d of %d supported (majority of %d passes)" % (sum(r["status"] == "Supported" for r in merged), len(merged), len(files)))
