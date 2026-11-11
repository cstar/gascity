package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// fakeBdLedger is one bd workspace answered in the JSON shapes bd 1.3.2
// emits for the reads `gc convoy list` makes (verified against a live bd on
// 2026-11-11):
//
//   - `bd list` / `bd query` return issue objects carrying their outgoing
//     dependency records; `bd list --parent X` also matches dotted ids X.*
//     that have no parent-child edge (the Go side filters those out).
//   - `bd dep list <one id>` returns the target issue objects with
//     dependency_type, DROPPING edges whose target has no row.
//   - `bd dep list <ids...>` returns raw {issue_id, depends_on_id, type}
//     records, KEEPING edges to missing targets; missing anchors are skipped.
//   - `bd dep list <ids...> --direction=up --type=T` returns the dependent
//     issue objects without the anchor id, and fails on a missing anchor.
//   - `bd show --json <ids...>` returns the found rows only, exit 0; with no
//     row found at all it fails "not found". It never sees wisps.
type fakeBdLedger struct {
	mu     sync.Mutex
	issues []*fakeBdIssue
	deps   []beads.Dep
	calls  [][]string
	// vanishAfterConvoyList, when set, is deleted right after the first
	// `bd list --type=convoy` has answered with it: a convoy that is gone by
	// the time the reads that follow run.
	vanishAfterConvoyList string
}

type fakeBdIssue struct {
	id        string
	title     string
	status    string
	typ       string
	created   time.Time
	ephemeral bool
}

func (l *fakeBdLedger) add(id, title, typ string, ephemeral bool) {
	l.issues = append(l.issues, &fakeBdIssue{
		id:        id,
		title:     title,
		status:    "open",
		typ:       typ,
		created:   time.Date(2026, 11, 11, 0, 0, len(l.issues), 0, time.UTC),
		ephemeral: ephemeral,
	})
}

func (l *fakeBdLedger) dep(from, to, typ string) {
	l.deps = append(l.deps, beads.Dep{IssueID: from, DependsOnID: to, Type: typ})
}

func (l *fakeBdLedger) find(id string) *fakeBdIssue {
	for _, is := range l.issues {
		if is.id == id {
			return is
		}
	}
	return nil
}

func (l *fakeBdLedger) parentOf(id string) string {
	for _, d := range l.deps {
		if d.IssueID == id && d.Type == "parent-child" {
			return d.DependsOnID
		}
	}
	return ""
}

func (l *fakeBdLedger) issueJSON(is *fakeBdIssue, depType string) map[string]any {
	obj := map[string]any{
		"id":         is.id,
		"title":      is.title,
		"status":     is.status,
		"issue_type": is.typ,
		"created_at": is.created.Format(time.RFC3339),
		"updated_at": is.created.Format(time.RFC3339),
	}
	if is.ephemeral {
		obj["ephemeral"] = true
	}
	if parent := l.parentOf(is.id); parent != "" {
		obj["parent"] = parent
	}
	var deps []map[string]any
	for _, d := range l.deps {
		if d.IssueID == is.id {
			deps = append(deps, map[string]any{"issue_id": d.IssueID, "depends_on_id": d.DependsOnID, "type": d.Type})
		}
	}
	if len(deps) > 0 {
		obj["dependencies"] = deps
	}
	if depType != "" {
		obj["dependency_type"] = depType
	}
	return obj
}

func marshalRows(rows []map[string]any) ([]byte, error) {
	if rows == nil {
		rows = []map[string]any{}
	}
	return json.Marshal(rows)
}

func (l *fakeBdLedger) run(_, name string, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, append([]string(nil), args...))
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch args[0] {
	case "list":
		return l.list(args[1:])
	case "query":
		return l.query(args[1:])
	case "dep":
		if len(args) > 1 && args[1] == "list" {
			return l.depList(args[2:])
		}
	case "show":
		return l.show(args[1:])
	}
	return nil, fmt.Errorf("unexpected command bd %v", args)
}

