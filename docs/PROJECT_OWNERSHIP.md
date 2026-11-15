# CuTePi project ownership

Status: initial ownership established 2026-11-15. This thread coordinates product decisions and integration; area ownership is responsibility for a contract, not exclusive ownership of files shared across boundaries.

## Direction agreed with the owner

- Destination: a replacement design/specification and prioritised delivery plan, with confirmed bugs fixed alongside the review.
- Preserve Pi 4/5 playback, live pages, layering, scheduling and linked units; propose feature-scope changes explicitly.
- The <50 ms target concerns GO to first visible picture and first audible sound. Cold/unprepared behavior and exact acceptance methodology remain open decisions.
- Keep the AV Sync Test's zero missing frames requirement. Software counters alone do not establish physical output.

## Coordination

Leader: [CuTePi](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/02f1a52b-03f4-47ef-b80c-7a20c21b02bb).

| Area thread | Contract and primary paths |
|---|---|
| [gsp — Playback, Audio and Display](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:0) | Playback lifetime, warming, fades, planes and shared audio: gsp/. |
| [ctp — Show Model, Scheduling and Persistence](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:1) | Durable show identity, order, membership, selection, archives and recurrence: ctp/. |
| [media and worker — Import and Preparation](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:2) | Source/analysis/prepared-artifact lifecycle and work queues: media/, worker/. |
| [routes and ws — Control, Security and Linked Units](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:3) | Control ingress, auth/Origin, protocols, linked units and browser synchronization: routes/, ws/. |
| [public and templates — Operator Workflow](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:4) | Operator workflow and feedback: public/, templates/. |
| [deploy and tools — Runtime and Hardware Verification](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:mcp:a6453223-cc4b-4ad6-bded-e076fa5aca34:cutepi-area-ownership-20261115-v1:5) | Runtime profiles, reproducible artifacts, verification and lifecycle wiring: deploy/, tools/, config/, main.go, cutepi.service, CI. |

Cross-area changes are coordinated here before another writer starts. The audit/ub-mode agent is the only source writer during this review; area assessments are read-only. Every project command/edit/build/cache stays inside CT110 at /workspace/CuTePi; AGENTS.md remains authoritative for machine operations.

## Design review

[Wayfinder map](../.scratch/design-review/map.md) is the index of decisions; its child tickets hold the questions and resolutions. No open decision is an approved implementation requirement. The current DESIGN.md/README.md include historical descriptions and measurements; the review must distinguish desired behavior, implemented behavior, measured evidence and proposals.

Prioritise safety/data preservation first, then the readiness/output/command contracts, then linked behavior and operator feedback, then qualified releases. This is an ordering recommendation, not a claim of verified <50 ms performance.

## Codec evidence

[RaspberryPi Codecs evidence index](CODEC_EVIDENCE.md) connects the owner-supplied reference thread to the review. gsp owns decoder integration and playback behavior; media and worker use the capability evidence; deploy and tools own qualification of the actual CuTePi artifact. The codec thread remains reference material and has not been messaged or changed.

## Shared hardware

[Shared Pi allocation](SHARED_PI_ALLOCATION.md) records verified inventory, TimerPi network ownership and current slots. Network changes are coordinated between project leads; no independent mesh controller runs on shared hardware.
