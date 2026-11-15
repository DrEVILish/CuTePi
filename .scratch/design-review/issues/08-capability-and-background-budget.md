# What is playable, supported and allowed during a show?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 01, 07
Area: media and worker / gsp / routes and ws

## Question

Define decode acceptance, measured-supported output, unknown capability, prepared availability and runtime readiness for file uploads, URL imports and live pages on Pi4/Pi5. Decide permissible analysis/import/encoding work while playback runs and behavior at decoder, plane, memory and I/O limits. Preserve codecs/features rather than silently reducing quality.

## Starting recommendation

Report separate capability states backed by hardware/path/profile evidence, bounded background work and explicit resource admission. Keep existing upload versus URL policy until the human chooses whether to unify it. Do not equate an estimate or missing warning with measured support.

## Evidence to inspect

DESIGN.md §2, §5.7, §12.15, §12.17; routes/supported.go; media/estimate.go; media/media.go; media/prepare.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
