# Plan: sling refuses to control-route a bead without a supported gc.kind (ga-u64i)

> **Status:** ready-to-execute — 2026-06-05
> **Bead:** `ga-u64i` (task, P2) — *sling hygiene: stamp/guard a valid
> `gc.kind` on any bead `gc sling` routes to the control-dispatcher.*
> **Parent:** `ga-3p3o` (P1) — its **(A) trigger-reduction** half. The
> **(B) reliability** half already shipped (the serve loop now PARKS
> empty/unknown-kind beads via `dispatch.IsParkableControlError` instead
> of crash-looping into quarantine), so this is **no longer a crash
> risk** — pure trigger reduction.
> **Investigation by:** `voxist.executor` (during `ga-3p3o` execution) +
> confirmed here. Refs `po-de0tu`.
> **One bead per plan.**

## Context

`cliBeadRouter.Route` (`cmd/gc/cmd_sling.go:611`) is the generic
single-bead routing boundary. It sets `gc.routed_to` with **no `gc.kind`
guard**:

```
628   routedTo := req.Target
629   if r.deps.Cfg != nil {
630       routedTo = agentutil.NormalizePoolRouteTarget(r.deps.Cfg, req.Target)
631   }
632   if err := r.deps.Store.SetMetadata(req.BeadID, "gc.routed_to", routedTo); err != nil {
```

Any automation / auto-task that slings a **kind-less** bead to the
control-dispatcher target (`config.ControlDispatcherAgentName =
"control-dispatcher"`, `internal/config/config.go:37`) lands it
routed-but-kindless. The dispatcher then can't categorize it — which used
to crash the singleton (now parked, post-(B)), but should never have been
routable in the first place.

### Red herring (do NOT "fix" this)

The parent plan's lead — `internal/sling/sling.go:1474`
(`privatizeAttachedRootOnlyWisp` → `mapsCloneWithout(root.Metadata,
"gc.kind")`) — is **not** the control path (executor-verified). It strips
`gc.kind` only for a RootOnly wisp **attached** to a source bead, sets
`root.Type="molecule"`, and routes to the **execution** target
(`ApplyGraphRouting` at `sling.go:1236` runs before the strip at 1240).
`IsControlDispatcherKind("wisp")` is false, so the workflow decorator
(`cmd/gc/cmd_sling.go:1198`) never assigns a control route to a wisp step.
Result: an execution molecule, not a control-dispatcher bead. Do not
re-stamp a kind at 1474.

### Fix — guard the route (sound), do NOT stamp an arbitrary kind

**Soundness constraint (from the executor):** do NOT slap an arbitrary
control kind (e.g. `scope-check`) on a kind-less bead — `ProcessControl`
would then mis-handle it as that kind. The fix must either stamp the
**correct** kind at the intent-site, or **guard the route**.

This plan **guards the route** — the general, caller-agnostic, sound fix:
in `cliBeadRouter.Route`, after computing the normalized `routedTo` and
before writing `gc.routed_to`, if the target is the control-dispatcher,
load the bead and **refuse** (clear error) unless its `gc.kind` is a
supported control kind (`graphroute.IsControlDispatcherKind`, the 8 kinds
`check, drain, fanout, retry-eval, scope-check, workflow-finalize, retry,
ralph` — `internal/graphroute/graphroute.go:60`).

Why guard, not stamp: the routing boundary does **not** know the bead's
intent, so it cannot pick the correct kind — but it can soundly refuse.
The refusal error names the offending caller's bead, pointing whoever
slings kind-less control work at the real fix (stamp the kind at the
creation site). This catches **every** auto-task source at once, rather
than chasing one production creator.

Why this won't break legitimate control-routing: real control beads
(`check`/`drain`/`fanout`/…) are routed by the **workflow decorator**
path, which sets `step.Metadata["gc.routed_to"]` directly
(`cmd/gc/cmd_sling.go:1181`) and always has the step's kind — it does
**not** go through `cliBeadRouter.Route`. Confirmed: `ga-3p3o`'s (B)
branch did not touch `cmd_sling.go`, so (A) and (B) are independent.

### Base branch

`feat/beads-proxied-pooling` (per the bead metadata; same as `ga-3p3o`;
on the `fork` remote `cstar/gascity`). Independent of (B) — different
file, no dependency on `IsParkableControlError`.

**Worktree prepared by the planner:**
- `work_dir`: `/Users/cstar/rigs/.gc-worktrees/gascity-ga-u64i`
- branch: `gc/ga-u64i` (based at `feat/beads-proxied-pooling` tip `e0c9bec81`)

