package main

import "testing"

// PortHarbour 2026-11-15: gc bd unclaim <id> --if-assignee gcg-session-… was refused as "owned by
// the infra class binding": --if-assignee was unknown to the scanner, so its value was judged as an id.
func TestBdUnclaimIfAssigneeValueIsNotAnAddressedID(t *testing.T) {
	args := []string{"unclaim", "sb-58t.35.4", "--if-assignee", "gcg-session-43f19f416cb299548924ce8d84b2aa1f", "--reason", "arret"}
	if ids := bdArgsAddressedClassIDs(args); len(ids) != 0 {
		t.Fatalf("addressed ids = %v, want none (the session id is a flag value)", ids)
	}
	inline := []string{"unclaim", "sb-1", "--if-assignee=gcg-session-43f1"}
	if ids := bdArgsAddressedClassIDs(inline); len(ids) != 0 {
		t.Fatalf("inline: addressed ids = %v, want none", ids)
	}
}
