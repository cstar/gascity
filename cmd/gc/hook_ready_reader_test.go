package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestHookReadyReaderReusesCandidatesButEnrichesOnlySelectedRows(t *testing.T) {
	store := &hookReadyDependencyRecorder{Store: beads.NewMemStore()}
	rows := []beads.Bead{{ID: "other", Status: "in_progress", Assignee: "other"}, {ID: "owned", Status: "in_progress", Assignee: "session"}}
	reads := 0
	reader := newHookReadyReader(func(status string) ([]beads.Bead, map[string]readyLeg, error) {
		reads++
		if status != readyStatusInProgress {
			t.Fatalf("unexpected status %q", status)
		}
		return rows, map[string]readyLeg{"owned": readyTestLeg("graph", store), "other": readyTestLeg("graph", store)}, nil
	})
	for _, identity := range []string{"missing-id", "session", "missing-alias"} {
		got, err := reader.query(readyOpts{status: readyStatusInProgress, assignee: identity, limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if identity == "session" && (len(got) != 1 || got[0].ID != "owned") {
			t.Fatalf("got %+v", got)
		}
	}
	if reads != 1 {
		t.Fatalf("repeated federated reads: %d", reads)
	}
	if len(store.deps) != 1 || store.deps[0] != "owned" {
		t.Fatalf("enriched unselected rows: %v", store.deps)
	}
}

type hookReadyDependencyRecorder struct {
	beads.Store
	deps []string
}

func (s *hookReadyDependencyRecorder) DepList(id, _ string) ([]beads.Dep, error) {
	s.deps = append(s.deps, id)
	return nil, nil
}

func TestHookReadyReaderKeepsReadyReadLazyAndErrorsLoud(t *testing.T) {
	reads := 0
	reader := newHookReadyReader(func(status string) ([]beads.Bead, map[string]readyLeg, error) {
		reads++
		if status == "" {
			return nil, nil, errors.New("rig unavailable")
		}
		return []beads.Bead{{ID: "owned", Status: "in_progress", Assignee: "session"}}, nil, nil
	})
	if _, err := reader.query(readyOpts{status: readyStatusInProgress, assignee: "session"}); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("eager read: %d", reads)
	}
	if _, err := reader.query(readyOpts{}); err == nil {
		t.Fatal("ready failure became no work")
	}
}

func TestNativeHookReaderDoesNotAdaptCustomOrSingleStoreQueries(t *testing.T) {
	for _, tc := range []struct {
		query    string
		agent    config.Agent
		topology config.QueryTopology
	}{
		{query: "custom arbitrary command", agent: config.Agent{WorkQuery: "custom arbitrary command"}, topology: config.QueryTopology{FederatedReady: true}},
		{query: "single store command", agent: config.Agent{}, topology: config.QueryTopology{}},
	} {
		got, cleanup, err := prepareNativeHookReadyQuery(tc.query, "", &tc.agent, tc.topology)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if got != tc.query {
			t.Fatalf("adapted caller-owned query: %q", got)
		}
	}
}

func TestHookReaderDoesNotCarryCandidatesAcrossInvocations(t *testing.T) {
	reads := 0
	read := func(string) ([]beads.Bead, map[string]readyLeg, error) {
		reads++
		return []beads.Bead{{ID: "task", Assignee: fmt.Sprint(reads)}}, nil, nil
	}
	for i := 1; i <= 2; i++ {
		got, err := newHookReadyReader(read).query(readyOpts{status: readyStatusInProgress, assignee: fmt.Sprint(i)})
		if err != nil || len(got) != 1 {
			t.Fatalf("stale ownership across invocation: %+v %v", got, err)
		}
	}
	if reads != 2 {
		t.Fatalf("new invocation reused old read: %d", reads)
	}
}
