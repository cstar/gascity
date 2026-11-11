// This file is Linux-only by its filename, like
// beads_provider_script_scope_lifecycle_linux_test.go: it initializes a real
// proxied scope through the POSIX provider script.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/testutil"
)

// c) On a real bd proxy in front of a real Dolt server: a write made by another
// process (a separate bd update) moves the working hash the probe reads, also
// as seen from a session whose current database is another one, and the next
// memoized read returns the new value.
func TestReadMemoProbeSeesAnotherProcessWriteOnRealProxy(t *testing.T) {
	skipSlowCmdGCTest(t, "starts a real bd proxy and Dolt server; run make test-cmd-gc-process for full coverage")
	bdPath := waitTestRealBDPath(t)
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv(proxyendpoint.RootPathEnv, "")
	t.Setenv(bdReadMemoEnv, "")

	dir := filepath.Join(t.TempDir(), "city")
	home := filepath.Join(t.TempDir(), "home")
	for _, d := range []string{dir, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	testutil.RunGit(t, dir, "init", "-q")
	env := sanitizedBaseEnv("HOME="+home, "DOLT_ROOT_PATH="+home, "GC_CITY_PATH="+dir, "BEADS_DIR="+filepath.Join(dir, ".beads"),
		"BD_BIN="+bdPath, "GC_BEADS_PROVIDER_OWNED=1", "GC_BEADS_TRANSPORT=proxied", "GC_BEADS_TARGET=local")
	t.Cleanup(func() {
		_, _ = runProviderOwnedScriptOp(t, env, "stop")
		if leaked := providerOwnedDoltPIDsUnderRoot(t, dir); len(leaked) != 0 {
			if errs := reapDoltLeakPIDs(leaked); len(errs) != 0 {
				t.Errorf("reap processes under %s: %v", dir, errs)
			}
		}
	})
	if out, err := runProviderOwnedScriptOp(t, env, "init", dir, "mm", "mm"); err != nil {
		t.Fatalf("gc-beads-bd init: %v\n%s", err, out)
	}
	if out, err := runProviderOwnedScriptOp(t, env, "health"); err != nil {
		t.Fatalf("gc-beads-bd health: %v\n%s", err, out)
	}

	bdEnv := append(append([]string(nil), env...), "BEADS_DOLT_PROXIED_SERVER=1", "BEADS_BACKEND=dolt", "BD_NON_INTERACTIVE=1", "BEADS_ACTOR=gc-test")
	// otherProcess runs bd as a process of its own, the way an agent writes.
	otherProcess := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bdPath, args...) //nolint:gosec // pinned test bd
		cmd.Dir = dir
		cmd.Env = bdEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("bd %v: %v\n%s%s", args, err, out, stderr.String())
		}
		return out
	}
	runner := func(runDir, name string, args ...string) ([]byte, error) {
		if name == "bd" {
			name = bdPath
		}
		cmd := exec.Command(name, args...) //nolint:gosec // pinned test bd
		cmd.Dir = runDir
		cmd.Env = bdEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}

	target, reason, ok := bdReadMemoTargetFor(dir)
	if !ok {
		t.Fatalf("a gc-initialized proxied scope is not eligible for the memo: %s", reason)
	}
	if target.database != "mm" {
		t.Fatalf("probe database = %q, want mm", target.database)
	}
	probe := &bdWorkingHashProbe{target: target, endpoints: &bdReadMemoEndpointCache{byRoot: map[string]bdReadMemoEndpoint{}}}
	store := beads.NewBdStoreWithPrefix(dir, runner, "mm")
	store.EnableReadMemo(probe.read, 1<<20)

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(otherProcess("create", "--json", "before", "-t", "task"), &created); err != nil || created.ID == "" {
		t.Fatalf("bd create: id %q, err %v", created.ID, err)
	}
	for i := 0; i < 2; i++ {
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if got.Title != "before" {
			t.Fatalf("Get %d title = %q, want before", i, got.Title)
		}
	}
	if stats := store.ReadMemoStats(); stats.Hits != 1 {
		t.Fatalf("stats before the write = %+v, want the second Get to be a hit", stats)
	}
	before, err := probe.read(context.Background())
	if err != nil {
		t.Fatalf("probe before the write: %v", err)
	}
	otherSessionBefore := readWorkingHashFromAnotherDatabase(t, target)

	otherProcess("update", "--json", created.ID, "--title", "after")

	after, err := probe.read(context.Background())
	if err != nil {
		t.Fatalf("probe after the write: %v", err)
	}
	if after == before {
		t.Fatalf("working hash did not move after another process's bd update: %s", after)
	}
	otherSessionAfter := readWorkingHashFromAnotherDatabase(t, target)
	if otherSessionAfter == otherSessionBefore {
		t.Fatalf("working hash read from another database's session did not move: %s", otherSessionAfter)
	}
	if !strings.HasSuffix(after, "/"+otherSessionAfter) {
		t.Fatalf("probe token %q and the other session's hash %q disagree", after, otherSessionAfter)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after the write: %v", err)
	}
	if got.Title != "after" {
		t.Fatalf("memoized Get after another process's write = %q, want after", got.Title)
	}
}

// readWorkingHashFromAnotherDatabase reads @@<db>_working over a private
// connection whose current database is information_schema, not db.
func readWorkingHashFromAnotherDatabase(t *testing.T, target bdReadMemoTarget) string {
	t.Helper()
	ep := proxyendpoint.Inspect(target.proxyRoot, proxyendpoint.DefaultProcessTable())
	if !ep.Verdict.Live() {
		t.Fatalf("proxy endpoint: %s (%v)", ep.Verdict, ep.Err)
	}
	cfg := mysql.NewConfig()
	cfg.User = "root"
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(proxyendpoint.Host, strconv.Itoa(ep.Record.Port))
	cfg.DBName = "information_schema"
	cfg.Timeout = 5 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close() //nolint:errcheck // test-private handle
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var current, hash string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE(), @@"+target.database+"_working").Scan(&current, &hash); err != nil {
		t.Fatalf("read @@%s_working from another database: %v", target.database, err)
	}
	if current == target.database {
		t.Fatalf("session's current database is %q, want another one", current)
	}
	return hash
}
