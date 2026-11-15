What this repo does: CuTePi runs cue-based HDMI video and audio playback on Raspberry Pi, with browser/remote control, live pages, layers, schedules and linked units. The assumed load is continuous playback, a few operators, and occasional edits/imports.

This records the independent audit followed by the authorised ub-mode fixes. Findings 1–13 were verified in the starting tree; 14 and 15 were discovered and reproduced during verification. All 15 have fixes in the shared working tree. Prior uncommitted work was preserved.

The authority for the fixes is the permanent regression suite. The earlier temporary overlay was a reproduction aid and no longer matches the updated optional-opacity API.

## Must fix

1. **Critical — APNG frame coordinate overflow** (`gsp/animdec/animdec.c`)
   - **What this is:** The animated-image decoder validates each APNG frame rectangle before copying its pixels.
   - **Problem:** A small crafted APNG passed metadata probing and crashed DecodeCheck.
   - **Fix:** Overflow-safe coordinate and dimension checks; subprocess regression. **Status: fixed.**
   - **If we skip it:** One malformed import can terminate the server and on-air playback.

2. **Warning — Foreign origin trust registration** (`routes/link.go; ws/hub.go`)
   - **What this is:** Linked units let a browser fail over to another unit using an origin-bound token.
   - **Problem:** Foreign GET registered a hostile page origin and later mutations passed without a token.
   - **Fix:** Reject foreign registration, authenticate cross-origin HTTP/WS requests per token, revoke removed-peer origins; regressions. **Status: fixed.**
   - **If we skip it:** A hostile webpage can gain cross-origin access to paired units.

3. **Warning — Stale warm media source** (`gsp/gsp_warm.go; gsp/gsp_pipeline.go`)
   - **What this is:** The selected and next cues keep prebuilt pipelines so GO can start promptly.
   - **Problem:** Atomic source replacement retained a pipeline prerolled from the old inode.
   - **Fix:** Resolve source path in one helper, compare inode/size/mtime on every warm match, clear identity on retirement. **Status: fixed.**
   - **If we skip it:** GO can play old content after a successful replacement upload.

4. **Warning — Show order and stable selection lost** (`ctp/export.go`)
   - **What this is:** Show archives must preserve the operator's cue/header order and selected item.
   - **Problem:** Export sorted stable positions rather than merged visual order; restored sequence differed.
   - **Fix:** Archive cue/header indices and stable local positions, translate indices on import, remap selected cues/groups. **Status: fixed.**
   - **If we skip it:** Restoring a show can change its GO sequence or select the wrong item.

5. **Warning — Group parent creation-order assumption** (`ctp/export.go`)
   - **What this is:** An imported group tree needs new local IDs without changing its parent relationships.
   - **Problem:** Older child nested under newer parent restored top-level.
   - **Fix:** Create all groups first, validate and assign parent relationships in a second transaction pass. **Status: fixed.**
   - **If we skip it:** Restored nested groups can silently become top-level.

6. **Warning — Schedule settings absent from archives** (`ctp/export.go`)
   - **What this is:** A cue's recurring weekday/time schedule is part of its saved show.
   - **Problem:** An enabled weekday/time disappeared after export/import.
   - **Fix:** Export and validate/import all schedule fields. **Status: fixed.**
   - **If we skip it:** A restored show can stop firing automatically.

7. **Warning — Explicit zero opacity became full opacity** (`ctp/export.go; gsp/wall.go; routes/index.go`)
   - **What this is:** Opacity controls picture visibility; zero means fully transparent.
   - **Problem:** Zero opacity restored as100 and playback also treated0 as default.
   - **Fix:** Optional archive opacity plus explicit playback opacity marker preserve zero and old defaults. **Status: fixed.**
   - **If we skip it:** An intended invisible cue can become fully visible.

8. **Warning — Scheduled warm start ignored latest options** (`routes/scheduler.go`)
   - **What this is:** The scheduler re-fetches a cue before firing so late edits apply.
   - **Problem:** Rate changed1 to2 after arming; playback used1.
   - **Fix:** Build options from the freshly fetched cue before warm-slot matching. **Status: fixed.**
   - **If we skip it:** A scheduled cue can ignore the latest trim, rate, gain or other settings.

9. **Warning — DST schedule calculation** (`ctp/ctp_schedule.go`)
   - **What this is:** The next-schedule calculation tells the scheduler when to poll closely.
   - **Problem:** London clock-change day reported14:00 as15:00.
   - **Fix:** Use calendar dates and local clock fields; include next same weekday seven days ahead. **Status: fixed.**
   - **If we skip it:** Clock-change days can put the scheduler outside a cue's firing window.

14. **Warning — Obsolete encode published after source replacement** (`media/prepare.go`)
   - **What this is:** An encode may keep reading an old source inode after the operator replaces its filename.
   - **Problem:** Controlled encode succeeded and published old source after an atomic replacement.
   - **Fix:** Validate source identity before publishing and discard obsolete output; regression. **Status: fixed.**
   - **If we skip it:** A new upload can play prepared content from its predecessor.

