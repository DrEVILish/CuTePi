# How are competing control commands ordered and cancelled?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: 04
Area: routes and ws / gsp

## Question

When HTTP, browser GO, OSC, HyperDeck, scheduler timers, auto-continue and linked replay compete, which command owns the next output? Define ordering, cancellation generations, duplicate-command handling and accepted/applied/failed/unknown outcomes, with protocol-specific feedback.

## Starting recommendation

Use one authority/admission contract for every ingress and preserve explicit emergency priority. Keep playback work outside request handlers while retaining command identity and cancellation. Avoid claiming exactly-once physical output after uncertain network failure.

## Evidence to inspect

routes/remote.go; routes/qlab.go; routes/hyperdeck.go; routes/scheduler.go; routes/link.go; gsp/startat.go. Consult the matching ownership thread for its read-only initial assessment; source line numbers can move during audit fixes.

## Comments

2026-11-15 — Charted from owner-confirmed scope and area assessments. This is an open decision, not an approved change or a resolved implementation ticket.
