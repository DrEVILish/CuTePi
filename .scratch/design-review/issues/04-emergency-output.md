# What must Panic and Stop guarantee on physical output?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: none
Area: gsp / routes and ws

## Question

Define Panic, Stop and ESC/fade outcomes across focus, voices, outgoing fades, soundtrack, live pages, warm/spare slots, scheduled starts and linked units. What is the physical output deadline, and what resource-release work may complete afterwards? Preserve the configured panic holding image or black fallback.

## Starting recommendation

Panic gates every audio producer, switches to configured holding image or black and cancels pending starts before cleanup. Keep physical output completion separate from bounded resource teardown. Retain the existing ESC fade gesture as a distinct action.

## Evidence to inspect

gsp/gsp.go Panic/Stop; gsp/panichold.go; gsp/background.go; gsp/wall.go retireOutgoing; DESIGN.md §6.9. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
