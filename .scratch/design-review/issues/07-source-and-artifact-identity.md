# When is media analysis or a prepared artifact still valid?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: none
Area: media and worker / ctp / gsp

## Question

Define immutable source identity and the conditions for publishing worker results, prepared media and warm pipelines after replacement, rename, deletion, display/profile change or a concurrent encode. Which source/target/tool versions identify an artifact, and how are stale or failed outputs exposed and reclaimed?

## Starting recommendation

Tie publication and cache validity to the source generation and relevant target profile. A late result for an older source must never mark the replacement ready. Keep source media, prepared artifacts and armed playback distinct.

## Evidence to inspect

media/prepare.go; worker/thumbnails.go; ctp/ctp_media.go; gsp/gsp_warm.go; DESIGN.md §12.17. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
