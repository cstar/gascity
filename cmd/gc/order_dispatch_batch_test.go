package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// recordingBatchStore wraps a real store and records how label writes arrive:
// directly via Update, or batched via Tx. ADR-0018 Layer 3 batches DEFERRABLE
// outcome-label writes, so these counters let tests prove a write was buffered
// (no direct Update) and later flushed (applied via Tx).
type recordingBatchStore struct {
	beads.Store

	mu          sync.Mutex
	updateIDs   []string // ids passed to direct Update (outside Tx)
	txCalls     int      // number of Tx invocations
	txUpdateIDs []string // ids updated inside Tx callbacks
}

func (s *recordingBatchStore) Update(id string, opts beads.UpdateOpts) error {
	s.mu.Lock()
	s.updateIDs = append(s.updateIDs, id)
	s.mu.Unlock()
	return s.Store.Update(id, opts)
}

func (s *recordingBatchStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	s.mu.Lock()
	s.txCalls++
	s.mu.Unlock()
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&recordingBatchTx{parent: s, tx: tx})
	})
}

func (s *recordingBatchStore) directUpdateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updateIDs)
}

func (s *recordingBatchStore) txCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txCalls
}

func (s *recordingBatchStore) txUpdates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.txUpdateIDs...)
}

type recordingBatchTx struct {
	parent *recordingBatchStore
	tx     beads.Tx
}

func (t *recordingBatchTx) Update(id string, opts beads.UpdateOpts) error {
	t.parent.mu.Lock()
	t.parent.txUpdateIDs = append(t.parent.txUpdateIDs, id)
	t.parent.mu.Unlock()
	return t.tx.Update(id, opts)
}

func (t *recordingBatchTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.tx.SetMetadataBatch(id, kvs)
}

func (t *recordingBatchTx) Close(id string) error { return t.tx.Close(id) }

