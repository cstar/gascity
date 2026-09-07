package suspensionstate

import (
	"encoding/json"
	"testing"
)

func TestRigDatabaseServedRoundTrip(t *testing.T) {
	var st State
	if RigDatabaseServed(st, "r") {
		t.Fatal("empty state must report not served")
	}
	tr := true
	SetRigDatabaseServed(&st, "r", &tr)
	if !RigDatabaseServed(st, "r") {
		t.Fatal("explicit true must report served")
	}
	// Suspension and keep-served live side by side on the same entry.
	SetRig(&st, "r", &tr)
	if !RigDatabaseServed(st, "r") || !IsRigSuspended(st, "r") {
		t.Fatalf("SetRig must not clobber DatabaseServed: %+v", st.Rigs["r"])
	}
	SetRigDatabaseServed(&st, "r", nil)
	if RigDatabaseServed(st, "r") || !IsRigSuspended(st, "r") {
		t.Fatalf("clearing DatabaseServed must keep Suspended: %+v", st.Rigs["r"])
	}
	SetRig(&st, "r", nil)
	if _, ok := st.Rigs["r"]; ok {
		t.Fatal("entry with no overrides must be removed")
	}
}

func TestDatabaseServedSerializesOnlyWhenSet(t *testing.T) {
	var st State
	tr := true
	SetRig(&st, "r", &tr)
	b, _ := json.Marshal(st)
	if string(b) == "" || contains(string(b), "database_served") {
		t.Fatalf("unset DatabaseServed must be omitted: %s", b)
	}
	SetRigDatabaseServed(&st, "r", &tr)
	b, _ = json.Marshal(st)
	if !contains(string(b), `"database_served":true`) {
		t.Fatalf("DatabaseServed must serialize: %s", b)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !RigDatabaseServed(back, "r") {
		t.Fatal("DatabaseServed must survive a round trip")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