15. **Warning — Unsafe generic caps-list marshalling** (`gsp/wall.go; gsp/signals.go`)
   - **What this is:** The playback code reads a format field from fixed or unfixed GStreamer caps.
   - **Problem:** Existing TestGLTailFormat crashed checkptr in go-gst marshalValueList.
   - **Fix:** Native gst_structure_get_string scalar accessor avoids broken list conversion without disabling checks or changing dependencies. **Status: fixed.**
   - **If we skip it:** Race/checkptr builds crash while inspecting an unfixed format list.

## Should fix

10. **Warning — Encoder launched while already busy** (`media/prepare.go`)
   - **What this is:** The show compiler encodes prepared media in the background.
   - **Problem:** Controlled encoder ran while PlaybackBusy was true.
   - **Fix:** Wait for idle before launching; cancellation works while waiting. **Status: fixed.**
   - **If we skip it:** Encoding competes with live playback before the first busy check.

11. **Warning — Cross-test database lifetime races** (`routes test setup; gsp app callbacks; routes/link.go`)
   - **What this is:** Route tests replace a package-wide database between fixtures.
   - **Problem:** Race detector observed old callbacks/election jobs reading the next test database.
   - **Fix:** Account for dispatched callbacks and election jobs; stop/detach/drain before replacing test DB. **Status: fixed.**
   - **If we skip it:** Old callbacks can read or modify the following test's database.

12. **Warning — Hardware test failed without CMA heap** (`gsp/bobdeint/bobdeint_test.go`)
   - **What this is:** The DMA-output test requires a usable CMA DMA heap.
   - **Problem:** CT110 produced zero DMA pictures and failed the complete suite.
   - **Fix:** Explicit availability/permission skip; real DMA checks stay enabled where hardware exists. **Status: fixed.**
   - **If we skip it:** Ordinary container test runs remain red and hide new failures.

## Nice to have

13. **Suggestion — Unused legacy import helpers** (`ctp/export.go`)
   - **What this is:** Older import helpers duplicate the transactional importer.
   - **Problem:** ImportGroups, rollback helper and SelectedCuePosFor had no callers.
   - **Fix:** Deleted obsolete path and comments after whole-tree reference search. **Status: fixed.**
   - **If we skip it:** Maintenance has competing import rules to understand and keep consistent.

## Verification

- `go build -o /tmp/cutepi-audit-ub .` — passed inside CT110.
- `go vet ./...` — passed.
- `go test ./... -count=1` — all packages passed after the last changes. `TestDMABufOutput` skips explicitly when the CMA heap is absent or inaccessible.
- `go test -race ./routes ./ctp ./config ./ws -count=1` — passed.
- `go test -race ./media -count=1` — passed.
- `go test -race ./gsp -count=1` — passed with checkptr enabled after the scalar caps accessor fix.
- Security, archive order/selection/nesting/schedules/zero-opacity, DST, warm-source replacement, late scheduled options and busy/prepared-source regressions were observed failing before their fixes.
- Extended archive checks cover append/overwrite, group selection, selection across position gaps, blank-number layer targets, tied indices and old-manifest defaults.
- Before the inherited baseline was checkpointed, `git diff --check` reported only existing EOF blank lines in `public/css/comp/themes.css:170` and `templates/settingsModal.html:470`; the audit fixes introduced no new whitespace errors.

## Action log and integration

`actions.log` records the actions and reasons. `changes.patch` and `changed-files.json` compare this task with the captured starting files, rather than with HEAD, so earlier WIP is excluded. `baseline/`, `baseline.diff`, `baseline-status.txt` retain the original state.

At subagent handoff, no commit, push, deployment or test-server change had been performed. The project lead subsequently integrated the changes as separate baseline, security and remaining-fix commits; no service deployment or network changes are included.

The named crapper, dryer and mutator tools were absent from the available catalog and installed CLI paths; no results are claimed for them. Fixes used behavior regressions, normal tests, race/checkptr checks, build and vet.

Verdict: All 15 concrete findings fixed and checked in CT110; physical output acceptance remains outstanding.

Lean: -55 obsolete helper/comment lines, -0 dependencies possible; necessary correctness fixes add code and regressions.

Not checked: physical HDMI/audio and <50 ms output latency, long-running hardware leak behavior, exhaustive third-party source/CVE analysis, and every background-worker artifact-publication race. The broader artifact-generation model, cold/unprepared-cue policy and remote-protocol command consistency remain design decisions.

## Project-lead integration

- Preserved inherited development state separately: 8046e23c5946859ac24045971f50aea9abb610d8.
- APNG safety fix: fe62eaf5fb3a2897eb457069316ce3a5b5647585.
- Remaining audited fixes: d7e2d8c418261793ea4e4286dd37a83d17143ff0.
- [Action log](2026-11-15-actions.md), [ownership](../PROJECT_OWNERSHIP.md), and [Wayfinder map](../../.scratch/design-review/map.md).
- The unrelated Archive.zip remains outside these commits. The playback Pi was only inspected read-only for owner-authorised TimerPi coordination.
