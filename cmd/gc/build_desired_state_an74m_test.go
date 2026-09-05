package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// an-74m regression bar (a).
//
// The specimen is the standard human-gate claim park, as observed on an-qaz /
// pr-7crj: `status=deferred` upstream, **no `defer_until`**, assignee kept. It
// reaches the controller through `mapBdStatus`, whose catch-all default folds
// `deferred` to `open`, so by the time the supervisor sees it:
//
//	Status     == "open"   (mapped — the raw status is gone)
//	DeferUntil == nil      (dateless park — beads.IsDeferred is structurally blind)
//
// Both a status filter and a date filter therefore miss it, it is counted as
// live assigned work, and a poolDesired=1 singleton with two claimants cycles
// its fresh-mode session between them — stripping the live sibling's claim on
// every supervisor pass.
//
// The assertion is on the assigned-work LIST assembly (the append predicates
// feeding filterAssignedWorkBeadsForPoolDemand), NOT on assignedWorkAssigneeSet,
// which only builds the ready-skip assignee set and is too far downstream to
// stop the cycling.
func TestAssignedWorkListExcludesDatelessDeferredPark(t *testing.T) {
	cfg := &config.City{}

	// Sibling: the genuinely live claim that must survive the cycle.
	live := beads.Bead{
		ID:       "an-a0t",
		Status:   "in_progress",
		Assignee: "portharbour-sysadmin__sysadmin-po-0xonh",
	}
	// Specimen: dateless park, still carrying its assignee.
	parked := beads.Bead{
		ID:             "an-qaz",
		Status:         "open", // mapBdStatus already collapsed "deferred" -> "open"
		UpstreamStatus: "deferred",
		DeferUntil:     nil, // dateless: IsDeferred() is false
		Assignee:       "portharbour-sysadmin__sysadmin-po-0xonh",
	}

	var dst []beads.Bead
	var stores []beads.Store
	var storeRefs []string
	seen := map[string]struct{}{}
	readyIDs := map[string]bool{}

	appendInProgressWorkUnique(cfg, &dst, &stores, &storeRefs, readyIDs, []beads.Bead{live}, seen, nil, "")
	appendAssignedUnique(&dst, &stores, &storeRefs, readyIDs, []beads.Bead{parked}, seen, nil, "")

	got := map[string]bool{}
	for _, b := range dst {
		got[b.ID] = true
	}

	if !got["an-a0t"] {
		t.Fatalf("live in_progress sibling dropped from the assigned-work list: %+v", dst)
	}
	if got["an-qaz"] {
		t.Errorf("dateless deferred park counted as live assigned work: %+v", dst)
	}
	// Index alignment is an invariant of this assembly path.
	if len(dst) != len(stores) || len(dst) != len(storeRefs) {
		t.Errorf("aligned slices desynced: beads=%d stores=%d refs=%d", len(dst), len(stores), len(storeRefs))
	}
}

// A park that carries a FUTURE defer_until is the well-formed case, excluded by
// beads.IsDeferred (the primary C-hygiene signal) rather than by the raw-status
// safety net.
func TestAssignedWorkListExcludesFutureDatedPark(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	parked := beads.Bead{
		ID:         "an-dated",
		Status:     "open",
		DeferUntil: &future,
		Assignee:   "someone",
	}

	var dst []beads.Bead
	var stores []beads.Store
	var storeRefs []string
	appendAssignedUnique(&dst, &stores, &storeRefs, map[string]bool{}, []beads.Bead{parked}, map[string]struct{}{}, nil, "")

	for _, b := range dst {
		if b.ID == "an-dated" {
			t.Fatalf("future-dated park counted as live assigned work: %+v", dst)
		}
	}
}

// Amendment 2: a LAPSED park is live work again. beads.IsDeferred already
// encodes this (a past date returns false); the raw-status net must not
// override it into a permanent park.
func TestAssignedWorkListKeepsLapsedPark(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour)
	lapsed := beads.Bead{
		ID:     "an-lapsed",
		Status: "open",
		// A lapsed park keeps status=deferred upstream — bd never reopens it on
		// its own — so the raw-status net alone would hold it parked forever.
		// When a date IS present it is authoritative: the raw status only decides
		// the dateless case.
		UpstreamStatus: "deferred",
		DeferUntil:     &past,
		Assignee:       "someone",
	}

	var dst []beads.Bead
	var stores []beads.Store
	var storeRefs []string
	appendAssignedUnique(&dst, &stores, &storeRefs, map[string]bool{}, []beads.Bead{lapsed}, map[string]struct{}{}, nil, "")

	found := false
	for _, b := range dst {
		if b.ID == "an-lapsed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lapsed park must resurface as live work: %+v", dst)
	}
}
