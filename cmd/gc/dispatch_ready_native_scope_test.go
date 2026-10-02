package main

import (
	"context"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestControlReadyNativeScopePreservesCandidateBeyondCLICap(t *testing.T) {
	target := "rig/control-dispatcher"
	rows := make([]beads.Bead, controlReadyFallbackLimit+1)
	for i := range rows {
		rows[i] = beads.Bead{ID: strconv.Itoa(i), Type: "task"}
	}
	rows[len(rows)-1].Assignee = target
	reader := controlReadyNativeScopeReader(func(_ context.Context, _ bool, limit int) ([]beads.Bead, error) {
		if limit > 0 && len(rows) > limit {
			return rows[:limit], nil
		}
		return rows, nil
	}, func() error { return nil })
	frontier, err := reader.Ready(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	chosen := evaluateControlReady(frontier, parsedControlReadyQuery{target: target}, nil)
	if len(chosen) != 1 || chosen[0].ID != rows[len(rows)-1].ID {
		t.Fatal("candidate beyond old shell cap lost before assignee filtering")
	}
}
