package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
)

// readMemoProxyFixture is a proxied scope on disk, its proxy root and record,
// and a process table that says the recorded proxy is alive and serving it.
type readMemoProxyFixture struct {
	scope string
	root  string
	pid   int
	port  int
	birth string
	argv  []string
}

func newReadMemoProxyFixture(t *testing.T) *readMemoProxyFixture {
	t.Helper()
	t.Setenv(proxyendpoint.RootPathEnv, "")
	t.Setenv(bdReadMemoEnv, "")
	f := &readMemoProxyFixture{
		scope: t.TempDir(),
		root:  t.TempDir(),
		pid:   4242,
		port:  40547,
		birth: "linux-v1:boot:1",
	}
	f.argv = []string{"bd", proxyendpoint.ChildVerb, "--root", f.root, "--port", "0", "--idle-timeout", "-1ns", "--database", "hq"}
	f.writeMetadata(t, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"ga"}`)
	f.writeSidecar(t, -1)
	f.writeRecord(t)
	prev := bdReadMemoProcessTable
	bdReadMemoProcessTable = func() proxyendpoint.ProcessTable {
		return proxyendpoint.ProcessTable{
			Alive: func(pid int) bool { return pid == f.pid },
			Argv:  func(int) ([]string, error) { return f.argv, nil },
			Birth: func(int) (string, error) { return f.birth, nil },
		}
	}
	t.Cleanup(func() { bdReadMemoProcessTable = prev })
	return f
}

func (f *readMemoProxyFixture) writeMetadata(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(f.scope, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *readMemoProxyFixture) writeSidecar(t *testing.T, idle time.Duration) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"root_path": f.root, "idle_timeout": idle})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.scope, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, proxyendpoint.SidecarFileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *readMemoProxyFixture) writeRecord(t *testing.T) {
	t.Helper()
	rootID, err := proxyendpoint.RootID(f.root)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(proxyendpoint.Record{
		PID: f.pid, Port: f.port, Schema: proxyendpoint.SchemaV2, Kind: proxyendpoint.RecordKind, Birth: f.birth, RootID: rootID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(f.root), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// a) Activation: only a proxied dolt scope with a valid database name, a live
// proxy record for its root and a proxy that never retires gets a memo.
func TestReadMemoActivation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f *readMemoProxyFixture)
		wantOn bool
		reason string
	}{
		{name: "conforming proxied scope", mutate: func(*testing.T, *readMemoProxyFixture) {}, wantOn: true},
		{name: "not proxied", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.writeMetadata(t, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"ga"}`)
		}, reason: "is not proxied-server"},
		{name: "not dolt", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.writeMetadata(t, `{"backend":"doltlite","dolt_mode":"proxied-server","dolt_database":"ga"}`)
		}, reason: "is not dolt"},
		{name: "proxy.pid absent", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			if err := os.Remove(proxyendpoint.PIDPath(f.root)); err != nil {
				t.Fatal(err)
			}
		}, reason: "proxy endpoint"},
		{name: "proxy process dead", mutate: func(_ *testing.T, f *readMemoProxyFixture) {
			f.pid = 4243 // the record still names 4242, which the table now reports dead
		}, reason: "proxy endpoint"},
		{name: "invalid database name", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.writeMetadata(t, `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"ga-x"}`)
		}, reason: "dolt_database"},
		{name: "database name with sql", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.writeMetadata(t, `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"ga;drop"}`)
		}, reason: "dolt_database"},
		{name: "proxy with finite idle timeout", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.argv = []string{"bd", proxyendpoint.ChildVerb, "--root", f.root, "--idle-timeout", "30s"}
			f.writeSidecar(t, 30*time.Second)
		}, reason: "idle policy"},
		{name: "sidecar default idle and argv silent", mutate: func(t *testing.T, f *readMemoProxyFixture) {
			f.argv = []string{"bd", proxyendpoint.ChildVerb, "--root", f.root}
			f.writeSidecar(t, 0)
		}, reason: "idle policy"},
		{name: "kill switch", mutate: func(t *testing.T, _ *readMemoProxyFixture) {
			t.Setenv(bdReadMemoEnv, "off")
		}, reason: bdReadMemoEnv + "=off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadMemoProxyFixture(t)
			tc.mutate(t, f)
			target, reason, ok := bdReadMemoTargetFor(f.scope)
			if ok != tc.wantOn {
				t.Fatalf("bdReadMemoTargetFor ok = %v (reason %q), want %v", ok, reason, tc.wantOn)
			}
			if !ok && !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.reason)
			}
			if ok && (target.database != "ga" || target.proxyRoot != f.root) {
				t.Fatalf("target = %+v, want database ga at root %s", target, f.root)
			}

			store := beads.NewBdStoreWithPrefix(f.scope, func(string, string, ...string) ([]byte, error) { return []byte(`[]`), nil }, "ga")
			policy := wrapStoreWithBeadPolicies(store, &config.City{})
			if got := enableBdReadMemo(policy, f.scope, "rig/test"); got != tc.wantOn {
				t.Fatalf("enableBdReadMemo = %v, want %v", got, tc.wantOn)
			}
			if store.ReadMemoEnabled() != tc.wantOn {
				t.Fatalf("store memo enabled = %v, want %v", store.ReadMemoEnabled(), tc.wantOn)
			}
		})
	}
}

