package beads_test

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// readyBlockerLookupRunner answers bd ready with dependents of the given
// blockers and records every other bd call. lookup answers the blocker read.
func readyBlockerLookupRunner(t *testing.T, blockerIDs []string, lookup func(args []string) ([]byte, error)) (beads.CommandRunner, *[][]string) {
	t.Helper()
	var deps []string
	for _, id := range blockerIDs {
		deps = append(deps, fmt.Sprintf(`{"issue_id":"bd-dependent","depends_on_id":%q,"type":"blocks"}`, id))
	}
	readyRows := []byte(fmt.Sprintf(`[{"id":"bd-dependent","title":"dependent","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z","dependency_count":%d,"dependencies":[%s]}]`,
		len(blockerIDs), strings.Join(deps, ",")))
	var calls [][]string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ready" {
			return readyRows, nil
		}
		calls = append(calls, append([]string(nil), args...))
		return lookup(args)
	}
	return runner, &calls
}

func argValue(args []string, prefix string) (string, bool) {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// assertTargetedBlockerLookup fails unless call is the exact-ID, both-plane,
// closed-inclusive bd list the veto now issues.
func assertTargetedBlockerLookup(t *testing.T, call []string) []string {
	t.Helper()
	if len(call) == 0 || call[0] != "list" {
		t.Fatalf("blocker lookup %q: want bd list", call)
	}
	ids, ok := argValue(call, "--id=")
	if !ok || ids == "" {
		t.Fatalf("blocker lookup %q scans the closed history: want --id=<blocker ids>", call)
	}
	for _, want := range []string{"--include-ephemeral", "--status=closed", "--all"} {
		if !hasArg(call, want) {
			t.Fatalf("blocker lookup %q: missing %s", call, want)
		}
	}
	return strings.Split(ids, ",")
}

// The work-outcome veto reads its closed blockers by exact id, in one bd list
// across both planes, and still removes a dependent whose blocker closed with
// gc.work_outcome=blocked wherever that blocker lives.
func TestBdStoreReadyVetoReadsClosedBlockersByExactIDs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rows      string
		lookupErr error
		wantReady int
		wantErr   bool
	}{
		{"issue tier blocked", `[{"id":"bd-blocker","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`, nil, 0, false},
		{"wisp tier blocked", `[{"id":"bd-blocker","status":"closed","ephemeral":true,"metadata":{"gc.work_outcome":"blocked"}}]`, nil, 0, false},
		{"closed without outcome", `[{"id":"bd-blocker","status":"closed"}]`, nil, 1, false},
		{"missing", `[]`, nil, 1, false},
		{"unrelated blocked", `[{"id":"bd-unrelated","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`, nil, 1, false},
		{"storage failure", "", errors.New("storage unavailable"), 0, true},
		{"malformed", `[`, nil, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, calls := readyBlockerLookupRunner(t, []string{"bd-other", "bd-blocker"}, func([]string) ([]byte, error) {
				return []byte(tc.rows), tc.lookupErr
			})
			got, err := beads.NewBdStore("/city", runner).Ready()
			if (err != nil) != tc.wantErr || len(got) != tc.wantReady {
				t.Fatalf("Ready() = %+v, %v; want %d ready, error %v", got, err, tc.wantReady, tc.wantErr)
			}
			if len(*calls) != 1 {
				t.Fatalf("blocker reads = %d (%q), want exactly 1 targeted bd list and no full closed scan", len(*calls), *calls)
			}
			ids := assertTargetedBlockerLookup(t, (*calls)[0])
			if want := []string{"bd-blocker", "bd-other"}; strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Fatalf("blocker ids = %v, want sorted %v", ids, want)
			}
		})
	}
}

// Many blockers are read in sorted slices of at most 200 ids, never with an
// empty --id= (which bd treats as no filter at all), and a blocked outcome in
// any slice still vetoes the dependent.
func TestBdStoreReadyVetoChunksBlockerIDs(t *testing.T) {
	var blockers []string
	for i := 0; i < 450; i++ {
		blockers = append(blockers, fmt.Sprintf("bd-b%03d", i))
	}
	runner, calls := readyBlockerLookupRunner(t, blockers, func(args []string) ([]byte, error) {
		ids, _ := argValue(args, "--id=")
		if strings.Contains(","+ids+",", ",bd-b449,") {
			return []byte(`[{"id":"bd-b449","status":"closed","metadata":{"gc.work_outcome":"blocked"}}]`), nil
		}
		return []byte(`[]`), nil
	})
	got, err := beads.NewBdStore("/city", runner).Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Ready() = %+v, want the dependent vetoed by bd-b449 in the last slice", got)
	}
	if len(*calls) != 3 {
		t.Fatalf("blocker reads = %d, want 3 slices for 450 ids", len(*calls))
	}
	var seen []string
	for _, call := range *calls {
		ids := assertTargetedBlockerLookup(t, call)
		if len(ids) > 200 {
			t.Fatalf("slice of %d ids, want at most 200", len(ids))
		}
		seen = append(seen, ids...)
	}
	if !sort.StringsAreSorted(seen) || len(seen) != len(blockers) {
		t.Fatalf("blocker ids across slices: %d ids, sorted=%v; want all %d in order", len(seen), sort.StringsAreSorted(seen), len(blockers))
	}
}
