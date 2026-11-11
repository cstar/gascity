package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file cuts the gc->bd read-storm documented on ga-ak6rt1: the
// control-dispatcher's per-tick readiness scan (workflowServeControlReadyQueryForBeads,
// dispatch_runtime.go) builds a shell script that fork-execs up to ~9
// bd/jq processes per agent per tick. Wire that same readiness evaluation to
// answer from exactly one batched read per sweep -- one `bd ready --json` call
// in the scope directory, plus an in-process Ready on the graph binding when
// the city split it out -- instead of the shell script's N separate `bd`
// invocations.
//
// One batched read per sweep is the whole cost (ga-yhp). An in-process
// CachingStore snapshot used to sit in front of it, primed on every sweep and
// reused for three seconds. That prime cost six bd processes (bd version,
// two bd list, two ephemeral bd query, bd sql) plus a full city-config load
// (~0.5 s CPU), and on the live city it never answered: every sweep that primed
// it still took the batched read. So the snapshot, and the config load it alone
// needed, are gone; nothing here is cached across sweeps.
//
// Why this hooks into nextWorkflowServeBeads (the default workflowServeList
// implementation) rather than drainWorkflowServeWork: workflowServeList is a
// package var every existing serve-loop test overrides wholesale to fake the
// ready queue, so changing drainWorkflowServeWork's call site to bypass it
// for control-dispatcher agents would silently stop exercising ~25 existing
// tests' fakes. nextWorkflowServeBeads is never called directly by any
// existing test (they all replace workflowServeList outright), so extending
// its body here is additive: the exact query-string shape from
// workflowServeControlReadyQueryForBeads is unchanged (still asserted upon by
// TestWorkflowServeControlReadyQuery* tests), and any non-control-ready query
// falls straight through to the original shell exec, unchanged.

// controlReadyQueryMarkerPrefix identifies a workQuery produced by
// workflowServeControlReadyQueryForBeads. That function always writes this
// exact literal prefix (BD_EXPORT_AUTO=false plus a non-empty
// GC_CONTROL_TARGET, dispatch_runtime.go:788); no other work_query shape
// produces it.
const controlReadyQueryMarkerPrefix = "BD_EXPORT_AUTO=false GC_CONTROL_TARGET="

// controlReadyExcludeType mirrors the shell script's --exclude-type=epic.
const controlReadyExcludeType = "epic"

// controlReadyFallbackLimit bounds the single batched bd ready call issued
// on every sweep. It must be generous enough that per-candidate/
// per-route filtering in Go (each capped at workflowServeScanLimit) is never
// starved by an earlier truncation at the bd layer -- unlike the shell script
// this replaces (which ran each candidate/route's own independently-capped bd
// call), this single batched call's cap is shared across every candidate and
// route, so it must hold a whole city's ready set even during write
// bursts. It costs one bd call
// regardless of value, so err on the generous side; controlReadyFallbackReady
// also logs if a response ever comes back exactly at this limit, so silent
// truncation is at least observable.
const controlReadyFallbackLimit = 5000

// parsedControlReadyQuery holds the values workflowServeControlReadyQueryForBeads
// bakes into its generated shell command as env-var prefix assignments.
type parsedControlReadyQuery struct {
	target             string
	controlSessionName string
	legacyTarget       string
	bareTarget         string
	includeEphemeral   bool
}

// parseControlReadyQuery recognizes a workQuery built by
// workflowServeControlReadyQueryForBeads and recovers the values it encoded
// as shell-quoted env-var prefix assignments, using shellquote.Split (the
// same package the query was built with) rather than hand-rolled parsing.
func parseControlReadyQuery(workQuery string) (parsedControlReadyQuery, bool) {
	if !strings.HasPrefix(workQuery, controlReadyQueryMarkerPrefix) {
		return parsedControlReadyQuery{}, false
	}
	parsed := parsedControlReadyQuery{
		includeEphemeral: strings.Contains(workQuery, "--include-ephemeral"),
	}
	for _, tok := range shellquote.Split(workQuery) {
		if tok == "sh" {
			break
		}
		switch {
		case strings.HasPrefix(tok, "GC_CONTROL_TARGET="):
			parsed.target = strings.TrimPrefix(tok, "GC_CONTROL_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_SESSION_NAME="):
			parsed.controlSessionName = strings.TrimPrefix(tok, "GC_CONTROL_SESSION_NAME=")
		case strings.HasPrefix(tok, "GC_CONTROL_LEGACY_TARGET="):
			parsed.legacyTarget = strings.TrimPrefix(tok, "GC_CONTROL_LEGACY_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_BARE_TARGET="):
			parsed.bareTarget = strings.TrimPrefix(tok, "GC_CONTROL_BARE_TARGET=")
		}
	}
	return parsed, parsed.target != ""
}