## Micro-tasks

TDD, red→green. First task is the failing test. Run from the worktree
root (Go 1.26.3).

| id | description | acceptance (single failing test → make it pass) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add failing test `TestRouteRefusesKindlessControlBead` in `cmd/gc/cmd_sling_test.go`: with a fake `slingDeps` store holding a bead with **no** `gc.kind`, `cliBeadRouter{deps}.Route(ctx, sling.RouteRequest{BeadID:"b1", Target: config.ControlDispatcherAgentName})` must return a non-nil error and leave `gc.routed_to` **unset**. Also assert: a bead with `gc.kind="check"` routed to the dispatcher **succeeds** (route written); a kind-less bead routed to a **non-dispatcher** target still **succeeds** (guard scoped to the dispatcher only). | `go test ./cmd/gc/ -run TestRouteRefusesKindlessControlBead` **fails** — current `Route` writes `gc.routed_to` unconditionally, so the refusal + unset assertions fail. | 5 | — |
| T-002 | In `cliBeadRouter.Route` (`cmd/gc/cmd_sling.go`), after the normalized `routedTo` (l.631) and before `SetMetadata` (l.632): if `routedTo == config.ControlDispatcherAgentName`, `r.deps.Store.Get(req.BeadID)` and, when `!graphroute.IsControlDispatcherKind(strings.TrimSpace(bead.Metadata["gc.kind"]))`, return a clear error (e.g. `refusing to control-route %s: gc.kind %q is not a supported control kind`). Add the `internal/graphroute` import. | `go test ./cmd/gc/ -run TestRouteRefusesKindlessControlBead -count=1` **passes**; `go test ./cmd/gc/ ./internal/sling/ -count=1` green; `go vet ./...` clean. | 5 | — |

Total est: ~10 min (1 red/green pair, 2 files: `cmd_sling.go` + `cmd_sling_test.go`).

## GDPR data-flow impact

**No impact.** This change adds a metadata guard on the orchestration
routing boundary: before writing `gc.routed_to`, it validates the bead's
`gc.kind` when the target is the control-dispatcher. No personal data is
read, written, transmitted, or logged. Beads carry operational
identifiers (`gc.kind`, `gc.routed_to`, bead IDs), not data-subject data.
The refusal error names a bead ID — an operational identifier. No new
persistence, network egress, or log fields. Article 30 record of
processing unaffected.

## MDR Class I traceability

**No-op outside voxmemo.** This change is in `gascity` (the `gc`
orchestration / sling routing layer), not the voxmemo→voxist-api clinical
documentation pipeline. It does not touch the chain-of-evidence from
microphone capture through ASR to exported clinical note. The heading is
retained per Voxist planner discipline so an auditor sees the explicit
consideration.

## Validation gates

- `go test ./cmd/gc/ ./internal/sling/ -count=1` green; `go vet ./...`
  clean on `feat/beads-proxied-pooling`.
- The new test proves the **invariant**: a bead `cliBeadRouter.Route`
  sends to `ControlDispatcherAgentName` always ends with a
  `IsControlDispatcherKind`-true `gc.kind`, else the route is refused and
  `gc.routed_to` is left unset.
- Existing sling + dispatch suites stay green (the workflow-decorator
  control path at `cmd_sling.go:1181` is untouched and unaffected).
- `git diff` confined to `cmd/gc/cmd_sling.go` + `cmd/gc/cmd_sling_test.go`.
- No new third-party Go modules; no new env vars.

## Notes for the executor

- **Target comparison.** `ControlDispatcherAgentName` is the literal
  `"control-dispatcher"`; the dispatcher is a singleton (not a pool), so
  `NormalizePoolRouteTarget` should leave it unchanged and
  `routedTo == config.ControlDispatcherAgentName` is the right test. Pin
  this in T-001 — if normalization or qualification alters it, compare on
  the resolved identity instead.
- **Do NOT stamp a kind.** The guard refuses; it must not invent a kind
  (soundness — `ProcessControl` would mis-handle a wrong kind).
- **Reuse the canonical predicate.** Use
  `graphroute.IsControlDispatcherKind`, not a re-listed kind set, so the
  guard and the dispatcher agree on the supported kinds.
- **Store.Get cost.** The extra `Get` only runs when routing to the
  dispatcher (rare), so it adds no cost to normal agent routing.

## Open questions

