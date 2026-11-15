# How do recurring schedules behave across clock and restart boundaries?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 03, 05
Area: ctp / routes and ws

## Question

Choose timezone ownership, DST gaps/repeated hours, clock steps, restart deduplication, schedule edits near firing, midnight lookahead, late-start recovery and retry policy. Define whether the once-per-day result persists and how countdown, arming and firing resolve the same occurrence.

## Starting recommendation

Resolve occurrences in one calendar-aware function with stable occurrence identity, and make restart/late-fire behavior explicit. Preserve current whole-second scheduling until a requirement changes it; do not silently replay missed cues.

## Evidence to inspect

ctp/ctp_schedule.go; routes/scheduler.go; DESIGN.md §6.8b. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
