package main

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// convoyListStoreReadConcurrency bounds how many convoy stores a fan-out read
// queries at once. Each store is still read by one goroutine at a time; the
// bound only overlaps the subprocess latency of different stores.
const convoyListStoreReadConcurrency = 4

// convoyChildKey names one convoy of one store view. Ids are unique only
// within a store, so the view index is part of the key: a city and a rig that
// both mint "gc-1" hold two different convoys.
type convoyChildKey struct {
	view int
	id   string
}

// convoyListPrefetchChildren answers `gc convoy list`'s per-convoy member
// reads in batches before the list is rendered. It is a variable so tests can
// set it to nil and render the same list through the per-convoy path alone.
var convoyListPrefetchChildren = prefetchConvoyChildren

// prefetchConvoyChildren reads the members of every listed convoy it can,
// with a fixed number of batched reads per store instead of three or more per
// convoy, and returns them keyed by view and convoy id.
//
// A convoy missing from the result is one the batch could not prove it
// answers exactly the way Members does (see convoycore.MembersBatch); the
// caller reads it per convoy as before, so the rendered list is the same
// either way. A batch read that fails is not an error here, for the same
// reason: every convoy of that store just takes the per-convoy path, which
// reports its own errors exactly as it always has.
func prefetchConvoyChildren(stores []convoyStoreView, convoys []convoyWithStore, stderr io.Writer) map[convoyChildKey][]beads.Bead {
	byView := make(map[int][]beads.Bead)
	storeOf := make(map[int]beads.Store)
	for _, c := range convoys {
		byView[c.view] = append(byView[c.view], c.bead)
		storeOf[c.view] = c.store
	}
	views := make([]int, 0, len(byView))
	for view := range byView {
		views = append(views, view)
	}
	sort.Ints(views)

	answered := make([]map[string][]beads.Bead, len(views))
	errs := make([]error, len(views))
	readers := make([]convoycore.MemberBatchReaders, len(views))
	viewStores := make([]beads.Store, len(views))
	for k, view := range views {
		viewStores[k] = storeOf[view]
		readers[k] = convoyMemberBatchReaders(viewStores[k])
	}
	forEachConvoyStore(viewStores, func(k int) {
		answered[k], errs[k] = convoycore.MembersBatch(readers[k], byView[views[k]], true)
	})

	out := make(map[convoyChildKey][]beads.Bead)
	for k, view := range views {
		for id, members := range answered[k] {
			out[convoyChildKey{view: view, id: id}] = members
		}
		// Only a store that can batch at all reports, so a store without the
		// capabilities keeps the stderr it has always had.
		if gcDebugEnabled() && readers[k].Complete() {
			path := ""
			if view < len(stores) {
				path = stores[view].path
			}
			reason := ""
			if errs[k] != nil {
				reason = fmt.Sprintf(" (%v)", errs[k])
			}
			fmt.Fprintf(stderr, "convoy list: store %s: batched %d, fallback %d%s\n", path, len(answered[k]), len(byView[view])-len(answered[k]), reason) //nolint:errcheck // best-effort stderr
		}
	}
	return out
}

// convoyMemberBatchReaders returns the batched reads behind store, leaving a
// field nil when the store cannot answer it (MembersBatch then answers
// nothing for that store).
//
// The edge read and the child-presence probe go to the store UNDER the bead
// policy wrapper, which does not forward either capability. That is sound
// because the policy layer shapes how beads are created and which tier a List
// reads; it says nothing about edges — DepList is promoted from the embedded
// store, so the per-convoy path reads the very same edges — and the probe only
// decides whether the exact, policy-shaped child List runs at all. The exact
// read goes through the wrapper, which forwards it, exactly as Get does.
func convoyMemberBatchReaders(store beads.Store) convoycore.MemberBatchReaders {
	var r convoycore.MemberBatchReaders
	if store == nil {
		return r
	}
	inner, _, _ := unwrapBeadPolicyStore(store)
	if deps, ok := beads.DepListBatchFor(inner); ok {
		r.Deps = deps
	}
	if exact, ok := store.(beads.ExactBatchGetter); ok {
		r.Exact = exact
	}
	if children, ok := inner.(beads.ChildPresenceProber); ok {
		r.Children = children
	}
	return r
}

// forEachConvoyStore runs fn(i) for every index of stores, overlapping
// different stores up to convoyListStoreReadConcurrency at a time.
//
// Indexes whose stores are the same handle run in one goroutine, one after
// the other, so no store is ever read by two goroutines at once. fn must only
// write to state owned by its own index.
func forEachConvoyStore(stores []beads.Store, fn func(i int)) {
	groups := convoyStoreHandleGroups(stores)
	if len(groups) <= 1 {
		for _, group := range groups {
			for _, i := range group {
				fn(i)
			}
		}
		return
	}
	sem := make(chan struct{}, convoyListStoreReadConcurrency)
	var wg sync.WaitGroup
	for _, group := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(group []int) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, i := range group {
				fn(i)
			}
		}(group)
	}
	wg.Wait()
}

// convoyStoreHandleGroups partitions the indexes of stores so that indexes
// holding the same store handle share a group. Handles that cannot be
// compared are treated as distinct, which is only ever unsafe for a store
// that is both non-comparable and handed out twice; every production opener
// returns a pointer store.
func convoyStoreHandleGroups(stores []beads.Store) [][]int {
	var groups [][]int
	for i, store := range stores {
		placed := false
		for g := range groups {
			if sameConvoyStoreHandle(stores[groups[g][0]], store) {
				groups[g] = append(groups[g], i)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []int{i})
		}
	}
	return groups
}

func sameConvoyStoreHandle(a, b beads.Store) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() || !av.Comparable() || !bv.Comparable() {
		return false
	}
	return a == b
}

// convoyListChildren returns the members of c for `gc convoy list`: the
// prefetched answer when the batch produced one, otherwise the per-convoy
// read every convoy took before the batch existed.
func convoyListChildren(prefetched map[convoyChildKey][]beads.Bead, c convoyWithStore) ([]beads.Bead, error) {
	if children, ok := prefetched[convoyChildKey{view: c.view, id: c.bead.ID}]; ok {
		return children, nil
	}
	return listConvoyChildren(c.store, c.bead.ID, true)
}