func (l *fakeBdLedger) matchStatus(is *fakeBdIssue, status string, all bool) bool {
	if status != "" {
		return is.status == status
	}
	return all || is.status != "closed"
}

func (l *fakeBdLedger) list(args []string) ([]byte, error) {
	var typ, status, parent string
	all := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--type="):
			typ = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "--status="):
			status = strings.TrimPrefix(a, "--status=")
		case a == "--all":
			all = true
		case a == "--parent":
			i++
			parent = args[i]
		case a == "--limit":
			i++
		case a == "--json", a == "--include-infra", a == "--include-gates", a == "--include-templates", a == "--skip-labels":
		default:
			return nil, fmt.Errorf("fake bd list: unexpected arg %q", a)
		}
	}
	var rows []map[string]any
	for _, is := range l.issues {
		if is.ephemeral || (typ != "" && is.typ != typ) || !l.matchStatus(is, status, all) {
			continue
		}
		if parent != "" && l.parentOf(is.id) != parent && !strings.HasPrefix(is.id, parent+".") {
			continue
		}
		rows = append(rows, l.issueJSON(is, ""))
	}
	if typ == "convoy" && l.vanishAfterConvoyList != "" {
		kept := l.issues[:0]
		for _, is := range l.issues {
			if is.id != l.vanishAfterConvoyList {
				kept = append(kept, is)
			}
		}
		l.issues = kept
		l.vanishAfterConvoyList = ""
	}
	return marshalRows(rows)
}

func (l *fakeBdLedger) query(args []string) ([]byte, error) {
	var expr string
	all := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
		case a == "--all":
			all = true
		case a == "--limit":
			i++
		case strings.HasPrefix(a, "--"):
			return nil, fmt.Errorf("fake bd query: unexpected arg %q", a)
		default:
			expr = a
		}
	}
	clauses := map[string]string{}
	for _, c := range strings.Split(expr, " AND ") {
		k, v, ok := strings.Cut(strings.TrimSpace(c), "=")
		if !ok {
			return nil, fmt.Errorf("fake bd query: bad clause %q", c)
		}
		switch k {
		case "ephemeral", "type", "status", "parent", "id":
			clauses[k] = v
		default:
			return nil, fmt.Errorf("fake bd query: unknown field: %s", k)
		}
	}
	var rows []map[string]any
	for _, is := range l.issues {
		if strconv.FormatBool(is.ephemeral) != clauses["ephemeral"] {
			continue
		}
		if v, ok := clauses["type"]; ok && is.typ != v {
			continue
		}
		if !l.matchStatus(is, clauses["status"], all) {
			continue
		}
		if v, ok := clauses["parent"]; ok && l.parentOf(is.id) != v {
			continue
		}
		if v, ok := clauses["id"]; ok && is.id != v {
			continue
		}
		rows = append(rows, l.issueJSON(is, ""))
	}
	return marshalRows(rows)
}

func (l *fakeBdLedger) depList(args []string) ([]byte, error) {
	var ids []string
	up := false
	typ := ""
	for _, a := range args {
		switch {
		case a == "--json":
		case a == "--direction=up":
			up = true
		case strings.HasPrefix(a, "--type="):
			typ = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "--"):
			return nil, fmt.Errorf("fake bd dep list: unexpected arg %q", a)
		default:
			ids = append(ids, a)
		}
	}
	if up {
		var rows []map[string]any
		for _, id := range ids {
			if l.find(id) == nil {
				return nil, fmt.Errorf(`exit status 1: {"error": "not found: issue %s"}`, id)
			}
			for _, d := range l.deps {
				if d.DependsOnID != id || (typ != "" && d.Type != typ) {
					continue
				}
				if child := l.find(d.IssueID); child != nil {
					rows = append(rows, l.issueJSON(child, d.Type))
				}
			}
		}
		return marshalRows(rows)
	}
	if len(ids) == 1 {
		var rows []map[string]any
		for _, d := range l.deps {
			if d.IssueID != ids[0] {
				continue
			}
			if target := l.find(d.DependsOnID); target != nil {
				rows = append(rows, l.issueJSON(target, d.Type))
			}
		}
		return marshalRows(rows)
	}
	var rows []map[string]any
	for _, id := range ids {
		for _, d := range l.deps {
			if d.IssueID == id {
				rows = append(rows, map[string]any{"issue_id": d.IssueID, "depends_on_id": d.DependsOnID, "type": d.Type})
			}
		}
	}
	return marshalRows(rows)
}

