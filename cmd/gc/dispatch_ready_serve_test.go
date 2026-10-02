package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestControlReadyServeUsesLiveScopeInsteadOfWarmCache(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	target := "gascity/control-dispatcher"
	if _, err := store.Create(beads.Bead{Assignee: target, Type: "task"}); err != nil {
		t.Fatal(err)
	}
	previous := controlReadyServeScopeRead
	t.Cleanup(func() { controlReadyServeScopeRead = previous })
	calls := 0
	controlReadyServeScopeRead = func(dir, city string, _ map[string]string, include bool) ([]beads.Bead, bool, error) {
		calls++
		if dir != cityDir || city != cityDir || include {
			t.Fatal("wrong scope or tier")
		}
		id := "live-first"
		if calls > 1 {
			id = "live-next"
		}
		return []beads.Bead{{ID: id, Assignee: target, Type: "task"}}, true, nil
	}
	noBDOnPathForTest(t)
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	for _, id := range []string{"live-first", "live-next"} {
		queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
		if err != nil || !handled || len(queue) != 1 || queue[0].ID != id {
			t.Fatalf("persistent scope bypassed: queue=%v handled=%v err=%v", queue, handled, err)
		}
	}
	if calls != 2 {
		t.Fatalf("live scans=%d", calls)
	}
}

func TestControlReadyServeScopeFailureDoesNotReturnPartialQueue(t *testing.T) {
	cityDir, _ := setUpControlReadyFileStoreCity(t)
	previous := controlReadyServeScopeRead
	t.Cleanup(func() { controlReadyServeScopeRead = previous })
	failure := errors.New("native ready failed")
	controlReadyServeScopeRead = func(string, string, map[string]string, bool) ([]beads.Bead, bool, error) {
		return []beads.Bead{{ID: "partial"}}, true, failure
	}
	queue, err := controlReadyFallbackReady(cityDir, cityDir, nil, false)
	if queue != nil || !errors.Is(err, failure) {
		t.Fatalf("read failure masked: queue=%v err=%v", queue, err)
	}
}

func TestControlReadyServeLifetimeClosesOwnedReadersAndRestoresPrevious(t *testing.T) {
	previousFactory := controlReadyServeReaderFactory
	previousRead := controlReadyServeScopeRead
	t.Cleanup(func() { controlReadyServeReaderFactory = previousFactory; controlReadyServeScopeRead = previousRead })
	fixture := &readyReaderFixture{}
	outerCalls := 0
	controlReadyServeScopeRead = func(string, string, map[string]string, bool) ([]beads.Bead, bool, error) {
		outerCalls++
		return nil, false, nil
	}
	factoryCalls := 0
	controlReadyServeReaderFactory = func() (*controlReadyReaders, func(string, string, map[string]string, bool) ([]beads.Bead, bool, error)) {
		factoryCalls++
		registry := &controlReadyReaders{entries: map[string]controlReadyReaderEntry{"rig": {reader: fixture.reader()}}}
		return registry, func(string, string, map[string]string, bool) ([]beads.Bead, bool, error) {
			rows, err := fixture.Ready(t.Context(), false)
			return rows, true, err
		}
	}
	failure := errors.New("serve stopped")
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	err := withControlReadyServeReaders(query, func() error {
		if _, handled, err := controlReadyServeScopeRead("rig", "city", nil, false); !handled || err != nil {
			t.Fatal("owned reader not installed")
		}
		return failure
	})
	if !errors.Is(err, failure) || factoryCalls != 1 || fixture.closes != 1 || fixture.calls != 1 {
		t.Fatalf("lifetime leak: factories=%d closes=%d reads=%d err=%v", factoryCalls, fixture.closes, fixture.calls, err)
	}
	_, _, _ = controlReadyServeScopeRead("rig", "city", nil, false)
	if outerCalls != 1 {
		t.Fatal("previous reader not restored")
	}
	if err := withControlReadyServeReaders("custom command", func() error { return nil }); err != nil || factoryCalls != 1 {
		t.Fatal("custom query acquired native readers")
	}
}

func TestControlReadyServeIneligibleScopeKeepsExistingCache(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	target := "gascity/control-dispatcher"
	seed, err := store.Create(beads.Bead{Assignee: target, Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	previous := controlReadyServeScopeRead
	t.Cleanup(func() { controlReadyServeScopeRead = previous })
	controlReadyServeScopeRead = nil
	noBDOnPathForTest(t)
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	if _, _, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil); err != nil {
		t.Fatal(err)
	}
	controlReadyServeScopeRead = func(string, string, map[string]string, bool) ([]beads.Bead, bool, error) { return nil, false, nil }
	queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
	if err != nil || !handled || len(queue) != 1 || queue[0].ID != seed.ID {
		t.Fatalf("ineligible scope lost existing cache: queue=%v handled=%v err=%v", queue, handled, err)
	}
}

func TestCaptureControlReadyServeOpeningPreservesExplicitLedger(t *testing.T) {
	cityDir, _ := setUpControlReadyFileStoreCity(t)
	other := t.TempDir()
	snapshot, err := captureControlReadyServeOpening(t.Context(), cityDir, cityDir, map[string]string{"BEADS_DIR": other})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Env["BEADS_DIR"] != other || snapshot.FallbackReason == "" {
		t.Fatal("explicit CLI ledger overwritten")
	}
}

func TestCaptureControlReadyServeOpeningMatchesCLIAmbientCleanup(t *testing.T) {
	cityDir, _ := setUpControlReadyFileStoreCity(t)
	other := t.TempDir()
	t.Setenv("BEADS_DIR", other)
	snapshot, err := captureControlReadyServeOpening(t.Context(), cityDir, cityDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := envListValue(mergeRuntimeEnv(beads.ProcessEnvSnapshotExcludingNativeDoltOpen(), nil), "BEADS_DIR"); got != "" {
		t.Fatal("fixture no longer matches the CLI namespace cleanup")
	}
	if snapshot.Env["BEADS_DIR"] != filepath.Join(cityDir, ".beads") {
		t.Fatal("SDK scope differs from the cleaned CLI discovery scope")
	}
}