// envListValue looks up key in a KEY=VALUE environment list such as the one
// mergeRuntimeEnv produces, preferring the last match (matching os/exec's own
// last-wins semantics for duplicate keys).
func envListValue(environ []string, key string) string {
	prefix := key + "="
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], prefix); ok {
			return v
		}
	}
	return ""
}

// candidateLegacyVariant mirrors the shell loop's per-candidate legacy
// expansion: `case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac`.
// This is a plain suffix rewrite of whatever raw session/alias/id string is
// being checked, distinct from workflowServeLegacyControlRoute (which only
// matches a qualified-name-shaped target).
func candidateLegacyVariant(id string) string {
	const suffix = "control-dispatcher"
	if !strings.HasSuffix(id, suffix) {
		return ""
	}
	return strings.TrimSuffix(id, suffix) + "workflow-control"
}

// controlReadyCandidates returns the deduped, precedence-ordered assignee
// candidates the shell script would have checked: GC_CONTROL_SESSION_NAME,
// GC_SESSION_NAME, GC_ALIAS, GC_CONTROL_TARGET, GC_SESSION_ID, each paired
// with its control-dispatcher -> workflow-control legacy variant.
func controlReadyCandidates(parsed parsedControlReadyQuery, envList []string) []string {
	sources := []string{
		parsed.controlSessionName,
		envListValue(envList, "GC_SESSION_NAME"),
		envListValue(envList, "GC_ALIAS"),
		parsed.target,
		envListValue(envList, "GC_SESSION_ID"),
	}

	seen := make(map[string]struct{}, len(sources)*2)
	var candidates []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	for _, id := range sources {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		add(id)
		add(candidateLegacyVariant(id))
	}
	return candidates
}

// controlReadyRoutes returns the routes routed_ready would have checked, in
// order: the target itself, its legacy alias, its bare alias.
func controlReadyRoutes(parsed parsedControlReadyQuery) []string {
	var routes []string
	for _, route := range []string{parsed.target, parsed.legacyTarget, parsed.bareTarget} {
		route = strings.TrimSpace(route)
		if route != "" {
			routes = append(routes, route)
		}
	}
	return routes
}