func (l *fakeBdLedger) show(args []string) ([]byte, error) {
	var rows []map[string]any
	var asked []string
	for _, a := range args {
		if a == "--json" {
			continue
		}
		asked = append(asked, a)
		if is := l.find(a); is != nil && !is.ephemeral {
			rows = append(rows, l.issueJSON(is, ""))
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("exit status 1: no issue found matching %v", asked)
	}
	return marshalRows(rows)
}

func (l *fakeBdLedger) resetCalls() {
	l.mu.Lock()
	l.calls = nil
	l.mu.Unlock()
}

func (l *fakeBdLedger) snapshotCalls() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]string(nil), l.calls...)
}

func countCalls(calls [][]string, match func([]string) bool) int {
	n := 0
	for _, c := range calls {
		if match(c) {
			n++
		}
	}
	return n
}

func isListParent(c []string) bool {
	if len(c) == 0 || c[0] != "list" {
		return false
	}
	for _, a := range c {
		if a == "--parent" {
			return true
		}
	}
	return false
}

func isSingleShow(c []string) bool {
	return len(c) == 3 && c[0] == "show" && c[1] == "--json"
}

func isSingleDepList(c []string) bool {
	return len(c) == 4 && c[0] == "dep" && c[1] == "list" && c[3] == "--json"
}

// convoyListLedgers builds two policy-wrapped BdStores whose ledgers hold k
// convoys with one tracked member each, plus whatever extra the caller adds.
func convoyListLedgers(t *testing.T, k int, extra func(prefix string, l *fakeBdLedger)) ([]convoyStoreView, []*fakeBdLedger) {
	t.Helper()
	var views []convoyStoreView
	var ledgers []*fakeBdLedger
	for _, prefix := range []string{"hq", "rg"} {
		l := &fakeBdLedger{}
		for i := 1; i <= k; i++ {
			convoyID := fmt.Sprintf("%s-c%03d", prefix, i)
			memberID := fmt.Sprintf("%s-t%03d", prefix, i)
			l.add(convoyID, "convoy "+convoyID, "convoy", false)
			l.add(memberID, "task "+memberID, "task", false)
			l.dep(convoyID, memberID, "tracks")
			if i%3 == 0 {
				l.find(memberID).status = "closed"
			}
		}
		if extra != nil {
			extra(prefix, l)
		}
		store := wrapStoreWithBeadPolicies(beads.NewBdStore("/"+prefix, l.run), &config.City{})
		views = append(views, convoyStoreView{path: "/" + prefix, store: store})
		ledgers = append(ledgers, l)
	}
	return views, ledgers
}

