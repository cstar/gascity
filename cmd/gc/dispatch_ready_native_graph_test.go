package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestControlReadyNativeGraphPreservesPostCapCandidate(t *testing.T) {
	store := beads.NewMemStore()
	zero, four := 0, 4
	for i := 0; i < controlReadyFallbackLimit; i++ {
		if _, err := store.Create(beads.Bead{Type: "task", Priority: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	target := "rig/control-dispatcher"
	candidate, err := store.Create(beads.Bead{Type: "task", Assignee: target, Priority: &four})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := controlReadyNativeGraphBindingReady("rig", store, false)
	if err != nil {
		t.Fatal(err)
	}
	chosen := evaluateControlReady(rows, parsedControlReadyQuery{target: target}, nil)
	if len(chosen) != 1 || chosen[0].ID != candidate.ID {
		t.Fatal("graph candidate beyond former cache frontier lost")
	}
	legacy, err := controlReadyBindingReady("rig", store, false)
	if err != nil || len(legacy) != controlReadyFallbackLimit {
		t.Fatalf("legacy binding fallback cap changed: len=%d err=%v", len(legacy), err)
	}
	if oldChosen := evaluateControlReady(legacy, parsedControlReadyQuery{target: target}, nil); len(oldChosen) != 0 {
		t.Fatal("fixture does not reproduce the former graph cap losing the candidate")
	}
}
