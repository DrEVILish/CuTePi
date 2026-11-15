# What should GO do when a cue is not ready?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: none
Area: gsp / media and worker / public and templates

## Question

When a cue cannot produce its first picture and sound within the agreed GO budget because decoding, live-page loading or resource reservation is incomplete, what must GO do in Show mode and Edit mode? Define the difference between an accepted command, a prepared artifact, an armed cue and verified output. Preserve all source types; do not silently choose blocking, rejection or best-effort fallback for the operator.

## Starting recommendation

Show mode advertises cue-specific Preparing/Ready/Unavailable state and preserves the current output on a non-ready GO; Edit mode allows explicit best-effort playback. This is a proposal requiring the user's decision, not implemented behavior.

## Evidence to inspect

DESIGN.md §6.1 and §12.17; gsp/gsp.go LoadWithOpts/startPlayback; routes/livestandby.go; templates/mediainfo.html. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
