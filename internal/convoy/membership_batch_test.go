package convoy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchReaderAdapter gives a MemStore the two batch capabilities MemStore does
// not implement itself, with the semantics their bd implementations promise:
// GetExactBatch answers exactly what Get answers, and ParentsWithChildren
// flags exactly the parents whose child List is non-empty. It counts calls so
// a test can pin the round trips.
type batchReaderAdapter struct {
	*beads.MemStore
	mu         sync.Mutex
	depCalls   int
	exactCalls int
	probeCalls int
	exactIDs   [][]string

	depErr   error
	exactErr error
	probeErr error
}

func (a *batchReaderAdapter) readers() MemberBatchReaders {
	return MemberBatchReaders{Deps: a, Exact: a, Children: a}
}

func (a *batchReaderAdapter) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	a.mu.Lock()
	a.depCalls++
	a.mu.Unlock()
	if a.depErr != nil {
		return nil, a.depErr
	}
	return a.MemStore.DepListBatch(ids)
}

func (a *batchReaderAdapter) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	a.mu.Lock()
	a.exactCalls++
	a.exactIDs = append(a.exactIDs, append([]string(nil), ids...))
	a.mu.Unlock()
	if a.exactErr != nil {
		return nil, nil, a.exactErr
	}
	found := make(map[string]beads.Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		b, err := a.Get(id)
		if err != nil {
			unresolved = append(unresolved, id)
			continue
		}
		found[id] = b
	}
	return found, unresolved, nil
}

func (a *batchReaderAdapter) ParentsWithChildren(ids []string) (map[string]bool, error) {
	a.mu.Lock()
	a.probeCalls++
	a.mu.Unlock()
	if a.probeErr != nil {
		return nil, a.probeErr
	}
	flagged := make(map[string]bool)
	for _, id := range ids {
		kids, err := a.List(beads.ListQuery{ParentID: id, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			return nil, err
		}
		if len(kids) > 0 {
			flagged[id] = true
		}
	}
	return flagged, nil
}

type membersBatchFixture struct {
	store   *batchReaderAdapter
	convoys []beads.Bead
	// answered are the convoy ids MembersBatch must answer; the rest must be
	// left to the per-convoy path.
	answered []string
}

func mustCreate(t *testing.T, s *beads.MemStore, b beads.Bead) beads.Bead {
	t.Helper()
	got, err := s.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustDep(t *testing.T, s *beads.MemStore, from, to, typ string) {
	t.Helper()
	if err := s.DepAdd(from, to, typ); err != nil {
		t.Fatal(err)
	}
}

func newMembersBatchFixture(t *testing.T) membersBatchFixture {
	t.Helper()
	mem := beads.NewMemStore()
	var convoys []beads.Bead
	var answered []string
	newConvoy := func(title string, ephemeral bool) beads.Bead {
		c := mustCreate(t, mem, beads.Bead{Title: title, Type: "convoy", Ephemeral: ephemeral})
		convoys = append(convoys, c)
		return c
	}

	// Tracks-only, open and closed members, created out of order.
	tracksOnly := newConvoy("tracks only", false)
	late := mustCreate(t, mem, beads.Bead{Title: "late"})
	early := mustCreate(t, mem, beads.Bead{Title: "early"})
	closed := mustCreate(t, mem, beads.Bead{Title: "closed"})
	if err := mem.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	mustDep(t, mem, tracksOnly.ID, late.ID, TrackingDepType)
	mustDep(t, mem, tracksOnly.ID, early.ID, TrackingDepType)
	mustDep(t, mem, tracksOnly.ID, closed.ID, TrackingDepType)
	answered = append(answered, tracksOnly.ID)

	// Non-tracks edges are ignored; a member shared with another convoy.
	mixed := newConvoy("mixed edge types", false)
	blocker := mustCreate(t, mem, beads.Bead{Title: "blocker"})
	mustDep(t, mem, mixed.ID, blocker.ID, "blocks")
	mustDep(t, mem, mixed.ID, early.ID, TrackingDepType)
	answered = append(answered, mixed.ID)

	// No members at all.
	empty := newConvoy("empty", false)
	answered = append(answered, empty.ID)

	// Legacy parent-child membership: the per-convoy path owns it.
	legacy := newConvoy("legacy child", false)
	mustCreate(t, mem, beads.Bead{Title: "legacy kid", ParentID: legacy.ID})
	mustDep(t, mem, legacy.ID, late.ID, TrackingDepType)

	// A dangling tracks target: the per-convoy path owns it.
	dangling := newConvoy("dangling", false)
	mustDep(t, mem, dangling.ID, "gc-missing", TrackingDepType)
	mustDep(t, mem, dangling.ID, early.ID, TrackingDepType)

	// An ephemeral convoy: the per-convoy path owns it.
	wisp := newConvoy("ephemeral", true)
	mustDep(t, mem, wisp.ID, early.ID, TrackingDepType)

	return membersBatchFixture{
		store:    &batchReaderAdapter{MemStore: mem},
		convoys:  convoys,
		answered: answered,
	}
}

func sortedKeys(m map[string][]beads.Bead) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestMembersBatchMatchesMembersForEveryAnsweredConvoy(t *testing.T) {
	for _, includeClosed := range []bool{true, false} {
		t.Run(fmt.Sprintf("includeClosed=%v", includeClosed), func(t *testing.T) {
			fx := newMembersBatchFixture(t)
			got, err := MembersBatch(fx.store.readers(), fx.convoys, includeClosed)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string(nil), fx.answered...)
			sort.Strings(want)
			if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
				t.Fatalf("answered convoys = %v, want %v", keys, want)
			}
			for id, members := range got {
				legacy, err := Members(fx.store.MemStore, id, includeClosed)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(members, legacy) {
					t.Fatalf("convoy %s: batch members = %+v\nwant Members() = %+v", id, members, legacy)
				}
			}
			if fx.store.depCalls != 1 || fx.store.probeCalls != 1 || fx.store.exactCalls != 1 {
				t.Fatalf("calls dep=%d probe=%d exact=%d, want one of each", fx.store.depCalls, fx.store.probeCalls, fx.store.exactCalls)
			}
		})
	}
}