func runConvoyListBoth(t *testing.T, views []convoyStoreView, ledgers []*fakeBdLedger, jsonOut bool) (batched, legacy string, batchedCalls, legacyCalls [][][]string) {
	t.Helper()
	run := func() (string, [][][]string) {
		for _, l := range ledgers {
			l.resetCalls()
		}
		var stdout, stderr bytes.Buffer
		if code := doConvoyListAcrossStores(views, jsonOut, &stdout, &stderr); code != 0 {
			t.Fatalf("doConvoyListAcrossStores = %d; stderr: %s", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		calls := make([][][]string, len(ledgers))
		for i, l := range ledgers {
			calls[i] = l.snapshotCalls()
		}
		return stdout.String(), calls
	}
	batched, batchedCalls = run()
	saved := convoyListPrefetchChildren
	convoyListPrefetchChildren = nil
	t.Cleanup(func() { convoyListPrefetchChildren = saved })
	legacy, legacyCalls = run()
	convoyListPrefetchChildren = saved
	return batched, legacy, batchedCalls, legacyCalls
}

func TestConvoyListBatchesMemberReadsPerStore(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	perStore := map[int][]int{}
	for _, k := range []int{2, 60} {
		for _, jsonOut := range []bool{false, true} {
			views, ledgers := convoyListLedgers(t, k, nil)
			batched, legacy, batchedCalls, legacyCalls := runConvoyListBoth(t, views, ledgers, jsonOut)
			if batched != legacy {
				t.Fatalf("k=%d json=%v: batched output differs from the per-convoy output\nbatched:\n%s\nlegacy:\n%s", k, jsonOut, batched, legacy)
			}
			if !strings.Contains(batched, fmt.Sprintf("hq-c%03d", k)) {
				t.Fatalf("k=%d: output is missing the last convoy:\n%s", k, batched)
			}
			for i, calls := range batchedCalls {
				if len(calls) > 5 {
					t.Fatalf("k=%d store %d: %d bd calls, want at most 5: %q", k, i, len(calls), calls)
				}
				if n := countCalls(calls, isListParent); n != 0 {
					t.Fatalf("k=%d store %d: %d per-convoy child lists, want 0", k, i, n)
				}
				if n := countCalls(calls, isSingleShow); n != 0 {
					t.Fatalf("k=%d store %d: %d single-id shows, want 0", k, i, n)
				}
				if n := countCalls(calls, isSingleDepList); n != 0 {
					t.Fatalf("k=%d store %d: %d single-id dep lists, want 0", k, i, n)
				}
				perStore[k] = append(perStore[k], len(calls))
			}
			// The per-convoy path is the O(convoys) shape this replaces.
			if n := len(legacyCalls[0]); n < 4*k {
				t.Fatalf("k=%d: legacy path made %d calls, want at least %d (sanity)", k, n, 4*k)
			}
		}
	}
	if fmt.Sprint(perStore[2]) != fmt.Sprint(perStore[60]) {
		t.Fatalf("bd calls per store: k=2 %v, k=60 %v; want the same count regardless of convoys", perStore[2], perStore[60])
	}
}

func TestConvoyListBatchFallsBackPerConvoyOnlyWhereItMust(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	views, ledgers := convoyListLedgers(t, 6, func(prefix string, l *fakeBdLedger) {
		// A dangling tracks target (no row).
		l.dep(prefix+"-c001", prefix+"-gone", "tracks")
		// A legacy parent-child member.
		l.add(prefix+"-k1", "legacy kid", "task", false)
		l.dep(prefix+"-k1", prefix+"-c002", "parent-child")
		// A dotted id bd's --parent matches without any parent-child edge.
		l.add(prefix+"-c003.1", "dotted lookalike", "task", false)
		// A tracked wisp: bd show cannot see it, Get's wisp fallback can.
		l.add(prefix+"-w1", "tracked wisp", "task", true)
		l.dep(prefix+"-c004", prefix+"-w1", "tracks")
		// An ephemeral convoy.
		l.add(prefix+"-e1", "ephemeral convoy", "convoy", true)
	})
	for _, jsonOut := range []bool{false, true} {
		batched, legacy, batchedCalls, _ := runConvoyListBoth(t, views, ledgers, jsonOut)
		if batched != legacy {
			t.Fatalf("json=%v: batched output differs from the per-convoy output\nbatched:\n%s\nlegacy:\n%s", jsonOut, batched, legacy)
		}
		// bd's one-id dep list drops the edge to the missing target, so the
		// per-convoy path reports c001 as 0/1; the batch must not "fix" that.
		if !jsonOut && !strings.Contains(batched, "hq-c001  convoy hq-c001    0/1 closed") {
			t.Fatalf("c001 progress changed:\n%s", batched)
		}
		for i, calls := range batchedCalls {
			// c001 (dangling), c002 (legacy child), c004 (wisp target) and e1
			// (ephemeral) take the per-convoy path; the other four do not.
			if n := countCalls(calls, isListParent); n != 4 {
				t.Fatalf("json=%v store %d: %d per-convoy child lists, want 4: %q", jsonOut, i, n, calls)
			}
			if n := countCalls(calls, isSingleDepList); n != 4 {
				t.Fatalf("json=%v store %d: %d single-id dep lists, want 4: %q", jsonOut, i, n, calls)
			}
		}
	}
}

func TestConvoyListBatchSkipsStoresWithFewerThanTwoConvoys(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	for _, k := range []int{0, 1} {
		views, ledgers := convoyListLedgers(t, k, nil)
		batched, legacy, batchedCalls, legacyCalls := runConvoyListBoth(t, views, ledgers, false)
		if batched != legacy {
			t.Fatalf("k=%d: batched output differs\nbatched:\n%s\nlegacy:\n%s", k, batched, legacy)
		}
		for i := range batchedCalls {
			if len(batchedCalls[i]) != len(legacyCalls[i]) {
				t.Fatalf("k=%d store %d: batched made %d calls, legacy %d; want the per-convoy path unchanged", k, i, len(batchedCalls[i]), len(legacyCalls[i]))
			}
		}
		if k == 0 && len(batchedCalls[0]) != 2 {
			t.Fatalf("empty store: %d calls, want 2 (both tiers of the convoy list)", len(batchedCalls[0]))
		}
	}
}

func TestConvoyListBatchProbeFailureFallsBackWithSameOutput(t *testing.T) {
	views, ledgers := convoyListLedgers(t, 4, nil)
	// A convoy listed by bd but gone by the time the probe runs: the probe
	// fails on the missing anchor and the store reads per convoy.
	ledgers[1].add("rg-ghost", "ghost", "convoy", false)
	ledgers[1].vanishAfterConvoyList = "rg-ghost"

	t.Setenv("GC_DEBUG", "1")
	var stdout, stderr bytes.Buffer
	if code := doConvoyListAcrossStores(views, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doConvoyListAcrossStores = %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "convoy list: store /rg: batched 0, fallback 5 (") {
		t.Fatalf("stderr = %q, want the fallback diagnostic for /rg", stderr.String())
	}
	if !strings.Contains(stderr.String(), "convoy list: store /hq: batched 4, fallback 0\n") {
		t.Fatalf("stderr = %q, want the batched diagnostic for /hq", stderr.String())
	}
	if !strings.Contains(stdout.String(), "rg-ghost") {
		t.Fatalf("stdout = %q, want the ghost convoy still listed", stdout.String())
	}
}

func TestConvoyStoreHandleGroupsKeepsOneGoroutinePerStore(t *testing.T) {
	a := beads.NewMemStore()
	b := beads.NewMemStore()
	groups := convoyStoreHandleGroups([]beads.Store{a, b, a, nil, b})
	got := fmt.Sprint(groups)
	if want := "[[0 2] [1 4] [3]]"; got != want {
		t.Fatalf("groups = %s, want %s", got, want)
	}

	var mu sync.Mutex
	active := map[beads.Store]int{}
	var order []int
	forEachConvoyStore([]beads.Store{a, b, a, b, a}, func(i int) {
		store := []beads.Store{a, b, a, b, a}[i]
		mu.Lock()
		active[store]++
		if active[store] > 1 {
			mu.Unlock()
			t.Errorf("store %d read by two goroutines at once", i)
			return
		}
		order = append(order, i)
		mu.Unlock()
		// Yield so another goroutine gets the chance to overlap this one.
		for range 100 {
			runtime.Gosched()
		}
		mu.Lock()
		active[store]--
		mu.Unlock()
	})
	sort.Ints(order)
	if fmt.Sprint(order) != "[0 1 2 3 4]" {
		t.Fatalf("ran %v, want every index once", order)
	}
}
