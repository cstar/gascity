//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltDeferralAgainstIsolatedServer(t *testing.T) {
	// Do not inherit the city's database endpoint or credential helpers.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "GC_DOLT") {
			t.Setenv(key, "")
		}
	}
	port := startTestDoltServer(t)
	dir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatal(err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "defer-integration", "gc")
	b, err := store.Create(Bead{Title: "isolated capacity wait", Status: "in_progress", Assignee: "worker", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err = store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, owner := "open", ""
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := store.UpdateIfMatch(b.ID, b.Revision, UpdateOpts{Status: &status, Assignee: &owner, DeferUntil: &deadline}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.DeferUntil == nil || !got.DeferUntil.Equal(deadline) {
		t.Fatalf("incomplete durable deferral: %+v", got)
	}
	rows, err := store.Ready(ReadyQuery{TierMode: TierWisps})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == b.ID {
			t.Fatal("future task ready")
		}
	}
	past := deadline.Add(-2 * time.Hour)
	if err := store.UpdateIfMatch(b.ID, got.Revision, UpdateOpts{DeferUntil: &past}); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Ready(ReadyQuery{TierMode: TierWisps})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == b.ID {
			return
		}
	}
	t.Fatal("expired task absent from Ready")
}