// Members() of the same id is the reference: the batch must not change what a
// convoy reports, only how many reads it takes.
func TestMembersBatchKeepsTrackedOrderAndDedupe(t *testing.T) {
	mem := beads.NewMemStore()
	a := mustCreate(t, mem, beads.Bead{Title: "a", Type: "convoy"})
	b := mustCreate(t, mem, beads.Bead{Title: "b", Type: "convoy"})
	x := mustCreate(t, mem, beads.Bead{Title: "x"})
	y := mustCreate(t, mem, beads.Bead{Title: "y"})
	mustDep(t, mem, a.ID, y.ID, TrackingDepType)
	mustDep(t, mem, a.ID, x.ID, TrackingDepType)
	mustDep(t, mem, a.ID, x.ID, TrackingDepType)
	mustDep(t, mem, b.ID, x.ID, TrackingDepType)
	store := &batchReaderAdapter{MemStore: mem}

	got, err := MembersBatch(store.readers(), []beads.Bead{a, b, a}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		want, err := Members(mem, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Fatalf("convoy %s: batch = %+v, want %+v", id, got[id], want)
		}
	}
	// The shared target is read once.
	if !reflect.DeepEqual(store.exactIDs, [][]string{{y.ID, x.ID}}) {
		t.Fatalf("exact reads = %v, want one read of [%s %s]", store.exactIDs, y.ID, x.ID)
	}
}

