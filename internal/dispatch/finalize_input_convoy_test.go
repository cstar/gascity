package dispatch

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// A graph.v2 workflow slung on a convoy links back to its work bead only through
// gc.input_convoy_id (graph roots clear gc.source_bead_id). On outcome=pass the
// finalizer must close the convoy's single tracked bead, as it does for
// gc.source_bead_id chains. Seen live: evo-simple roots for car-1z5z.3/.4 passed and
// merged, but the product bead stayed open until a human closed it.

type inputConvoyFixture struct {
	infra, rig *beads.MemStore
	product    beads.Bead
	finalizer  beads.Bead
	workflow   beads.Bead
	resolver   func(string) (beads.Store, error)
}

func newInputConvoyFixture(t *testing.T, tracked int) inputConvoyFixture {
	t.Helper()
	f := inputConvoyFixture{infra: beads.NewMemStore(), rig: beads.NewMemStore()}
	f.product = mustCreateWorkflowBead(t, f.rig, beads.Bead{Title: "Product work", Type: "task", Status: "blocked"})
	convoy := mustCreateWorkflowBead(t, f.rig, beads.Bead{Title: "sling convoy", Type: "convoy"})
	mustDepAdd(t, f.rig, convoy.ID, f.product.ID, "tracks")
	for i := 1; i < tracked; i++ {
		other := mustCreateWorkflowBead(t, f.rig, beads.Bead{Title: fmt.Sprintf("Other %d", i), Type: "task"})
		mustDepAdd(t, f.rig, convoy.ID, other.ID, "tracks")
	}
	f.workflow = mustCreateWorkflowBead(t, f.infra, beads.Bead{
		Title: "evo-simple",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  convoy.ID,
			"gc.root_store_ref":   "rig:test",
		},
	})
	cleanup := mustCreateWorkflowBead(t, f.infra, beads.Bead{
		Title: "Handoff", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "pass"},
	})
	f.finalizer = mustCreateWorkflowBead(t, f.infra, beads.Bead{
		Title: "Finalize workflow", Type: "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": f.workflow.ID},
	})
	mustDepAdd(t, f.infra, f.finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, f.infra, f.workflow.ID, f.finalizer.ID, "blocks")
	f.resolver = func(ref string) (beads.Store, error) {
		if ref == "rig:test" {
			return f.rig, nil
		}
		return nil, fmt.Errorf("unknown store ref: %s", ref)
	}
	return f
}

func TestProcessWorkflowFinalizeClosesInputConvoyTrackedBead(t *testing.T) {
	t.Parallel()
	f := newInputConvoyFixture(t, 1)
	if _, err := ProcessControl(f.infra, f.finalizer, ProcessOptions{ResolveStoreRef: f.resolver}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	after, err := f.rig.Get(f.product.ID)
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("product status = %q, want closed", after.Status)
	}
	if got := after.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("product gc.outcome = %q, want pass", got)
	}
}

func TestProcessWorkflowFinalizeLeavesAmbiguousInputConvoyOpen(t *testing.T) {
	t.Parallel()
	f := newInputConvoyFixture(t, 2)
	if _, err := ProcessControl(f.infra, f.finalizer, ProcessOptions{ResolveStoreRef: f.resolver}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	root, _ := f.infra.Get(f.workflow.ID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed even when the convoy is ambiguous", root.Status)
	}
	after, _ := f.rig.Get(f.product.ID)
	if after.Status == "closed" {
		t.Fatalf("product closed although its convoy tracks two beads")
	}
}

func TestProcessWorkflowFinalizeToleratesUnresolvableInputConvoy(t *testing.T) {
	t.Parallel()
	f := newInputConvoyFixture(t, 1)
	broken := func(string) (beads.Store, error) { return nil, fmt.Errorf("store offline") }
	if _, err := ProcessControl(f.infra, f.finalizer, ProcessOptions{ResolveStoreRef: broken}); err != nil {
		t.Fatalf("finalize must not fail on an unreadable input convoy: %v", err)
	}
	root, _ := f.infra.Get(f.workflow.ID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
}
