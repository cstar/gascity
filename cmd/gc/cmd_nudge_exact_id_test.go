package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type nudgeReadCountingStore struct {
	beads.Store
	lists int
	gets  int
}

func (s *nudgeReadCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}
func (s *nudgeReadCountingStore) Get(id string) (beads.Bead, error) { s.gets++; return s.Store.Get(id) }

// A poller with a stable session ID must not query session-name metadata on
// every liveness observation, and must still reject a reused runtime name.
func TestNudgeObservationKnownIDDoesNotSearchSessions(t *testing.T) {
	for _, reused := range []bool{false, true} {
		name := "current"
		if reused {
			name = "reused"
		}
		t.Run(name, func(t *testing.T) {
			store := &nudgeReadCountingStore{Store: beads.NewMemStore()}
			b, err := store.Create(beads.Bead{Title: "worker", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{"session_name": "sess-worker", "provider": "codex", "continuation_epoch": "1"}})
			if err != nil {
				t.Fatal(err)
			}
			fake := runtime.NewFake()
			if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			liveID := b.ID
			if reused {
				liveID = "replacement-session"
			}
			if err := fake.SetMeta("sess-worker", "GC_SESSION_ID", liveID); err != nil {
				t.Fatal(err)
			}
			target := nudgeTarget{sessionID: b.ID, sessionName: "sess-worker", agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "codex"}}
			obs, err := workerObserveNudgeTarget(target, store, fake)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Running == reused {
				t.Fatalf("Running=%v, reused=%v", obs.Running, reused)
			}
			if store.gets > 2 {
				t.Fatalf("session gets=%d, want at most 2", store.gets)
			}
			if store.lists != 0 {
				t.Fatalf("session list reads=%d, want0 with known identity (gets=%d)", store.lists, store.gets)
			}
		})
	}
}

func TestNudgeObservationMissingIDFallsBackToRuntimeName(t *testing.T) {
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	target := nudgeTarget{sessionID: "missing-session", sessionName: "sess-worker", agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "codex"}}
	obs, err := workerObserveNudgeTarget(target, store, fake)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Running {
		t.Fatal("live runtime must remain observable when the session record is absent")
	}
}
