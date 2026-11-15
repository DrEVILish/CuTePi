# What proves first output and frame-perfect playback?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 01
Area: gsp / deploy and tools

## Question

Precisely define GO time origin, first visible picture/first audible sample, fades and intentional silence, <50ms bound versus percentile, scheduled/linked start skew, frame mapping for lower-rate sources, zero missing opening/loop frames for the AV Sync Test, A/V offset and permitted drift. What independent measurement and workload makes a release pass or fail?

## Starting recommendation

Use one original command timestamp plus presentation timestamps, independent scanout and audio capture, and a published scenario matrix. Keep the <50ms target and zero-loss AV Sync Test requirement; no numerical tolerance or percentile is silently relaxed.

## Evidence to inspect

DESIGN.md §2, §6.1.4, §6.8c, §12.11; tools/codec-corpus/support.py; gsp/startat.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

[Codec evidence](../../../docs/CODEC_EVIDENCE.md): retain the decoder pixel checks, but qualify actual scanout/audio separately. Include buffering effects on first output, frame-count verification and an exclusive measurement slot; discard-sink throughput does not prove the target.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