- `[architect]` **Refuse (hard error) vs warn-and-drop-route.** This plan
  hard-refuses a kind-less control route. Since (B) already makes a
  stray kind-less bead non-fatal (parked), a hard refusal at sling time is
  safe and surfaces the buggy caller loudly. Confirm hard-refuse is
  preferred over a softer warn-and-skip during rollout.
- `[architect]` **Base/remote.** PR base is
  `fork/feat/beads-proxied-pooling` (fork remote), as with `ga-3p3o`.
  Confirm. Not a blocker.
- If a *legitimate* production caller is found that routes to the
  dispatcher and stamps the kind only afterward, the correct fix is to
  stamp the kind **before** routing at that caller; the guard's error will
  surface it. (Executor-resolvable via the failing-caller error.)

## Out of scope

- `internal/sling/sling.go:1474` (the wisp strip — red herring, leave it).
- Re-stamping or back-filling kinds on already-routed kind-less beads.
- The (B) serve-loop park path (already shipped under `ga-3p3o`).
- `origin/main` — targets `feat/beads-proxied-pooling` only.

## Status

**Shipped — both micro-tasks green.**

- [x] T-001 — failing test `TestRouteRefusesKindlessControlBead` in `cmd/gc/cmd_sling_test.go` (kind-less → dispatcher currently succeeds)   ✅ red at `65de98eba`
- [x] T-002 — route guard in `cliBeadRouter.Route`: refuse a control-dispatcher route unless `graphroute.IsControlDispatcherKind(gc.kind)`; `gc.routed_to` left unset on refusal   ✅ green at `65de98eba`

Gates: `go test ./internal/sling/` green; `cmd/gc` `-run 'Sling|Route|Dispatch'` blast-radius green; `go vet ./cmd/gc/ ./internal/sling/` clean. Diff confined to `cmd/gc/cmd_sling.go` + `cmd/gc/cmd_sling_test.go`.

**Open-question resolutions (executor):**
- *Refuse vs warn-and-drop:* took the plan's **hard-refuse** — (B) already makes a stray kind-less bead non-fatal, so a loud refusal at sling time is safe and surfaces the buggy caller. No legitimate caller routes kind-less control work through this boundary (the workflow decorator at `cmd_sling.go:1181` is the real control-routing path and is untouched).
- *Base/remote:* PR targets `fork/feat/beads-proxied-pooling`, as with `ga-3p3o`. The bd-init bootstrap commit is rebased out so the diff is scoped to the fix.
- Test note: `beads.NewMemStore()` assigns its own IDs, so the test captures the real ID from `Create` rather than asserting an input ID (the `slingTestStore` synthetic-fabrication path only handles dash-shaped IDs).

### Reviewer round 1 — rig-scoped dispatcher bypass (CHANGES_REQUESTED → addressed)

Reviewer (PR #3) found a MEDIUM correctness gap, empirically reproduced: the
bare-name guard `routedTo == config.ControlDispatcherAgentName` matched only the
literal `"control-dispatcher"`, so a kind-less bead routed to the **rig-scoped**
form `<rig>/control-dispatcher` (produced by `controlDispatcherTargetForExecutionTarget`,
injected per-rig at `config.go:4135`) **bypassed** the guard — `Route(kindless →
gascity/control-dispatcher)` returned `err=nil` and wrote `gc.routed_to`.

- [x] T-003 — extend `TestRouteRefusesKindlessControlBead` with the rig-scoped repro: kind-less → `gascity/control-dispatcher` must refuse + leave `gc.routed_to` unset, plus a non-regression case (valid kind still routes rig-scoped)   ✅ red at `34bad69ae` (case 4 failed: `err = nil, want refusal`)
- [x] T-004 — make the guard suffix-aware via new `isControlDispatcherRouteTarget` helper, mirroring the serve-loop predicate `isWorkflowServeControlDispatcherAgent` (`dispatch_runtime.go:659`): `==bare || HasSuffix("/"+bare)`   ✅ green at `34bad69ae`

Gates (round 1): `go test ./cmd/gc/ -run TestRouteRefusesKindlessControlBead` green; `cmd/gc -run 'Sling|Route|Dispatch'` blast-radius green; `internal/sling` green; `go vet ./cmd/gc/ ./internal/sling/` clean. Diff stays confined to `cmd_sling.go` + `cmd_sling_test.go` (per the validation gate); no refactor of the two sibling copies of the predicate (kept the change minimal per the reviewer's "mirror" prescription).