// filterReadyByAssignee mirrors `bd ready --assignee=$cand --exclude-type=epic --limit=N`.
// ready is expected to already be in canonical ready order
// (SortBeadsReadyOrder), matching bd's own default (no --sort) ready order.
func filterReadyByAssignee(ready []beads.Bead, assignee string, limit int) []beads.Bead {
	var out []beads.Bead
	for _, b := range ready {
		if b.Assignee != assignee || b.Type == controlReadyExcludeType {
			continue
		}
		out = append(out, b)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// filterReadyByRoute mirrors `bd ready --metadata-field $metadataKey=$route --unassigned --exclude-type=epic --exclude-label "hold:mayor" --exclude-label "hold:external" --sort oldest --limit=N`.
// This is a route-scoped, unassigned tier (Tier 3 pool-demand/control-dispatcher
// routing), so held beads must be excluded (ga-5736js): filterReadyByAssignee
// (Tier 1/2, assignee-scoped) stays hold-transparent by design and must not
// gain this filter.
func filterReadyByRoute(ready []beads.Bead, metadataKey, route string) []beads.Bead {
	var matched []beads.Bead
	for _, b := range ready {
		if b.Assignee != "" || b.Type == controlReadyExcludeType {
			continue
		}
		if b.Metadata[metadataKey] != route {
			continue
		}
		held := false
		for _, label := range beadmeta.DispatchHoldLabels {
			if beadLabelsContain(b.Labels, label) {
				held = true
				break
			}
		}
		if held {
			continue
		}
		matched = append(matched, b)
	}
	beads.SortBeads(matched, beads.SortCreatedAsc)
	if len(matched) > workflowServeScanLimit {
		matched = matched[:workflowServeScanLimit]
	}
	return matched
}

// mergeControlReadyGroups flattens the per-candidate/per-route result groups
// in the order they were checked, dropping beads still mid-instantiation and
// deduping by ID on first occurrence -- mirroring the shell script's closing
// `jq -s 'reduce add[] as $item (...)'` filter exactly, including its
// specific quirk: an instantiating-tagged occurrence of an ID is skipped
// WITHOUT being marked seen, so a later non-instantiating occurrence of the
// same ID still gets admitted.
func mergeControlReadyGroups(groups ...[]beads.Bead) []beads.Bead {
	seen := make(map[string]struct{})
	var merged []beads.Bead
	for _, group := range groups {
		for _, b := range group {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.InstantiatingMetadataKey]) != "" {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	return merged
}

// evaluateControlReady answers a control-dispatcher readiness scan against an
// already-fetched ready set (the single batched read of
// controlReadyFallbackReady), applying the exact candidate precedence, legacy/bare route
// aliasing, and instantiating-metadata dedup that
// workflowServeControlReadyQueryForBeads encodes as shell.
func evaluateControlReady(ready []beads.Bead, parsed parsedControlReadyQuery, envList []string) []beads.Bead {
	var groups [][]beads.Bead
	for _, cand := range controlReadyCandidates(parsed, envList) {
		groups = append(groups, filterReadyByAssignee(ready, cand, workflowServeScanLimit))
	}
	for _, route := range controlReadyRoutes(parsed) {
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, route))
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RoutedToMetadataKey, route))
	}
	return mergeControlReadyGroups(groups...)
}

