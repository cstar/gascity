package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdByIDConditionalDeferralPreservesOwnershipAndDeadline(t *testing.T) {
	cityPath, store := foreignProviderCity(t)
	bead := mustCreateClassBead(t, store, beads.Bead{Title: "capacity wait", Type: "task", Status: "in_progress", Assignee: "worker"})
	deadline := time.Now().UTC().Add(3 * time.Minute).Truncate(time.Second)
	for _, owner := range []string{"other", "worker"} {
		var out, errout bytes.Buffer
		code, handled := maybeRouteBdByID(cityPath, "", []string{"update", bead.ID, "--if-assignee", owner, "--if-status", "in_progress", "--assignee", "", "--status", "deferred", "--defer", deadline.Format(time.RFC3339), "--json"}, &out, &errout)
		if !handled {
			t.Fatal("native deferral fell through")
		}
		if (code == 0) != (owner == "worker") {
			t.Fatalf("owner %s code %d: %s", owner, code, errout.String())
		}
		got, err := store.Get(bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		if owner == "other" && (got.Assignee != "worker" || got.Status != "in_progress" || got.DeferUntil != nil) {
			t.Fatalf("changed another owner: %+v", got)
		}
		if owner == "worker" && (got.Assignee != "" || got.DeferUntil == nil || !got.DeferUntil.Equal(deadline)) {
			t.Fatalf("deferral not persisted atomically: %+v", got)
		}
	}
}

func TestBdByIDExpiredDeferralReturnsToReady(t *testing.T) {
	cityPath, store := foreignProviderCity(t)
	bead := mustCreateClassBead(t, store, beads.Bead{Title: "expired wait", Type: "task", Status: "in_progress", Assignee: "worker"})
	var out, errout bytes.Buffer
	deadline := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", bead.ID, "--if-assignee", "worker", "--if-status", "in_progress", "--assignee", "", "--status", "deferred", "--defer", deadline, "--json"}, &out, &errout)
	if !handled || code != 0 {
		t.Fatalf("defer: %d %s", code, errout.String())
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range ready {
		if row.ID == bead.ID {
			return
		}
	}
	t.Fatalf("expired task not ready: %+v", ready)
}

func TestBdByIDDeferralOnlyIsWriteAndCannotJoinClaim(t *testing.T) {
	deadline := "2026-10-02T12:00:00Z"
	_, _, ok := parseBdByIDUpdateArgs([]string{"gcg-1", "--defer", deadline})
	if !ok {
		t.Fatal("deadline-only update rejected")
	}
	_, _, ok = parseBdByIDUpdateArgs([]string{"gcg-1", "--claim", "--defer", deadline})
	if ok {
		t.Fatal("claim silently discarded deferral")
	}
	_, _, ok = parseBdByIDUpdateArgs([]string{"gcg-1", "--defer", "invalid"})
	if ok {
		t.Fatal("invalid deadline accepted")
	}
}
