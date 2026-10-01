package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSyncSessionBeads_DeferredSingletonBackoffSkipsAliasReads(t *testing.T) {
	base := beads.NewMemStore()
	store := &sessionSnapshotListSpyStore{Store: base}
	clk := &clock.Fake{Time: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "pack", MaxActiveSessions: intPtr(1)}}}
	const template = "pack/worker"
	owner, err := base.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"template": template, "session_name": "owner", "agent_name": template, "alias": template, "state": "asleep", "session_origin": "ephemeral", poolManagedMetadataKey: "true",
	}})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := base.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"template": template, "session_name": "deferred", "agent_name": "", "state": "asleep", "session_origin": "ephemeral", poolManagedMetadataKey: "true",
		"continuation_epoch":         strconv.Itoa(session.DefaultContinuationEpoch),
		poolAliasConflictMetadataKey: template, poolAliasConflictCountMetadataKey: "20", poolAliasConflictAtMetadataKey: clk.Now().Add(-time.Second).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]TemplateParams{"deferred": {TemplateName: template, Command: "new-command", WorkDir: "/new-workdir"}}
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	run := func() {
		store.queries = nil
		stderr.Reset()
		syncSessionBeads("", store, desired, sp, allConfiguredDS(desired), cfg, clk, &stderr, true)
	}
	aliasReads := func() int {
		n := 0
		for _, q := range store.queries {
			if q.Metadata["alias"] == template {
				n++
			}
		}
		return n
	}
	run()
	if n := aliasReads(); n != 0 {
		t.Fatalf("pending backoff issued %d alias reads; log=%s", n, stderr.String())
	}
	got, err := base.Get(victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["command"] != "new-command" {
		t.Fatal("unrelated metadata update was lost during backoff")
	}
	if got.Metadata[poolAliasConflictCountMetadataKey] != "20" || got.Metadata["alias"] != "" {
		t.Fatal("backoff changed alias ownership/conflict")
	}
	if got.Metadata["agent_name"] != "" || got.Metadata["work_dir"] != "" {
		t.Fatal("backoff applied alias-guarded identity metadata")
	}
	if strings.Contains(stderr.String(), "alias ") {
		t.Fatalf("backoff still logs alias attempts: %s", stderr.String())
	}
	clk.Time = clk.Time.Add(deferredSingletonAliasRetryMax + time.Second)
	run()
	if aliasReads() == 0 {
		t.Fatal("expired backoff never retried alias ownership")
	}
	if err := base.Close(owner.ID); err != nil {
		t.Fatal(err)
	}
	// The newly loaded census must release the backoff immediately.
	run()
	if aliasReads() == 0 {
		t.Fatal("owner release must still perform authoritative alias validation")
	}
	got, err = base.Get(victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["alias"] != template || got.Metadata[poolAliasConflictMetadataKey] != "" {
		t.Fatalf("alias recovery failed after owner close: %+v", got.Metadata)
	}
}
