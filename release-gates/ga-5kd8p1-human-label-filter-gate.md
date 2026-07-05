# Release Gate: ga-5kd8p1 human-label dispatch filter

Date: 2026-07-05

Branch: `fix/ga-5kd8p1-human-label-filter-clean`
Head before gate commit: `a96f90c03291b4adb5d5c45088922b73ff3fcf59`
Base: `origin/main` at `d9225156f9b30fa4fe7fee28c30699ee2cc8cc3d`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in this
checkout. This gate uses the active deployer release criteria plus the local
testing guidance in `TESTING.md`.

## Summary

This change prevents Gas City dispatch readiness queries from selecting beads
that bd has flagged for human attention with the native `human` label. The
fix applies to the ready/unassigned query construction paths used by configured
pool demand, migration demand, legacy ephemeral pool demand, and control
dispatcher routed readiness.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-8k1975` is closed with close reason `pass` and notes state `REVIEW VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | The branch adds `--exclude-label human` to native `bd ready` ready/unassigned queries and the equivalent jq label exclusion to the legacy `bd query` path. Regression coverage exercises the config work-query paths and `workflowServeControlReadyQueryForBeads`. |
| 3 | Tests pass | PASS | Focused tests and broad fast gate passed on this branch. See test evidence below. |
| 4 | No high-severity review findings open | PASS | Review notes contain no blocking HIGH finding. The only follow-up called out by review is `ga-pcrcfb` at P3 and explicitly non-blocking. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; final cleanliness is verified after committing the gate file before PR creation. |
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...HEAD` returned `0 1` before the gate commit, so the feature branch is directly ahead of current `origin/main`. |
| 7 | Single feature theme | PASS | The diff is one dispatch-query filtering theme across `cmd/gc` and `internal/config`, with matching tests. No independent feature surface is bundled. |

## Scope Evidence

Changed files before gate commit:

- `cmd/gc/cmd_convoy_dispatch_test.go`
- `cmd/gc/dispatch_runtime.go`
- `internal/config/config.go`
- `internal/config/config_test.go`

Diff summary before gate commit:

```text
cmd/gc/cmd_convoy_dispatch_test.go | 70 ++++++++++++++++++++-------
cmd/gc/dispatch_runtime.go         |  4 +-
internal/config/config.go          |  7 +--
internal/config/config_test.go     | 99 +++++++++++++++++++++++++++++++++++---
4 files changed, 152 insertions(+), 28 deletions(-)
```

## Test Evidence

- PASS: `go test ./internal/config/...`
  - `ok github.com/gastownhall/gascity/internal/config 3.003s`
- PASS: `GC_FAST_UNIT=1 go test ./cmd/gc/... -run TestWorkflowServeControlReadyQuery`
  - `ok github.com/gastownhall/gascity/cmd/gc 0.826s`
- PASS: `go build ./...`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel`
  - `fsys-darwin-compile`: ok
  - `unit-cmd-gc-1-of-6`: ok
  - `unit-cmd-gc-2-of-6`: ok
  - `unit-cmd-gc-3-of-6`: ok
  - `unit-cmd-gc-4-of-6`: ok
  - `unit-cmd-gc-5-of-6`: ok
  - `unit-cmd-gc-6-of-6`: ok
  - `unit-core`: ok
  - Final line: `All fast jobs passed`

## Decision

PASS. Proceed with PR creation and merge-request handoff to mayor. The deployer
must not merge.