// Stores that run no bd are never armed.
func TestReadMemoActivationSkipsNonBdStores(t *testing.T) {
	f := newReadMemoProxyFixture(t)
	if enableBdReadMemo(beads.NewMemStore(), f.scope, "city") {
		t.Fatal("a MemStore was armed with a bd read memo")
	}
}

// The probe reads the working hash through the resolved proxy port, tags it
// with the proxy generation, re-inspects the record only when it is stale, and
// after a failed query reads proxy.pid again (a restarted proxy moves port).
func TestReadMemoProbeRereadsProxyRecordAfterConnectionError(t *testing.T) {
	f := newReadMemoProxyFixture(t)
	target, reason, ok := bdReadMemoTargetFor(f.scope)
	if !ok {
		t.Fatalf("fixture not eligible: %s", reason)
	}
	var (
		mu    sync.Mutex
		ports []int
		fail  bool
	)
	prevQuery := bdReadMemoQueryHash
	bdReadMemoQueryHash = func(_ context.Context, port int, database string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		ports = append(ports, port)
		if database != "ga" {
			return "", fmt.Errorf("database %q", database)
		}
		if fail {
			fail = false
			return "", errors.New("dial tcp: connection refused")
		}
		return "h1", nil
	}
	t.Cleanup(func() { bdReadMemoQueryHash = prevQuery })

	probe := &bdWorkingHashProbe{target: target, endpoints: &bdReadMemoEndpointCache{byRoot: map[string]bdReadMemoEndpoint{}}}
	first, err := probe.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := f.birth + ":40547/h1"; first != want {
		t.Fatalf("probe token = %q, want %q", first, want)
	}

	// The proxy restarts on another port. Until a query fails, the cached port
	// is used; the failure makes the next read take the new record.
	f.port, f.birth = 40999, "linux-v1:boot:2"
	f.writeRecord(t)
	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := probe.read(context.Background()); err == nil {
		t.Fatal("probe hid a connection error")
	}
	again, err := probe.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "linux-v1:boot:2:40999/h1"; again != want {
		t.Fatalf("probe token after restart = %q, want %q", again, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []int{40547, 40547, 40999}; !reflect.DeepEqual(ports, want) {
		t.Fatalf("probe ports = %v, want %v", ports, want)
	}
}

// tickLeg is one memoizable bd leg of the tick test: a BdStore over a fake bd
// that counts its subprocesses by verb.
type tickLeg struct {
	mu     sync.Mutex
	prefix string
	calls  map[string]int
}

func (l *tickLeg) run(_, _ string, args ...string) ([]byte, error) {
	l.mu.Lock()
	l.calls[args[0]]++
	l.mu.Unlock()
	p := l.prefix
	row := func(id, status, assignee, extra string) string {
		return fmt.Sprintf(`{"id":%q,"title":%q,"status":%q,"assignee":%q,"issue_type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{}%s}`, id, id, status, assignee, extra)
	}
	joined := strings.Join(args, " ")
	switch args[0] {
	case "list":
		switch {
		case strings.Contains(joined, "--id="):
			return []byte("[" + row(p+"-done", "closed", "", "") + "]"), nil
		case strings.Contains(joined, "--status=in_progress"):
			return []byte("[" + row(p+"-2", "in_progress", "city--worker-1", "") + "]"), nil
		case strings.Contains(joined, "--status=open"):
			return []byte("[" + row(p+"-1", "open", "city--worker-1", "") + "," + row(p+"-3", "open", "", "") + "]"), nil
		}
		return []byte(`[]`), nil
	case "query":
		if strings.Contains(joined, "status=open") {
			return []byte("[" + row(p+"-wisp-1", "open", "city--worker-2", "") + "]"), nil
		}
		return []byte(`[]`), nil
	case "ready":
		dep := fmt.Sprintf(`,"dependency_count":1,"dependencies":[{"issue_id":%q,"depends_on_id":%q,"type":"blocks"}]`, p+"-3", p+"-done")
		return []byte("[" + row(p+"-3", "open", "", dep) + "]"), nil
	}
	return []byte(`[]`), nil
}

func (l *tickLeg) take() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.calls
	l.calls = map[string]int{}
	return out
}

type tickObservation struct {
	byAssignee []map[string][]string
	demand     map[string][]string
	ready      map[string][]string
}

// runTickPass reads what one supervisor tick reads from every leg: the seat
// work index, the controller-demand open list and the live ready snapshot.
func runTickPass(t *testing.T, cityPath string, cfg *config.City, city beads.Store, rigs map[string]beads.Store, names map[beads.Store]string) tickObservation {
	t.Helper()
	obs := tickObservation{demand: map[string][]string{}, ready: map[string][]string{}}
	idx := newSeatWorkIndex(cityPath, cfg, city, rigs)
	idx.read()
	if idx.err != nil {
		t.Fatalf("seat work index: %v", idx.err)
	}
	for _, leg := range idx.legs {
		if leg.err != nil {
			t.Fatalf("seat work leg %s: %v", names[leg.store], leg.err)
		}
		ids := map[string][]string{}
		for assignee, rows := range leg.byAssignee {
			for _, b := range rows {
				ids[assignee] = append(ids[assignee], b.ID)
			}
			sort.Strings(ids[assignee])
		}
		obs.byAssignee = append(obs.byAssignee, ids)
	}
	demandCache := newReadyDemandCache()
	all := []beads.Store{city}
	for _, name := range []string{"ra", "rb"} {
		all = append(all, rigs[name])
	}
	for _, store := range all {
		rows, err := listOpenForControllerDemandLive(store)
		if err != nil {
			t.Fatalf("demand list %s: %v", names[store], err)
		}
		for _, b := range rows {
			obs.demand[names[store]] = append(obs.demand[names[store]], b.ID)
		}
		sort.Strings(obs.demand[names[store]])
		ready, err := demandCache.liveSnapshot(store)
		if err != nil {
			t.Fatalf("ready %s: %v", names[store], err)
		}
		for _, b := range ready {
			obs.ready[names[store]] = append(obs.ready[names[store]], b.ID)
		}
	}
	return obs
}

// runTick builds three supervisor-shaped bd legs (policy store over a
// CachingStore over a BdStore, as buildStores makes them), memoized with a
// constant probe or not, runs two tick passes and reports what each cost.
func runTick(t *testing.T, memo bool) ([]map[string]int, []tickObservation) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Rigs: []config.Rig{
			{Name: "ra", Path: filepath.Join(cityPath, "ra"), Prefix: "ra"},
			{Name: "rb", Path: filepath.Join(cityPath, "rb"), Prefix: "rb"},
		},
	}
	legs := map[string]*tickLeg{}
	stores := map[string]beads.Store{}
	names := map[beads.Store]string{}
	for _, prefix := range []string{"ct", "ra", "rb"} {
		leg := &tickLeg{prefix: prefix, calls: map[string]int{}}
		bd := beads.NewBdStoreWithPrefix(t.TempDir(), leg.run, prefix)
		if memo {
			bd.EnableReadMemo(func(context.Context) (string, error) { return "steady", nil }, bdReadMemoMaxBytes)
		}
		store := wrapWithCachingStore(context.Background(), wrapStoreWithBeadPolicies(bd, cfg), nil, false)
		legs[prefix], stores[prefix], names[store] = leg, store, prefix
	}
	rigs := map[string]beads.Store{"ra": stores["ra"], "rb": stores["rb"]}

	var costs []map[string]int
	var observations []tickObservation
	for pass := 0; pass < 2; pass++ {
		for _, leg := range legs {
			leg.take() // the cache's synchronous pre-prime, then the previous pass
		}
		observations = append(observations, runTickPass(t, cityPath, cfg, stores["ct"], rigs, names))
		total := map[string]int{}
		for _, leg := range legs {
			for verb, n := range leg.take() {
				total[verb] += n
			}
		}
		costs = append(costs, total)
	}
	return costs, observations
}

