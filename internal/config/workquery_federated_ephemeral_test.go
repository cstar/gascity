package config

import (
	"strings"
	"testing"
)

// Federated ready reads already include both storage tiers. Workspace
// fallbacks only duplicate these reads and can return
// an obsolete retained copy after the authoritative federation found no work.
func TestFederatedReadyQueriesDoNotRepeatEphemeralWorkspaceScans(t *testing.T) {
	a := Agent{Name: "worker"}
	for _, modern := range []bool{false, true} {
		topo := QueryTopology{FederatedReady: true}
		if modern {
			topo = federatedTopology()
		}
		queries := []string{a.EffectiveAssignedReadyQueryFor(topo), a.EffectiveRoutedPoolQueryFor(topo)}
		for _, query := range queries {
			if strings.Contains(query, "bd query") {
				t.Errorf("federated discovery repeats a workspace scan: %s", query)
			}
		}
	}
}

func TestFederatedRecoveryRetainsEligibleEphemeralFallbackAfterHeldHead(t *testing.T) {
	requireJQ(t)
	held := `[{"id":"held","status":"in_progress","assignee":"sess-1","labels":["hold:external"],"blocked_by":[]}]`
	bd := `#!/bin/sh
case "$1" in
 query) printf '%s' '[{"id":"eligible","status":"in_progress","assignee":"sess-1","ephemeral":true}]' ;;
 *) printf '[]' ;;
esac
`
	res := runGeneratedQueryWithBD(t, (&Agent{Name: "worker"}).EffectiveAssignedInProgressQueryFor(federatedTopology()), map[string]string{"GC_SESSION_ID": "sess-1"}, fakeGCServingInProgress(held), bd)
	if res.exit != 0 || !strings.Contains(res.stdout, `"eligible"`) {
		t.Fatalf("lost eligible recovery behind held first row: exit=%d stdout=%s stderr=%s", res.exit, res.stdout, res.stderr)
	}
}
