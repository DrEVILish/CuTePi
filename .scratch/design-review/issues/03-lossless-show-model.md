# Which facts and identities make a lossless show?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: none
Area: ctp

## Question

Define cue and group identity separately from optional labels, visual order, membership and runtime focus. Which ordered rows, cross-cue references, typed selection, recurring schedules and explicit zero values must export/overwrite/append preserve, and which credentials/device/runtime facts stay local? Include append ID remapping and legacy archive compatibility.

## Starting recommendation

Store explicit stable identities/references and merged row order in a versioned archive. Preserve existing show behavior and keep credentials/device settings and playback history out unless separately requested. Confirm selection semantics rather than treating numeric positions as identities.

## Evidence to inspect

DESIGN.md §4, §5.8, §6.8 identity guards; ctp/export.go; ctp/sheet.go; ctp/link.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
