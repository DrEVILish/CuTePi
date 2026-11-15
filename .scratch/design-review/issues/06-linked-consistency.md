# What do linked units guarantee during failure and recovery?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 03, 05
Area: routes and ws / ctp

## Question

Separate synced playback with independent shows from backup mode with one authoritative show. Decide election refusal, partition behavior, uncertain command retries, replay/deduplication, state-versus-transport recovery, lost-primary commands, page capability scope/expiry/revocation and clock-quality requirements.

## Starting recommendation

Keep backup show mutations and transport under a known primary, expose unknown command outcomes, and distinguish eventual show-state convergence from live transport delivery. Revoke peer-bound page grants on unlink. Preserve arbitrary HTTP Host names and Origin/Referer protection.

## Evidence to inspect

DESIGN.md §6.8d; routes/link.go; routes/linked.go; routes/linkmedia.go; public/src/link.js; ws/hub.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