// b) One supervisor tick's reads, twice, over three memoized bd legs at a
// constant hash: the second pass runs only the three bd ready (one per store),
// and every answer is the one the unmemoized supervisor gets.
func TestReadMemoTickSecondPassRunsOnlyReady(t *testing.T) {
	memoCosts, memoObs := runTick(t, true)
	liveCosts, liveObs := runTick(t, false)
	t.Logf("bd calls per pass: memo %v, no memo %v", memoCosts, liveCosts)

	if got := memoCosts[1]; !reflect.DeepEqual(got, map[string]int{"ready": 3}) {
		t.Fatalf("second memoized pass ran %v, want only 3 bd ready (first pass %v)", got, memoCosts[0])
	}
	if liveCosts[1]["list"] == 0 || liveCosts[1]["query"] == 0 {
		t.Fatalf("unmemoized second pass ran %v; the premise is a supervisor that re-reads every leg", liveCosts[1])
	}
	for pass := range memoObs {
		if !reflect.DeepEqual(memoObs[pass], liveObs[pass]) {
			t.Fatalf("pass %d answers differ with the memo:\nmemo: %+v\nlive: %+v", pass, memoObs[pass], liveObs[pass])
		}
	}
	if len(memoObs[1].byAssignee) != 3 {
		t.Fatalf("seat work index read %d legs, want 3", len(memoObs[1].byAssignee))
	}
	for _, store := range []string{"ct", "ra", "rb"} {
		if got := memoObs[1].ready[store]; len(got) != 1 {
			t.Fatalf("ready rows for %s = %v, want the one candidate", store, got)
		}
	}
}

