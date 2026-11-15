# CuTePi design and specification review

Labels: wayfinder:map
Status: open

## Destination

An agreed replacement design/specification and prioritised delivery plan for CuTePi's existing Pi4/Pi5 playback, live pages, layering, scheduling and linked-unit features. Completion means the remaining behavior, ownership and acceptance decisions are settled, not merely that documentation has been rewritten.

## Notes

- Owner confirmed destination, preserved feature scope and GO-to-first-picture/first-sound latency interpretation on 2026-11-15.
- Wayfinder is planning; confirmed bug remediation is separately authorised and runs in parallel under ponytail-audit then ub-mode. Resolving this map does not silently authorise new product behavior.
- Consult wayfinder, grilling and domain-modeling for design sessions. Human decisions require the owner's live answers; never invent their side of the conversation. Resolve at most one ticket per session except research tickets.
- Local Markdown tracker: one ticket per file under issues/. Status open = unclaimed; claim by setting Assignee and Status claimed before working it; resolve by recording an Answer and setting Status resolved. Blocked-by dependencies must all be resolved before a ticket enters the frontier.
- [Area ownership and thread links](../../docs/PROJECT_OWNERSHIP.md). The coordinator integrates cross-area changes; area threads initially assess read-only.
- This map and ticket filenames live in .scratch/design-review/ per the local tracker convention. No external tracker has been configured for this workflow; /setup-matt-pocock-skills can configure one later.
- [Codec evidence index](../../docs/CODEC_EVIDENCE.md) links the owner-supplied reference thread, final corrections and integration/measurement limits. Decoder throughput and pixel checks do not establish physical output acceptance.
- Source evidence: DESIGN.md, README.md, PERFORMANCE_PROPOSALS.md, TEST_REPORT.md and current code. Stale Graphify source locations must be checked against current source. AGENTS.md and the owner's environment constraints govern operations.
- All project work inside CT110. Downloads and caches on the guest's fourTB-backed root. Test hardware remains a separate real service, with no browser/Node installations.
- <50ms concerns first visible picture and first audible sound, not HTTP acknowledgement. The timing origin, cold/unprepared behavior and pass/fail definition still need decisions. Zero missing AV Sync Test frames remains a requirement.
- [Shared Pi allocation](../../docs/SHARED_PI_ALLOCATION.md): TimerPi owns shared network configuration; CuTePi retains playback/deploy/DRM; a disruptive slot and second Pi are not allocated.
- Query open child files and their Blocked by lines to find the frontier; open tickets are not duplicated in this map.

## Decisions so far

No decision ticket has been resolved. Initial scope agreements are recorded in Notes; ticket-specific outcomes will be indexed here after live review.

## Not yet specified

- Exact capacity limits and supported simultaneous-source combinations depend on the readiness, media and output contracts and target-specific measurement.
- How preparation and cue admission interact with operator editing, network loss and long-running resource recovery may expose further questions.
- A precise implementation sequence and migration/rollback plan depend on the show-model, command and release decisions.
- Any Pi5-specific limits need evidence; Pi4 measurements are not Pi5 qualification.

## Out of scope

- Feature removal or a wholesale rewrite without a separate explicit decision.
- Treating architecture proposals or successful unit tests as physical latency/frame-loss proof.
- Replacing the operator's product choices with agent assumptions.
- PVE-host project builds/cache work, extra test-Pi applications, or changes to unrelated dev-server projects.
