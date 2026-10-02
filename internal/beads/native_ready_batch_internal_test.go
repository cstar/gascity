package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// This spy exposes the guarded batch API that the production Dolt store and
// its telemetry decorator both implement. Count calls independently of rows.
type readyBatchStorage struct {
	*nativeDoltStorageSpy
	edges                                issueops.EdgeReadResult
	edgeErr, hydrateErr                  error
	issues                               []*beadslib.Issue
	requested, hydrated                  []string
	edgeCalls, hydrateCalls, singleCalls int
}

func (s *readyBatchStorage) EdgeReader() (issueops.EdgeReader, error) { return s, nil }
func (s *readyBatchStorage) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	s.edgeCalls++
	s.requested = append([]string(nil), req.IDs...)
	return s.edges, s.edgeErr
}

func (s *readyBatchStorage) GetIssuesByIDs(_ context.Context, ids []string) ([]*beadslib.Issue, error) {
	s.hydrateCalls++
	s.hydrated = append([]string(nil), ids...)
	if s.issues != nil || s.hydrateErr != nil {
		return s.issues, s.hydrateErr
	}
	return []*beadslib.Issue{{ID: "blocked", Status: beadslib.StatusClosed, Metadata: []byte(`{"gc.work_outcome":"blocked"}`)}}, nil
}

func (s *readyBatchStorage) GetDependenciesWithMetadata(_ context.Context, _ string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	s.singleCalls++
	return []*beadslib.IssueWithDependencyMetadata{nativeReadyGateBlocker("blocked", beadslib.StatusClosed, beadslib.DependencyType("blocks"), `{"gc.work_outcome":"blocked"}`)}, nil
}

func TestNativeReadyBatchesWorkOutcomeReads(t *testing.T) {
	storage := &readyBatchStorage{nativeDoltStorageSpy: &nativeDoltStorageSpy{}}
	candidates := make([]Bead, 200)
	for i := range candidates {
		id := fmt.Sprintf("gc-candidate-%d", i)
		candidates[i] = Bead{ID: id}
		storage.edges.Anchors = append(storage.edges.Anchors, issueops.AnchorEdges{ID: id, Edges: []*issueops.Dependency{{IssueID: id, DependsOnID: "blocked", Type: beadslib.DependencyType("blocks")}}})
	}
	store := newNativeDoltStoreForTest(storage)
	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("blocked candidates admitted: %d", len(got))
	}
	if len(storage.requested) != 200 || !reflect.DeepEqual(storage.hydrated, []string{"blocked"}) {
		t.Fatalf("batch scope: anchors=%d blockers=%v", len(storage.requested), storage.hydrated)
	}
	if storage.singleCalls != 0 || storage.edgeCalls != 1 || storage.hydrateCalls != 1 {
		t.Fatalf("reads for 200 candidates: per-candidate=%d, edge batches=%d, hydration batches=%d; want 0,1,1", storage.singleCalls, storage.edgeCalls, storage.hydrateCalls)
	}
}

// Existing test doubles intentionally model stores without the optional batch
// implementation; their per-anchor behavior also covers the fallback path.
func (s *nativeDoltStorageSpy) EdgeReader() (issueops.EdgeReader, error) {
	return nil, &beadslib.ErrUnsupported{}
}

func (s *nativeDoltMemStorage) EdgeReader() (issueops.EdgeReader, error) {
	return nil, &beadslib.ErrUnsupported{}
}

func TestNativeReadyBatchPreservesGateSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, status, kind, metadata string
		veto                         bool
	}{
		{"blocked", "closed", "blocks", `{"gc.work_outcome":"blocked"}`, true},
		{"successful", "closed", "blocks", `{}`, false},
		{"pinned", "pinned", "blocks", `{"gc.work_outcome":"blocked"}`, false},
		{"spawner", "open", "waits-for", `{"gc.work_outcome":"blocked"}`, false},
		{"nonblocking", "closed", "related", `{"gc.work_outcome":"blocked"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &readyBatchStorage{nativeDoltStorageSpy: &nativeDoltStorageSpy{}, issues: []*beadslib.Issue{{ID: "target", Status: beadslib.Status(tc.status), Metadata: []byte(tc.metadata)}}}
			s.edges.Anchors = []issueops.AnchorEdges{{ID: "candidate", Edges: []*issueops.Dependency{{DependsOnID: "target", Type: beadslib.DependencyType(tc.kind)}}}}
			got, err := newNativeDoltStoreForTest(s).filterReadyByWorkOutcome(context.Background(), s, []Bead{{ID: "candidate"}})
			if err != nil {
				t.Fatal(err)
			}
			if (len(got) == 0) != tc.veto {
				t.Fatalf("result %v, veto=%v", got, tc.veto)
			}
			if tc.kind == "related" && s.hydrateCalls != 0 {
				t.Fatal("hydrated nonblocking target")
			}
		})
	}
}

func TestNativeReadyBatchFailsClosed(t *testing.T) {
	sentinel := errors.New("database read failed")
	for _, phase := range []string{"edges", "hydrate", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			s := &readyBatchStorage{nativeDoltStorageSpy: &nativeDoltStorageSpy{}}
			s.edges.Anchors = []issueops.AnchorEdges{{ID: "candidate", Edges: []*issueops.Dependency{{DependsOnID: "blocked", Type: beadslib.DependencyType("blocks")}}}}
			switch phase {
			case "edges":
				s.edgeErr = sentinel
			case "hydrate":
				s.hydrateErr = sentinel
			case "metadata":
				s.issues = []*beadslib.Issue{{ID: "blocked", Metadata: []byte(`{`)}}
			}
			got, err := newNativeDoltStoreForTest(s).filterReadyByWorkOutcome(context.Background(), s, []Bead{{ID: "candidate"}})
			if err == nil || got != nil {
				t.Fatalf("failed read admitted candidates: %v %v", got, err)
			}
			if phase != "metadata" && !errors.Is(err, sentinel) {
				t.Fatalf("lost error: %v", err)
			}
			if s.singleCalls != 0 {
				t.Fatal("retried failed batch with per-anchor reads")
			}
		})
	}
}
