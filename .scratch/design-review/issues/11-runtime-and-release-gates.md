# What makes a build a qualified and recoverable CuTePi release?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 02, 06, 08, 10
Area: deploy and tools / all area owners

## Question

Define Pi4/Pi5 runtime profiles, binary/assets/theme/dependency identity, effective service environment, readiness and rollback, unit/race/native/hardware verification responsibilities, and show-duration soak workloads and thresholds for memory/FD/process growth, CMA, audio xruns, frame tails and restart behavior.

## Starting recommendation

Publish reproducible artifacts and measured per-hardware qualification. Require actual picture/audio evidence and resource gates, not only build success, ALSA RUNNING or presenter counters. Preserve CT110 isolation, real Pi service use and all AGENTS operational restrictions.

## Evidence to inspect

AGENTS.md; cutepi.service; deploy/install-service-user.sh; .github/workflows/test.yml; tools/codec-corpus/support.py; TEST_REPORT.md. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

[Codec evidence](../../../docs/CODEC_EVIDENCE.md): pin imported decoder/SDK revisions and runtime selection. The reference review did not rerun hardware tests; qualify the resulting CuTePi artifact independently, with nonoverlapping workloads and no additional test-Pi tooling.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.

2026-11-15 — [Pi hardware evidence](../../../docs/reviews/2026-11-15-hardware.md): rapid-refire testing exposed an audio input teardown warning and persistent BCM2835/MMAL decoder failure on experimental builds. Audio candidates were retained off main after failed hardware qualification. Application restarts did not clear the decoder; a normal stock reboot did. Include decoder failure/recovery and live-page startup/cache behavior in qualification. A short recovered run does not establish a show-duration release gate.