// The stats line sums each memoized BdStore once, through the controller's
// cache and policy wrappers, and ignores stores without a memo.
func TestReadMemoStatsAggregateAcrossStores(t *testing.T) {
	runner := func(string, string, ...string) ([]byte, error) { return []byte(`[]`), nil }
	memoized := beads.NewBdStoreWithPrefix(t.TempDir(), runner, "ga")
	memoized.EnableReadMemo(func(context.Context) (string, error) { return "h", nil }, 1<<20)
	plain := beads.NewBdStoreWithPrefix(t.TempDir(), runner, "hq")
	for i := 0; i < 3; i++ {
		if _, err := memoized.List(beads.ListQuery{Status: "open"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{}
	wrapped := wrapStoreWithBeadPolicies(beads.NewCachingStore(wrapStoreWithBeadPolicies(memoized, cfg), nil), cfg)
	stats, n := bdReadMemoStatsOf([]beads.Store{wrapped, memoized, plain, beads.NewMemStore()})
	if n != 1 {
		t.Fatalf("memoized stores counted = %d, want 1", n)
	}
	if stats.Hits != 2 || stats.Misses != 1 || stats.Entries != 1 {
		t.Fatalf("stats = %+v, want 2 hits, 1 miss, 1 entry", stats)
	}
	line := formatBdReadMemoStats(stats, n)
	for _, want := range []string{"stores=1", "hits=2", "misses=1", "hit_ratio=0.667"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stats line %q lacks %q", line, want)
		}
	}
}