func beadsToHookBeads(items []beads.Bead) []hookBead {
	out := make([]hookBead, 0, len(items))
	for _, b := range items {
		out = append(out, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
	}
	return out
}

// controlReadyFallbackReady is the batched ready scan every control-ready sweep
// reads: one call per leg, nothing reused from an earlier sweep. The "fallback"
// name predates ga-yhp, when an in-process snapshot was tried first.
//
// It reads whichever ledger(s) the control dispatcher will actually dispatch
// against, which controlGraphBinding and controlGraphExtraLeg answer between
// them:
//
//   - A CITY scope whose graph class relocated reads the binding INSTEAD of its
//     own store. `bd` in dir speaks to the work store, and the control beads
//     there are the copies the migration retained, which no longer receive the
//     workflow's mutations. Enumerating those would hand the drain loop a queue
//     of ids the dispatch then no-ops on forever.
//   - A RIG scope on that same city reads its own store AND the binding. Its
//     queue is split across both — its own workflows minted control beads
//     locally, and city-scoped molecules minted theirs in the city-keyed binding
//     and routed them here by name — so a scope-only scan answers `[]` for a
//     queue that is not empty.
//
// Every leg fails LOUD, matching `gc ready`: a work query has nowhere to say
// "this answer is short", so a leg that errors must not degrade to a partial
// array that reads as "no work".
func controlReadyFallbackReady(dir, cityPath string, env map[string]string, includeEphemeral bool) ([]beads.Bead, error) {
	if binding, relocated := controlGraphBinding(cityPath, dir); relocated {
		return controlReadyBindingReady(dir, binding, includeEphemeral)
	}
	scoped, err := controlReadyScopeShellReady(dir, env, includeEphemeral)
	if err != nil {
		return nil, err
	}
	binding, federated := controlGraphExtraLeg(cityPath, dir)
	if !federated {
		return scoped, nil
	}
	graphRows, err := controlReadyBindingReady(dir, binding, includeEphemeral)
	if err != nil {
		return nil, err
	}
	return mergeControlReadyLegs(scoped, graphRows), nil
}

// mergeControlReadyLegs unions the legs in order, first leg winning on a
// duplicate id, then restores canonical ready order over the whole set.
//
// The global re-sort is a deliberate divergence from `gc ready`'s federation,
// which preserves per-leg order. evaluateControlReady's inputs are documented as
// canonical (see filterReadyByAssignee), and its assignee tiers cap at
// workflowServeScanLimit by truncating the head of that order — so leaving the
// graph leg's rows appended after the scope leg's would let leg membership, not
// readiness, decide which beads survive the cap.
func mergeControlReadyLegs(legs ...[]beads.Bead) []beads.Bead {
	var merged []beads.Bead
	seen := make(map[string]struct{})
	for _, leg := range legs {
		for _, b := range leg {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	beads.SortBeadsReadyOrder(merged)
	return merged
}

// controlReadyScopeShellReady is the scope leg: the batched ready scan taken by
// shelling `bd` in the scope directory, exactly as it always was.
func controlReadyScopeShellReady(dir string, env map[string]string, includeEphemeral bool) ([]beads.Bead, error) {
	query := fmt.Sprintf("bd --readonly --sandbox ready --json --exclude-type=%s --limit=%d", controlReadyExcludeType, controlReadyFallbackLimit)
	if includeEphemeral {
		query += " --include-ephemeral"
	}
	output, err := shellWorkQueryWithEnv(query, dir, mergeRuntimeEnv(os.Environ(), env))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if !workQueryHasReadyWork(trimmed) {
		return nil, nil
	}
	var result []beads.Bead
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("control-ready fallback: unexpected bd ready output: %s", trimmed)
	}
	if len(result) == controlReadyFallbackLimit {
		log.Printf("control-ready fallback: bd ready for %s returned exactly the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyFallbackLimit)
	}
	beads.SortBeadsReadyOrder(result)
	return result, nil
}

// controlReadyBindingReady is the relocated-graph arm of the fallback: the same
// batched ready scan, taken in-process against the binding instead of by
// shelling `bd` in a directory that no longer holds the class.
//
// It reproduces the shell arm's three filters rather than approximating them:
// --include-ephemeral is the TierBoth/TierIssues split BdStore.Ready itself
// applies, --exclude-type is applied in Go because ReadyQuery carries no type
// selector, and the limit is taken after that exclusion so the batched cap means
// the same thing on both arms.
func controlReadyBindingReady(dir string, binding beads.Store, includeEphemeral bool) ([]beads.Bead, error) {
	tier := beads.TierIssues
	if includeEphemeral {
		tier = beads.TierBoth
	}
	ready, err := binding.Ready(beads.ReadyQuery{TierMode: tier})
	if err != nil {
		return nil, fmt.Errorf("control-ready fallback: reading the graph binding for %s: %w", dir, err)
	}
	result := make([]beads.Bead, 0, len(ready))
	for _, bead := range ready {
		if bead.Type == controlReadyExcludeType {
			continue
		}
		result = append(result, bead)
		if len(result) == controlReadyFallbackLimit {
			log.Printf("control-ready fallback: the graph binding for %s returned at least the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyFallbackLimit)
			break
		}
	}
	beads.SortBeadsReadyOrder(result)
	return result, nil
}

// tryControlReadyFromCacheOrFallback answers a control-dispatcher readiness
// scan in-process instead of running workflowServeControlReadyQueryForBeads's
// shell script. handled reports whether workQuery was even recognized as a
// control-ready query; when handled is false the caller must run workQuery
// as a shell command exactly as before. This changes the DATA SOURCE for
// control-dispatcher readiness, not the decision logic (ga-ak6rt1): candidate
// precedence, legacy/bare route aliasing, and the instantiating-metadata
// dedup filter are reproduced exactly by evaluateControlReady.
//
// A sweep is one batched read (controlReadyFallbackReady) and loads no city
// config. The name is kept from the cache it no longer has (ga-yhp) so the
// change stays small to carry across branches.
func tryControlReadyFromCacheOrFallback(workQuery, dir string, env map[string]string) (queue []hookBead, handled bool, err error) {
	parsed, ok := parseControlReadyQuery(workQuery)
	if !ok {
		return nil, false, nil
	}

	envList := mergeRuntimeEnv(os.Environ(), env)
	ready, err := controlReadyFallbackReady(dir, cityForStoreDir(dir), env, parsed.includeEphemeral)
	if err != nil {
		return nil, true, err
	}
	return beadsToHookBeads(evaluateControlReady(ready, parsed, envList)), true, nil
}
