package beads

import (
	"encoding/json"
	"testing"
)

// an-74m regression bar (b).
//
// `open -> deferred(dateless)` is invisible to the cache diff today: mapBdStatus
// folds both to "open" and DeferUntil is nil on both sides, so every field
// beadChanged compares is equal and the reconcile records `updates=0`. The cache
// is not stale — it is being told nothing. Carrying the raw upstream status is
// what makes the transition observable through the CACHED handle, which is the
// handle the controller actually reads (listBothTiersForControllerDemand).
func TestBeadChangedDetectsDatelessDeferredPark(t *testing.T) {
	old := Bead{ID: "pr-7crj", Status: "open", UpstreamStatus: "open", Assignee: "worker"}
	fresh := Bead{ID: "pr-7crj", Status: "open", UpstreamStatus: "deferred", Assignee: "worker"}

	if !beadChanged(old, fresh, false) {
		t.Fatalf("open -> dateless-deferred must be a cache change; mapped Status and DeferUntil are both equal, so UpstreamStatus is the only signal")
	}
}

// an-74m regression bar (c).
//
// Event patches merge field-by-field. A patch that does not carry `status` must
// leave the raw status alone — otherwise any unrelated event (a title edit, a
// metadata stamp) silently blanks the park signal and the bead reappears as live
// assigned work.
func TestMergeCacheEventPatchPreservesUpstreamStatusWhenStatusAbsent(t *testing.T) {
	base := Bead{ID: "pr-7crj", Status: "open", UpstreamStatus: "deferred"}
	patch := Bead{ID: "pr-7crj", Title: "retitled"}
	fields := map[string]json.RawMessage{"title": json.RawMessage(`"retitled"`)}

	merged := mergeCacheEventPatch(base, patch, fields)

	if merged.UpstreamStatus != "deferred" {
		t.Fatalf("status-free patch blanked UpstreamStatus: got %q, want %q", merged.UpstreamStatus, "deferred")
	}
}

// The mirror case: when the patch DOES carry status, the raw status must follow
// it, or the event and reconcile paths disagree about the same bead.
func TestMergeCacheEventPatchCarriesUpstreamStatusWhenStatusPresent(t *testing.T) {
	base := Bead{ID: "pr-7crj", Status: "open", UpstreamStatus: "open"}
	patch := Bead{ID: "pr-7crj", Status: "open", UpstreamStatus: "deferred"}
	fields := map[string]json.RawMessage{"status": json.RawMessage(`"deferred"`)}

	merged := mergeCacheEventPatch(base, patch, fields)

	if merged.UpstreamStatus != "deferred" {
		t.Fatalf("status-bearing patch did not carry UpstreamStatus: got %q, want %q", merged.UpstreamStatus, "deferred")
	}
}

// decodeCacheEvent unmarshals the wire bead directly (b := wire.Bead) with no
// mapBdStatus pass, so an event carries the RAW status where the reconcile path
// carries the mapped one. Both paths must agree: Status mapped, UpstreamStatus raw.
func TestDecodeCacheEventSplitsRawAndMappedStatus(t *testing.T) {
	payload := json.RawMessage(`{"id":"pr-7crj","status":"deferred","assignee":"worker"}`)

	b, fields, err := decodeCacheEvent(payload)
	if err != nil {
		t.Fatalf("decodeCacheEvent: %v", err)
	}
	if _, ok := fields["status"]; !ok {
		t.Fatalf("status field not recorded in patch fields: %v", fields)
	}
	if b.Status != "open" {
		t.Errorf("event Status must be mapped to the gascity contract: got %q, want %q", b.Status, "open")
	}
	if b.UpstreamStatus != "deferred" {
		t.Errorf("event UpstreamStatus must keep the raw upstream value: got %q, want %q", b.UpstreamStatus, "deferred")
	}
}