// TestEnqueueLabelUpdateBuffersUntilFlush proves deferrable label writes are
// held in the write-behind buffer (no store write) until flushLabelUpdates,
// which applies them in a single batched Tx.
func TestEnqueueLabelUpdateBuffersUntilFlush(t *testing.T) {
	base := beads.NewMemStore()
	bead, err := base.Create(beads.Bead{Title: "order:test", Labels: []string{"order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingBatchStore{Store: base}
	m := &memoryOrderDispatcher{}

	m.enqueueLabelUpdate(store, bead.ID, beads.UpdateOpts{Labels: []string{"wisp"}})

	// Nothing should have hit the store yet.
	if got := store.directUpdateCount(); got != 0 {
		t.Fatalf("direct Update calls before flush = %d, want 0 (must be buffered)", got)
	}
	if got := store.txCount(); got != 0 {
		t.Fatalf("Tx calls before flush = %d, want 0", got)
	}
	cur, _ := base.Get(bead.ID)
	if hasLabel(cur.Labels, "wisp") {
		t.Fatal("bead has wisp label before flush — write was not buffered")
	}

	m.flushLabelUpdates(store)

	if got := store.txCount(); got != 1 {
		t.Fatalf("Tx calls after flush = %d, want 1 (batched)", got)
	}
	if got := store.directUpdateCount(); got != 0 {
		t.Fatalf("direct Update calls after flush = %d, want 0 (Tx path used)", got)
	}
	cur, _ = base.Get(bead.ID)
	if !hasLabel(cur.Labels, "wisp") {
		t.Fatalf("bead missing wisp label after flush, got %v", cur.Labels)
	}

	// Idempotent: flushing an empty buffer is a no-op.
	m.flushLabelUpdates(store)
	if got := store.txCount(); got != 1 {
		t.Fatalf("Tx calls after empty flush = %d, want 1 (no extra commit)", got)
	}
}

// TestEnqueueLabelUpdateMergesSameBead proves repeated enqueues for one bead id
// collapse into a single buffered Update with merged (deduped) labels and
// merged metadata, so they flush as one batched write per bead.
func TestEnqueueLabelUpdateMergesSameBead(t *testing.T) {
	base := beads.NewMemStore()
	bead, err := base.Create(beads.Bead{Title: "order:test"})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingBatchStore{Store: base}
	m := &memoryOrderDispatcher{}

	m.enqueueLabelUpdate(store, bead.ID, beads.UpdateOpts{
		Labels:   []string{"wisp"},
		Metadata: map[string]string{"k1": "v1"},
	})
	m.enqueueLabelUpdate(store, bead.ID, beads.UpdateOpts{
		Labels:   []string{"wisp", "wisp-failed"}, // "wisp" is a duplicate
		Metadata: map[string]string{"k2": "v2"},
	})

	m.flushLabelUpdates(store)

	// Exactly one Tx, and the bead was updated exactly once inside it.
	if got := store.txCount(); got != 1 {
		t.Fatalf("Tx calls = %d, want 1 (merged into one batch)", got)
	}
	if updates := store.txUpdates(); len(updates) != 1 || updates[0] != bead.ID {
		t.Fatalf("tx updates = %v, want one update for %s (merged per id)", updates, bead.ID)
	}

	cur, _ := base.Get(bead.ID)
	for _, want := range []string{"wisp", "wisp-failed"} {
		if !hasLabel(cur.Labels, want) {
			t.Errorf("merged labels missing %q, got %v", want, cur.Labels)
		}
	}
	// "wisp" must appear exactly once (deduped).
	wispCount := 0
	for _, l := range cur.Labels {
		if l == "wisp" {
			wispCount++
		}
	}
	if wispCount != 1 {
		t.Errorf("label %q appears %d times, want 1 (deduped)", "wisp", wispCount)
	}
	if cur.Metadata["k1"] != "v1" || cur.Metadata["k2"] != "v2" {
		t.Errorf("merged metadata = %v, want k1=v1,k2=v2", cur.Metadata)
	}
}

// TestDispatchExecBuffersOutcomeLabelFlushedOnDrain proves the exec outcome
// label is NOT applied via a synchronous Update during the tick; it is buffered
// and applied via a batched Tx on drain.
func TestDispatchExecBuffersOutcomeLabelFlushedOnDrain(t *testing.T) {
	base := beads.NewMemStore()
	store := &recordingBatchStore{Store: base}

	aa := []orders.Order{{
		Name:     "exec-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m := ad.(*memoryOrderDispatcher)

	m.dispatch(context.Background(), t.TempDir(), time.Now())

	// At this point the tracking bead CREATE happened synchronously, and the
	// dispatchOne goroutine ran (it will close the tracking bead). The exec
	// outcome label must NOT have been written via a direct Update.
	if got := store.directUpdateCount(); got != 0 {
		t.Fatalf("direct Update calls during tick = %d, want 0 (outcome label must be buffered)", got)
	}

	m.drain(context.Background())

	// drain flushes the buffer via a batched Tx.
	if got := store.txCount(); got < 1 {
		t.Fatalf("Tx calls after drain = %d, want >=1 (buffered outcome label flushed)", got)
	}

	// The tracking bead must now carry the "exec" outcome label.
	beadsForOrder := trackingBeads(t, store, "order-run:exec-order")
	var tracking *beads.Bead
	for i := range beadsForOrder {
		if beadsForOrder[i].Title == "order:exec-order" {
			tracking = &beadsForOrder[i]
			break
		}
	}
	if tracking == nil {
		t.Fatalf("no tracking bead found for exec-order; beads=%+v", beadsForOrder)
	}
	if !hasLabel(tracking.Labels, "exec") {
		t.Fatalf("tracking bead missing flushed exec outcome label, got %v", tracking.Labels)
	}
}

// TestDispatchWispRootLabelVisibleNextTickNoDuplicate is the correctness-
// critical test: the wisp-root order-run:* label is written synchronously (not
// buffered), so a second tick's open-work gate sees it and does NOT re-dispatch
// a duplicate wisp.
func TestDispatchWispRootLabelVisibleNextTickNoDuplicate(t *testing.T) {
	store := beads.NewMemStore()

	aa := []orders.Order{{
		Name:         "wisp-order",
		Trigger:      "cooldown",
		Interval:     "1m",
		Formula:      "test-formula",
		Pool:         "worker",
		FormulaLayer: sharedTestFormulaDir,
	}}
	ad := buildOrderDispatcherFromList(aa, store, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	now := time.Now()
	// First tick: dispatch + drain (flushes deferrable labels).
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	work := workBeadByOrderLabel(t, store, "order-run:wisp-order")
	if !slicesContain(work.Labels, "order-run:wisp-order") {
		t.Fatalf("wisp root missing order-run label after first tick, got %v", work.Labels)
	}

	// Second tick a moment later (still inside the cooldown interval is not
	// required; the open-work gate keys off the order-run:* wisp the first tick
	// created). If the root label were buffered and not yet flushed, the gate
	// would miss it and a duplicate wisp would fire.
	ad.dispatch(context.Background(), t.TempDir(), now.Add(time.Second))
	ad.drain(context.Background())

	// Count distinct wisp (work) beads carrying the order-run label — exclude
	// the tracking beads (Title "order:*").
	all := trackingBeads(t, store, "order-run:wisp-order")
	workCount := 0
	for _, b := range all {
		if b.Title != "order:wisp-order" {
			workCount++
		}
	}
	if workCount != 1 {
		t.Fatalf("wisp work beads = %d, want 1 (no duplicate dispatch — root label must be visible by next tick)", workCount)
	}
}

// TestFlushOnShutdownAppliesPendingUpdates proves drain (the controller
// shutdown path) flushes any buffered updates so nothing is lost on stop.
func TestFlushOnShutdownAppliesPendingUpdates(t *testing.T) {
	base := beads.NewMemStore()
	bead, err := base.Create(beads.Bead{Title: "order:test", Labels: []string{"order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingBatchStore{Store: base}
	m := &memoryOrderDispatcher{}

	// Buffer an update with nothing in flight (inflightDone is nil).
	m.enqueueLabelUpdate(store, bead.ID, beads.UpdateOpts{Labels: []string{"wisp-failed"}})

	cur, _ := base.Get(bead.ID)
	if hasLabel(cur.Labels, "wisp-failed") {
		t.Fatal("label applied before drain — should be buffered")
	}

	// drain with nothing in flight must still flush.
	if !m.drain(context.Background()) {
		t.Fatal("drain returned false with nothing in flight")
	}

	cur, _ = base.Get(bead.ID)
	if !hasLabel(cur.Labels, "wisp-failed") {
		t.Fatalf("buffered label not flushed on drain, got %v", cur.Labels)
	}
}

// TestEnqueueLabelUpdateConcurrent stresses the pendingMu discipline: many
// goroutines enqueue concurrently — a mix of writes to one shared bead id and
// to per-goroutine distinct ids — then a single flush must land every label
// with no loss and no duplication. Run under -race to validate the locking.
func TestEnqueueLabelUpdateConcurrent(t *testing.T) {
	base := beads.NewMemStore()
	store := &recordingBatchStore{Store: base}
	m := &memoryOrderDispatcher{}

	const goroutines = 16

	// Create the shared bead plus one distinct bead per goroutine up front so
	// the flush has real targets to update (MemStore assigns the ids).
	sharedBead, err := base.Create(beads.Bead{Title: "shared-target"})
	if err != nil {
		t.Fatal(err)
	}
	distinctIDs := make([]string, goroutines)
	for i := range distinctIDs {
		b, err := base.Create(beads.Bead{Title: fmt.Sprintf("distinct-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		distinctIDs[i] = b.ID
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Each goroutine writes a distinct label to the SHARED bead (so the
			// merge path is exercised concurrently) and a distinct label to its
			// OWN bead (so distinct-id inserts race too).
			m.enqueueLabelUpdate(store, sharedBead.ID, beads.UpdateOpts{
				Labels: []string{fmt.Sprintf("shared-label-%d", g)},
			})
			// Enqueue a duplicate to the shared bead to exercise dedup under
			// concurrency.
			m.enqueueLabelUpdate(store, sharedBead.ID, beads.UpdateOpts{
				Labels: []string{fmt.Sprintf("shared-label-%d", g)},
			})
			m.enqueueLabelUpdate(store, distinctIDs[g], beads.UpdateOpts{
				Labels: []string{fmt.Sprintf("own-label-%d", g)},
			})
		}(g)
	}
	wg.Wait()

	m.flushLabelUpdates(store)

	// Shared bead: every per-goroutine label present, each exactly once.
	shared, _ := base.Get(sharedBead.ID)
	counts := map[string]int{}
	for _, l := range shared.Labels {
		counts[l]++
	}
	for g := 0; g < goroutines; g++ {
		label := fmt.Sprintf("shared-label-%d", g)
		switch counts[label] {
		case 0:
			t.Errorf("shared bead lost label %q (got %v)", label, shared.Labels)
		case 1:
			// ok
		default:
			t.Errorf("shared bead has duplicate label %q x%d (got %v)", label, counts[label], shared.Labels)
		}
	}

	// Each distinct bead has its own label.
	for g := 0; g < goroutines; g++ {
		b, _ := base.Get(distinctIDs[g])
		want := fmt.Sprintf("own-label-%d", g)
		if !hasLabel(b.Labels, want) {
			t.Errorf("distinct bead %d lost label %q (got %v)", g, want, b.Labels)
		}
	}
}

// txFailUpdateOKStore is a fake whose Tx always errors but whose direct Update
// succeeds — exercising flushLabelUpdates' Tx-failure → sequential-Update
// fallback.
type txFailUpdateOKStore struct {
	beads.Store
}

func (s txFailUpdateOKStore) Tx(string, func(beads.Tx) error) error {
	return fmt.Errorf("tx unavailable")
}

// TestFlushLabelUpdatesFallsBackToSequentialUpdateOnTxFailure proves a failing
// Tx does not lose buffered labels: the flush falls back to sequential Update
// (which succeeds) and the labels still land.
func TestFlushLabelUpdatesFallsBackToSequentialUpdateOnTxFailure(t *testing.T) {
	base := beads.NewMemStore()
	b1, err := base.Create(beads.Bead{Title: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := base.Create(beads.Bead{Title: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	store := txFailUpdateOKStore{Store: base}
	m := &memoryOrderDispatcher{}

	m.enqueueLabelUpdate(store, b1.ID, beads.UpdateOpts{Labels: []string{"wisp"}})
	m.enqueueLabelUpdate(store, b2.ID, beads.UpdateOpts{Labels: []string{"exec-failed"}})

	// Flush: Tx fails, sequential Update fallback applies the labels. Must not
	// panic or lose data; failure is logged, not fatal.
	m.flushLabelUpdates(store)

	got1, _ := base.Get(b1.ID)
	if !hasLabel(got1.Labels, "wisp") {
		t.Errorf("b1 missing label after Tx-failure fallback, got %v", got1.Labels)
	}
	got2, _ := base.Get(b2.ID)
	if !hasLabel(got2.Labels, "exec-failed") {
		t.Errorf("b2 missing label after Tx-failure fallback, got %v", got2.Labels)
	}
}
