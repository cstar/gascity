package beads_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// childPresenceRunner records every bd invocation and answers the up-direction
// parent-child probe and `bd show` from fixed tables, in the shapes bd 1.3.2
// emits: the probe returns child issue objects WITHOUT the anchor id, and
// `bd show` returns the found rows only (exit 0) with the full dependency list.
type childPresenceRunner struct {
	mu sync.Mutex
	// childrenOf maps a parent id to the ids of its parent-child children.
	childrenOf map[string][]string
	// shown maps a child id to the raw `bd show` JSON object for it. A child
	// absent from this map is "not found" by bd show.
	shown map[string]string
	// probeErr, when set, is returned for every probe.
	probeErr error
	calls    [][]string
}

func (r *childPresenceRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	switch {
	case args[0] == "dep" && len(args) > 1 && args[1] == "list":
		if r.probeErr != nil {
			return nil, r.probeErr
		}
		var rows []string
		for _, a := range args[2:] {
			if strings.HasPrefix(a, "--") {
				continue
			}
			for _, child := range r.childrenOf[a] {
				rows = append(rows, fmt.Sprintf(`{"id":%q,"title":%q,"status":"open","issue_type":"task","dependency_type":"parent-child"}`, child, child))
			}
		}
		return []byte("[" + strings.Join(rows, ",") + "]"), nil
	case args[0] == "show":
		var rows []string
		for _, a := range args[1:] {
			if a == "--json" {
				continue
			}
			if obj, ok := r.shown[a]; ok {
				rows = append(rows, obj)
			}
		}
		return []byte("[" + strings.Join(rows, ",") + "]"), nil
	}
	return nil, fmt.Errorf("unexpected command: %s %v", name, args)
}

func (r *childPresenceRunner) callCount(verb string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == verb {
			n++
		}
	}
	return n
}

func shownChild(id, parent string, parentChildTargets ...string) string {
	deps := make([]string, 0, len(parentChildTargets))
	for _, target := range parentChildTargets {
		deps = append(deps, fmt.Sprintf(`{"issue_id":%q,"depends_on_id":%q,"type":"parent-child"}`, id, target))
	}
	parentField := ""
	if parent != "" {
		parentField = fmt.Sprintf(`,"parent":%q`, parent)
	}
	return fmt.Sprintf(`{"id":%q,"title":%q,"status":"open","issue_type":"task"%s,"dependencies":[%s]}`, id, id, parentField, strings.Join(deps, ","))
}

