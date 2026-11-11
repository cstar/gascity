package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// closeCountingStore is a beads.Store that counts CloseStore calls and poisons
// its read methods after close, so a test can prove how many times a handle was
// closed and that nothing reads it afterward. It embeds *beads.MemStore so the
// dispatch's reads behave like a real in-memory store.
type closeCountingStore struct {
	*beads.MemStore
	closeCount  atomic.Int64
	closed      atomic.Bool
	getNotFound bool // when true, Get always answers ErrNotFound (dispatch error path)
}

func newCloseCountingStore() *closeCountingStore {
	return &closeCountingStore{MemStore: beads.NewMemStore()}
}

func (s *closeCountingStore) CloseStore() error { //nolint:unparam // must satisfy the CloseStore() error store interface closeBeadStoreHandle asserts
	s.closeCount.Add(1)
	s.closed.Store(true)
	return nil
}

func (s *closeCountingStore) closes() int64 { return s.closeCount.Load() }

// List poisons after close so a stray read from a closed handle surfaces loudly.
func (s *closeCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("closeCountingStore.List called after CloseStore (use-after-close)")
	}
	return s.MemStore.List(q)
}

// Get either forces the not-found dispatch path or poisons after close.
func (s *closeCountingStore) Get(id string) (beads.Bead, error) {
	if s.getNotFound {
		return beads.Bead{}, beads.ErrNotFound
	}
	if s.closed.Load() {
		return beads.Bead{}, fmt.Errorf("closeCountingStore.Get called after CloseStore (use-after-close)")
	}
	return s.MemStore.Get(id)
}

// TestRunControlDispatcherInStoreClosesScopeStoreOnError pins the second leak
// site: runControlDispatcherInStore must close the scope store it opened even
// when the dispatch fails before completing.
func TestRunControlDispatcherInStoreClosesScopeStoreOnError(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	fake := newCloseCountingStore()
	fake.getNotFound = true // controlBeadLedger.Get -> ErrNotFound -> dispatch returns an error

	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return fake, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	var stdout, stderr bytes.Buffer
	err := runControlDispatcherInStore(cityDir, cityDir, "ga-missing-control", &stdout, &stderr)
	if err == nil {
		t.Fatalf("runControlDispatcherInStore: err = nil, want an error for a missing control bead (stderr=%q)", stderr.String())
	}
	if got := fake.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the dispatch error path must not leak the opened store", got)
	}
}

// TestRunControlDispatcherInStoreClosesScopeStoreOnSuccess pins the same leak
// site on the path the serve loop actually takes. Both paths share one
// unconditional defer today, so the error-path sibling above would stay green
// under a restructure that closed only inside the error branch — while every
// successful dispatch leaked a store again, which is the per-bead leak this
// file exists to prevent.
func TestRunControlDispatcherInStoreClosesScopeStoreOnSuccess(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	fake := newCloseCountingStore()
	// An orphaned scope-check is the cheapest control bead that dispatches all
	// the way to a processed result: gc.root_bead_id names a root the store
	// does not hold, so ProcessControl closes the control bead and reports
	// Processed without an error, and scope-check needs no city-config
	// resolution. Status is forced to "open" by Create, which is what keeps
	// this off ProcessControl's not-open skip.
	control, err := fake.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindScopeCheck,
			beadmeta.RootBeadIDMetadataKey:   "ga-missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("seed control bead: %v", err)
	}

	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return fake, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherInStore(cityDir, cityDir, control.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcherInStore: %v (stderr=%q)", err, stderr.String())
	}
	// Assert the dispatch actually reached the processed branch. Without this
	// the test would still pass if the bead stopped qualifying and ProcessControl
	// returned a nil error from an early skip, which would silently stop
	// covering the success path it is named for.
	if !strings.Contains(stdout.String(), "action=orphaned-workflow") {
		t.Fatalf("stdout = %q, want a processed control dispatch (stderr=%q)", stdout.String(), stderr.String())
	}
	if got := fake.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the dispatch success path must not leak the opened store", got)
	}
}
