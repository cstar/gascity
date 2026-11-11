package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// deferredFoldingBacking is a work-store backing shaped like bd over a ledger
// holding deferred beads: a status-filtered list (`bd list --status open`)
// leaves out the beads whose native status is deferred, while a full scan
// returns them folded to "open" (mapBdStatus). A CachingStore primed over it
// therefore holds rows its own live open list does not return. Get is counted:
// it is the per-row `bd show` the seat work index must not pay for them.
type deferredFoldingBacking struct {
	*beads.MemStore
	mu       sync.Mutex
	deferred map[string]bool
	gets     atomic.Int64
}

func newDeferredFoldingBacking() *deferredFoldingBacking {
	return &deferredFoldingBacking{MemStore: beads.NewMemStore(), deferred: make(map[string]bool)}
}

// addDeferred stores an unassigned bead whose native status is deferred.
func (b *deferredFoldingBacking) addDeferred(t *testing.T) {
	t.Helper()
	bead, err := b.Create(beads.Bead{Title: "parked", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create deferred bead: %v", err)
	}
	b.mu.Lock()
	b.deferred[bead.ID] = true
	b.mu.Unlock()
}

func (b *deferredFoldingBacking) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := b.MemStore.List(q)
	if err != nil || q.Status != "open" {
		return rows, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := rows[:0]
	for _, row := range rows {
		if !b.deferred[row.ID] {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

func (b *deferredFoldingBacking) Get(id string) (beads.Bead, error) {
	b.gets.Add(1)
	return b.MemStore.Get(id)
}

// The seat work index reads its legs live from the backing, not through the
// cache's live-list refresh. CachingStore.List(Live) re-read with one backing
// Get every cached row the backing's status-filtered list left out, which on bd
// is every deferred or blocked bead, on every tick: 278 bd show per two ticks
// on the live supervisor (ga-yhp). A question with no hit must cost no Get, and
// a hit exactly its one live re-read.
//
// The leg is the primed CachingStore in each wrapper a tick can hand the index:
// bare, under the bead-policy store the controller wraps its stores in, and
// under the work class view (which has no Handles of its own).
func TestSeatWorkIndex_ReadsLegsWithoutPerRowGets(t *testing.T) {
	const parked = 5
	shapes := []struct {
		name string
		wrap func(cs *beads.CachingStore, cfg *config.City) beads.Store
	}{
		{name: "caching store", wrap: func(cs *beads.CachingStore, _ *config.City) beads.Store { return cs }},
		{name: "bead-policy store", wrap: func(cs *beads.CachingStore, cfg *config.City) beads.Store { return wrapStoreWithBeadPolicies(cs, cfg) }},
		{name: "work class view", wrap: func(cs *beads.CachingStore, _ *config.City) beads.Store { return beads.WorkStore{Store: cs} }},
	}
	for _, shape := range shapes {
		for _, withHit := range []bool{false, true} {
			name := shape.name + "/no hit"
			if withHit {
				name = shape.name + "/one hit"
			}
			t.Run(name, func(t *testing.T) {
				cityPath := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}}
				backing := newDeferredFoldingBacking()
				for i := 0; i < parked; i++ {
					backing.addDeferred(t)
				}
				const seat = "kill-town--worker-1"
				if withHit {
					if _, err := backing.Create(beads.Bead{Title: "claim", Type: "task", Status: "open", Assignee: seat}); err != nil {
						t.Fatalf("create claim: %v", err)
					}
				}
				cs := beads.NewCachingStore(backing, nil)
				if err := cs.Prime(context.Background()); err != nil {
					t.Fatalf("prime: %v", err)
				}
				// PREMISE: the cache holds the deferred rows as open and the
				// backing's open list omits them, so the old live-list refresh had
				// rows to re-read.
				cached, err := cs.List(beads.ListQuery{Status: "open"})
				if err != nil {
					t.Fatalf("cached open list: %v", err)
				}
				live, err := backing.List(beads.ListQuery{Status: "open"})
				if err != nil {
					t.Fatalf("backing open list: %v", err)
				}
				if len(cached)-len(live) != parked {
					t.Fatalf("premise: cache holds %d open rows, backing lists %d; want %d folded deferred rows apart", len(cached), len(live), parked)
				}

				sessions := beads.NewMemStore()
				registerWorkShapeWith(t, cityPath, "split", sessions, shape.wrap(cs, cfg))
				useSeatWorkPath(t, "index", cityPath, cfg, sessions, nil)
				backing.gets.Store(0)

				has, err := seatHasWork(cityPath, sessions, seatWorkQuery{ids: []string{seat}, statuses: seatWorkStatuses}, func() (bool, error) {
					t.Fatal("the gate took its live probe; the tick's index was not consulted")
					return false, nil
				})
				if err != nil {
					t.Fatalf("seatHasWork: %v", err)
				}
				if has != withHit {
					t.Fatalf("seatHasWork = %v, want %v", has, withHit)
				}
				want := int64(0)
				if withHit {
					want = 1 // the hit's live re-read
				}
				if got := backing.gets.Load(); got != want {
					t.Fatalf("backing Get calls = %d, want %d; the index re-read the %d deferred rows one by one", got, want, parked)
				}
			})
		}
	}
}