func flaggedSet(ids ...string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func TestBdStoreParentsWithChildrenNoChildrenIsOneCall(t *testing.T) {
	r := &childPresenceRunner{}
	s := beads.NewBdStore("/city", r.run)

	got, err := s.ParentsWithChildren([]string{"gc-1", "gc-2", "gc-1", ""})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("flagged = %#v, want an empty non-nil set", got)
	}
	want := [][]string{{"bd", "dep", "list", "gc-1", "gc-2", "--direction=up", "--type=parent-child", "--json"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %q, want %q", r.calls, want)
	}
}

func TestBdStoreParentsWithChildrenEmptyInputRunsNothing(t *testing.T) {
	r := &childPresenceRunner{}
	s := beads.NewBdStore("/city", r.run)
	got, err := s.ParentsWithChildren(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("flagged = %#v, want an empty non-nil set", got)
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %q, want none", r.calls)
	}
}

func TestBdStoreParentsWithChildrenFlagsOnlyTheAttributedParent(t *testing.T) {
	r := &childPresenceRunner{
		childrenOf: map[string][]string{"gc-2": {"gc-2.1", "gc-2.2"}},
		shown: map[string]string{
			"gc-2.1": shownChild("gc-2.1", "gc-2", "gc-2"),
			"gc-2.2": shownChild("gc-2.2", "gc-2", "gc-2"),
		},
	}
	s := beads.NewBdStore("/city", r.run)

	got, err := s.ParentsWithChildren([]string{"gc-1", "gc-2", "gc-3"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, flaggedSet("gc-2")) {
		t.Fatalf("flagged = %v, want {gc-2}", got)
	}
	if n := r.callCount("dep"); n != 1 {
		t.Fatalf("probe calls = %d, want 1", n)
	}
	if n := r.callCount("show"); n != 1 {
		t.Fatalf("show calls = %d, want 1 batched show", n)
	}
	last := r.calls[len(r.calls)-1]
	if want := []string{"bd", "show", "--json", "gc-2.1", "gc-2.2"}; !reflect.DeepEqual(last, want) {
		t.Fatalf("show argv = %q, want %q", last, want)
	}
}

// A child carrying parent-child edges to two requested parents flags both,
// because bd's single "parent" field names only one of them.
func TestBdStoreParentsWithChildrenFlagsEveryParentChildTarget(t *testing.T) {
	r := &childPresenceRunner{
		childrenOf: map[string][]string{"gc-1": {"gc-9"}, "gc-2": {"gc-9"}},
		shown:      map[string]string{"gc-9": shownChild("gc-9", "gc-1", "gc-1", "gc-2")},
	}
	s := beads.NewBdStore("/city", r.run)

	got, err := s.ParentsWithChildren([]string{"gc-1", "gc-2", "gc-3"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, flaggedSet("gc-1", "gc-2")) {
		t.Fatalf("flagged = %v, want {gc-1, gc-2}", got)
	}
}

func TestBdStoreParentsWithChildrenUnattributableChildFlagsEveryParent(t *testing.T) {
	cases := map[string]*childPresenceRunner{
		// bd show cannot see the child (a wisp, or deleted between reads).
		"unresolved child": {
			childrenOf: map[string][]string{"gc-2": {"gc-wisp-7"}},
		},
		// bd show answers a parent outside the request.
		"parent outside the request": {
			childrenOf: map[string][]string{"gc-2": {"gc-5.1"}},
			shown:      map[string]string{"gc-5.1": shownChild("gc-5.1", "gc-5", "gc-5")},
		},
		// bd show answers no parent at all.
		"no parent recorded": {
			childrenOf: map[string][]string{"gc-2": {"gc-6"}},
			shown:      map[string]string{"gc-6": shownChild("gc-6", "")},
		},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			s := beads.NewBdStore("/city", r.run)
			got, err := s.ParentsWithChildren([]string{"gc-1", "gc-2", "gc-3"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, flaggedSet("gc-1", "gc-2", "gc-3")) {
				t.Fatalf("flagged = %v, want every requested id", got)
			}
		})
	}
}

func TestBdStoreParentsWithChildrenReturnsReadFailures(t *testing.T) {
	cases := map[string]struct {
		err             error
		wantUnsupported bool
	}{
		"transport":    {err: errors.New("exit status 1: invalid connection"), wantUnsupported: false},
		"missing id":   {err: errors.New(`exit status 1: {"error": "not found: issue gc-2"}`), wantUnsupported: false},
		"unsupported":  {err: errors.New(`exit status 1: operation "IssueRelations" not supported by the postgres backend`), wantUnsupported: true},
		"other unsupp": {err: errors.New("exit status 1: dep list: not supported"), wantUnsupported: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &childPresenceRunner{probeErr: tc.err}
			s := beads.NewBdStore("/city", r.run)
			got, err := s.ParentsWithChildren([]string{"gc-1", "gc-2"})
			if err == nil {
				t.Fatalf("err = nil, flagged = %v; want a read failure", got)
			}
			if got != nil {
				t.Fatalf("flagged = %v, want nil on failure", got)
			}
			if errors.Is(err, beads.ErrChildPresenceUnsupported) != tc.wantUnsupported {
				t.Fatalf("errors.Is(err, ErrChildPresenceUnsupported) = %v, want %v (err = %v)", !tc.wantUnsupported, tc.wantUnsupported, err)
			}
		})
	}
}

func TestBdStoreParentsWithChildrenChunksLargeRequests(t *testing.T) {
	for _, tc := range []struct {
		total int
		want  []int
	}{
		{total: 200, want: []int{200}},
		{total: 201, want: []int{201}},
		{total: 401, want: []int{200, 201}},
		{total: 450, want: []int{200, 200, 50}},
	} {
		t.Run(fmt.Sprint(tc.total), func(t *testing.T) {
			ids := make([]string, tc.total)
			for i := range ids {
				ids[i] = fmt.Sprintf("gc-%d", i)
			}
			// One child under the last parent, so the show leg runs once.
			last := ids[len(ids)-1]
			r := &childPresenceRunner{
				childrenOf: map[string][]string{last: {last + ".1"}},
				shown:      map[string]string{last + ".1": shownChild(last+".1", last, last)},
			}
			s := beads.NewBdStore("/city", r.run)
			got, err := s.ParentsWithChildren(ids)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, flaggedSet(last)) {
				t.Fatalf("flagged = %v, want {%s}", got, last)
			}
			var sizes []int
			for _, c := range r.calls {
				if c[1] != "dep" {
					continue
				}
				// bd dep list <ids...> --direction=up --type=parent-child --json
				sizes = append(sizes, len(c)-6)
			}
			if !reflect.DeepEqual(sizes, tc.want) {
				t.Fatalf("probe chunk sizes = %v, want %v", sizes, tc.want)
			}
		})
	}
}
