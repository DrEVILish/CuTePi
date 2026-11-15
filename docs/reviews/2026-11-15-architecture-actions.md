# Architecture work action log — 2026-11-15

All project operations ran in CT110 at /workspace/CuTePi. Source commits and validation are described in the [review](2026-11-15-architecture.md).

## Coordinator

- 2026-11-15T00:53:36.086343 Started architecture review at 32b0be1; read invoked architecture/ub-mode/codebase-design/domain-modeling/grilling/ponytail skills, current AGENTS and recent history; queried Graphify then inspected current sources because graph locations can be stale. No glossary or ADR directory exists. Assigned three read-only fresh-context reviews for playback, show archives and media preparation. Tools crapper/dryer/mutator unavailable in catalogue and PATH; no substitute metrics claimed. Repository has unrelated Archive.zip, leave untouched. All project work CT110; no hardware access.
- 2026-11-15T00:56:00.287849 Presented temporary architecture HTML inline after preview (CT110 /tmp/architecture-review-20261115-005457.html); user already requested implementation, so selected three bounded recommendations without a redundant approval round. Assigned nonoverlapping source ownership: show export snapshot, preparation lifecycle, armed-cue slots. Added domain glossary describing existing concepts; no ADR needed for reversible refactors. Deferred new generic source-snapshot package and divergent Warm/WarmNext build unification because leverage is low/risk unnecessary. No Wayfinder product tickets resolved.
- Created concise durable architecture review with selected modules, deletion-test rationale and explicit deferred work; validation section pending team completion. Read full caller lists to check export compatibility; no additional production callers beyond archive route. Checked CT Go/runtime/storage and worktree inventory; only main exists, 18GiB free, nothing to reclaim safely. Sent reviewer guardrails for transaction lifetime and slot resource ownership.
- Reviewed complete source/test diff: export readers use real DB/Tx seam and finish before media streaming; queue cancellation preserves terminal results and serial drain; warm-slot source provenance moves with complete slot and no retired global symbols remain. Validated new local Markdown links. Started build/vet/full test/targeted race integration checks with separate logs. Independent read-only integration review assigned after all source edits stopped.
- Integration complete: build 6.0s, vet 1.1s, full go test ./... -count=1 56.2s, race ctp/gsp/routes 52.1s all pass; separate media race 10.480s passes. Independent integration reviewer found no actionable issues; concurrency snapshot proof is structural code inspection, no synthetic concurrent-mutation hook added. gofmt and staged whitespace checks passed. Created three ordered source commits: 34034ef, 4cc9cc6, f99a500. No physical-output/latency tests or test-Pi access.
- Recorded completed validation and remaining limits in architecture review. Consolidated coordinator and area action logs for repository review; preparing final documentation commit and normal main push. Unrelated Archive.zip remains untouched; no obsolete worktrees to remove.

## Show export

- Added malformed-selection behavioral regression before implementation. Initial heredoc quoting attempt failed without editing files; corrected shell quoting.
- Observed expected regression failure: ExportCues logged malformed selection but returned nil error. Added error return at selection boundary.
- Added ExportedShow snapshot transaction, shared real DB/Tx readers, legacy adapters, and one route call. Adapted merged-order append/overwrite round-trip to snapshot API; both export callers reject malformed selection.
- Targeted snapshot/legacy/import tests passed under 60-second timeout (show-green-ctp.log), exercising successful transaction release and error rollback on single DB connection.
- Existing HTTP TestShowExportImportRoundtrip covers manifest, append/overwrite, media embedding and re-export; reused instead of adding duplicate route test.
- Broader ctp and show-specific routes tests attempted in parallel; both blocked during compilation by concurrent media/prepare.go:220 undefined ctx. No media files changed by this agent; parent notified.
- Reviewed owned diff: every ExportShow read uses tx, deferred rollback covers errors, commit precedes return/ZIP headers, and compatibility readers share unchanged SQL/field conversion. No schedule/order/import/media streaming semantics changed.
- git diff --check passed for owned tracked files; gofmt applied also to new regression file. crapper, dryer and mutator are unavailable on PATH, so no results claimed. Parent will run broad verification after other agents finish.
- Owned final files: ctp/ctp.go, ctp/export.go, ctp/export_snapshot_test.go, ctp/import_transaction_test.go, routes/show.go. No staged files, commits or pushes by this agent.

## Preparation

- Read ub-mode and ponytail skills; scope is Preparer implementation and tests only, preserving HTTP adapter and serial encoding. Read-only caller inspection completed previously: routes/prepare.go owns the sole production Preparer.
- Added channel-controlled cancellation/drain/retry regression; assertions cover late runner results, queued cancellation, duplicate retry suppression, fresh context and no overlap. Initial edit command failed shell quoting before any project write; corrected quoting.
- Regression failed as expected in all three return cases: retry stranded and cancellation result overwritten (media-red.log). Move cancellation ownership from worker lifetime to each job; retain single worker and terminal cancellation result.
- First implementation check exposed an incomplete textual edit (undefined ctx), corrected the local loop block before rerunning tests.
- Targeted queue tests and real subprocess preparation regressions pass after fix (media-green.log). Running full media package under race detector for broader concurrency and adapter coverage.
- Full media race run passes (media-race.log; 10.480s). Reviewed owned-file diff and git diff --check passes. Only media/prepare.go and media/prepare_test.go edited; no stage/commit/push. crapper/dryer/mutator unavailable in session, so no fabricated tool results. Existing real PrepareVideo tests exercised subprocess lifecycle and artifacts; no adapter or resource policy changes.

## Playback

- Playback: inspect warm/spare ownership and existing tests; add behavior characterization before refactoring.
- Playback: characterization baseline passes (selected/spare stale source, current playback preserved, promotion retains readiness); replace parallel state with owned warmSlot and remove provenance registry.
- Playback: warmSlot refactor and targeted gsp checks pass; stale refusal retains slot ownership for deferred Panic cleanup; public interface, timing and generation policy unchanged.
