package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"io"
	"testing"
	"time"
)

func TestOrderTrackingWatchdogColdScopes(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "hot", Path: "/tmp/hot"}, {Name: "cold", Path: "/tmp/cold", SuspendedOnStart: true}}}
	var state suspensionstate.State
	check := func(full bool, want int) {
		t.Helper()
		got := orderTrackingWatchdogTargets("/tmp/city", cfg, state, full)
		if len(got) != want {
			t.Fatalf("full=%v got %d targets, want %d", full, len(got), want)
		}
		if got[0].target.ScopeKind != "city" {
			t.Fatal("city recovery must never be skipped")
		}
	}
	check(false, 2)
	check(true, 3)
	resumed := false
	suspensionstate.SetRig(&state, "cold", &resumed)
	check(false, 3)
	suspended := true
	suspensionstate.SetRig(&state, "hot", &suspended)
	check(false, 2)
	check(true, 3)
}

func TestOrderTrackingWatchdogSuspendedCleanupIsBounded(t *testing.T) {
	city := beads.NewMemStore()
	cold := beads.NewMemStore()
	row, err := cold.Create(beads.Bead{Title: "order:probe", Labels: []string{"order-run:probe", labelOrderTracking}})
	if err != nil {
		t.Fatal(err)
	}
	now := row.CreatedAt.Add(orderTrackingSweepWatchdogStaleAfter + time.Minute)
	cr := &CityRuntime{cityPath: t.TempDir(), cfg: &config.City{Rigs: []config.Rig{{Name: "cold", Path: "/tmp/cold", SuspendedOnStart: true}}}, standaloneCityStore: city, standaloneRigStores: map[string]beads.Store{"cold": cold}, stderr: io.Discard, stdout: io.Discard, orderSweepColdLast: now}
	cr.runOrderTrackingSweepWatchdog(now)
	got, _ := cold.Get(row.ID)
	if got.Status != "open" {
		t.Fatal("cold rig swept at hot cadence")
	}
	cr.runOrderTrackingSweepWatchdog(now.Add(orderTrackingColdSweepInterval))
	got, _ = cold.Get(row.ID)
	if got.Status != "closed" {
		t.Fatal("suspended rig never cleaned")
	}
}