func TestMembersBatchAnswersNothingForFewerThanTwoConvoys(t *testing.T) {
	mem := beads.NewMemStore()
	c := mustCreate(t, mem, beads.Bead{Title: "only", Type: "convoy"})
	wisp := mustCreate(t, mem, beads.Bead{Title: "wisp", Type: "convoy", Ephemeral: true})
	store := &batchReaderAdapter{MemStore: mem}

	for name, convoys := range map[string][]beads.Bead{
		"none":                   nil,
		"one":                    {c},
		"one plus an ephemeral":  {c, wisp},
		"the same convoy twice":  {c, c},
		"only ephemeral convoys": {wisp, wisp},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := MembersBatch(store.readers(), convoys, true)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("answered = %v, want an empty non-nil map", got)
			}
		})
	}
	if store.depCalls+store.probeCalls+store.exactCalls != 0 {
		t.Fatalf("readers were called (dep=%d probe=%d exact=%d), want none", store.depCalls, store.probeCalls, store.exactCalls)
	}
}

func TestMembersBatchAnswersNothingWithoutEveryReader(t *testing.T) {
	fx := newMembersBatchFixture(t)
	full := fx.store.readers()
	var typedNil *batchReaderAdapter
	for name, readers := range map[string]MemberBatchReaders{
		"no deps":          {Exact: full.Exact, Children: full.Children},
		"no exact":         {Deps: full.Deps, Children: full.Children},
		"no children":      {Deps: full.Deps, Exact: full.Exact},
		"typed nil reader": {Deps: typedNil, Exact: full.Exact, Children: full.Children},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := MembersBatch(readers, fx.convoys, true)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("answered = %v, want an empty non-nil map", got)
			}
		})
	}
}

func TestMembersBatchReadFailureAnswersNothing(t *testing.T) {
	boom := errors.New("boom")
	for name, set := range map[string]func(*batchReaderAdapter){
		"probe":             func(a *batchReaderAdapter) { a.probeErr = boom },
		"probe unsupported": func(a *batchReaderAdapter) { a.probeErr = beads.ErrChildPresenceUnsupported },
		"deps":              func(a *batchReaderAdapter) { a.depErr = boom },
		"deps unsupported":  func(a *batchReaderAdapter) { a.depErr = beads.ErrDepListBatchUnsupported },
		"exact":             func(a *batchReaderAdapter) { a.exactErr = boom },
		"exact unsupported": func(a *batchReaderAdapter) { a.exactErr = beads.ErrExactBatchGetUnsupported },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newMembersBatchFixture(t)
			set(fx.store)
			got, err := MembersBatch(fx.store.readers(), fx.convoys, true)
			if err == nil {
				t.Fatalf("err = nil, answered = %v; want the read failure", got)
			}
			if got != nil {
				t.Fatalf("answered = %v, want nil on failure", got)
			}
		})
	}
}

func TestMembersBatchChunksExactReads(t *testing.T) {
	mem := beads.NewMemStore()
	a := mustCreate(t, mem, beads.Bead{Title: "a", Type: "convoy"})
	b := mustCreate(t, mem, beads.Bead{Title: "b", Type: "convoy"})
	for i := 0; i < 2*memberBatchExactChunk+1; i++ {
		item := mustCreate(t, mem, beads.Bead{Title: fmt.Sprintf("item %d", i)})
		owner := a.ID
		if i%2 == 1 {
			owner = b.ID
		}
		mustDep(t, mem, owner, item.ID, TrackingDepType)
	}
	store := &batchReaderAdapter{MemStore: mem}

	got, err := MembersBatch(store.readers(), []beads.Bead{a, b}, true)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, ids := range store.exactIDs {
		sizes = append(sizes, len(ids))
	}
	if want := []int{memberBatchExactChunk, memberBatchExactChunk, 1}; !reflect.DeepEqual(sizes, want) {
		t.Fatalf("exact read sizes = %v, want %v", sizes, want)
	}
	for _, id := range []string{a.ID, b.ID} {
		want, err := Members(mem, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Fatalf("convoy %s: batch has %d members, Members() has %d", id, len(got[id]), len(want))
		}
	}
}
