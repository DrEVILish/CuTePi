# What should the operator see and control at every transition?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 01, 03, 05, 08
Area: public and templates / routes and ws

## Question

Define selected target versus running cues, group direct/subtree counts and GO action for plain/slideshow/Awards groups, Show/Edit protections, pending/ready/unavailable feedback, connection health versus command outcome, and which inspector edits apply live versus on next firing. Resolve live-page URL reload semantics.

## Starting recommendation

Use domain terms and backend-authoritative states. Show the actual next GO action and distinguish saved-for-next-fire from applied-live feedback. Keep implementation internals out of operator decisions.

## Evidence to inspect

DESIGN.md §5.4, §5.5, §5.10; templates/cuesheet.html; templates/cueinspector.html; templates/mediainfo.html; routes/groups.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
