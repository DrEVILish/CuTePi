# CuTePi architecture improvements — 2026-11-15

Scope: recently changed show export, preparation jobs and armed cues. The owner requested the architecture review and implementation with the team under ub-mode. Project work runs in CT110; hardware and unresolved product choices are outside these changes.

## Selected improvements

| Module | Problem | Change |
|---|---|---|
| Show export | Archive callers assemble independent reads of cues, groups and selection; malformed saved selection is silently accepted. | One consistent show snapshot behind the export interface; keep archive streaming in the route adapter. |
| Preparation queue | Batch cancellation can overwrite a cancelled result and strand retries queued during draining. | The queue owns its lifetime and each running job owns its cancellation. |
| Armed cue | Pipeline, cue options and source identity have separate ownership; matching and transfer rules repeat. | A private slot owns all reusable-cue state and its matching/transfer rules. |

Each change adds depth by concentrating existing rules behind a small interface. The deletion test places those rules back across callers if the module is removed. Locality improves without adding a hypothetical adapter or a new resource policy.

The [glossary](../../GLOSSARY.md) records the existing domain concepts. No ADR was needed for these reversible changes. The temporary visual report is /tmp/architecture-review-20261115-005457.html inside CT110 and was displayed in the project thread.

## Deliberately deferred

- A generic source-snapshot package for two short predicates would add little leverage.
- Warm and WarmNext have different generation and preroll behavior; this change does not merge their build flows.
- Cold/unprepared GO behavior, command authority and physical output acceptance remain open in the [design review](../../.scratch/design-review/map.md). No product decision ticket is resolved by these refactors.
- No new decoder revision is imported and no Pi performance claim is made.

## Validation and action log

- The malformed-selection regression failed before the fix. Export snapshot, append/overwrite and existing HTTP archive round-trips pass.
- Channel-controlled preparation regressions reproduced both cancellation bugs for late success, cancellation and skipped results; they pass after the fix. Tests verify duplicate retry suppression, fresh contexts and no overlapping runners.
- Armed-cue characterization passed before and after the refactor, including stale-source rejection in both slots, preserved active playback, ownership retained for cleanup and promotion readiness.
- Integration: go build, go vet ./..., and go test ./... -count=1 pass.
- Race detector: ctp, gsp, routes and media pass.
- Independent integration review found no actionable issues. Export snapshot consistency was checked structurally; no concurrent-mutation test hook was added.
- Formatting, diff whitespace, local documentation links and removal of retired warm-state symbols were checked.
- Crapper, dryer and mutator were unavailable in the tool catalogue and guest PATH. No complexity, duplication or mutation scores are claimed.
- No hardware deployment, physical latency/frame-loss measurement, Pi changes or decoder benchmark was performed.

Source commits: 34034ef (show snapshot), 4cc9cc6 (preparation cancellation), f99a500 (armed-cue ownership).

[Action log](2026-11-15-architecture-actions.md). Full command output remains on CT110 under /var/lib/cutepi-work/architecture-20261115/.
