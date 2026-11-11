package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoTestRunner is a bd CommandRunner that logs every argv it is handed and
// answers from a caller-supplied function. It is the subprocess counter the
// read-memo tests assert on.
type memoTestRunner struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(args []string) ([]byte, error)
}

func (r *memoTestRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	respond := r.respond
	r.mu.Unlock()
	if respond == nil {
		return []byte(`[]`), nil
	}
	return respond(args)
}

// count reports how many logged argvs had sub as their bd subcommand.
func (r *memoTestRunner) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if len(call) > 1 && call[1] == sub {
			n++
		}
	}
	return n
}

// dataReads counts the reads the memo is allowed to answer: everything but the
// once-per-store capability probe (bd version).
func (r *memoTestRunner) dataReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if len(call) > 1 && call[1] != "version" {
			n++
		}
	}
	return n
}

func (r *memoTestRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// memoTestProbe is a scripted working-hash probe. hash is what it answers;
// err, when set, is returned instead.
type memoTestProbe struct {
	mu    sync.Mutex
	hash  string
	err   error
	calls int
}

func (p *memoTestProbe) probe(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return "", p.err
	}
	return p.hash, nil
}

func (p *memoTestProbe) set(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hash = hash
}

const memoTestOpenList = `[{"id":"ga-1","title":"one","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{"k":"v"}}]`

const memoTestOpenWisps = `[{"id":"ga-wisp-1","title":"wisp","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:01Z","metadata":{}}]`

func memoTestListResponder(args []string) ([]byte, error) {
	switch args[0] {
	case "list":
		return []byte(memoTestOpenList), nil
	case "query":
		return []byte(memoTestOpenWisps), nil
	case "show":
		if len(args) > 2 {
			return []byte(`[{"id":"` + args[2] + `","title":"shown","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z"}]`), nil
		}
	case "ready":
		return []byte(memoTestOpenList), nil
	case "update":
		return []byte(`{}`), nil
	}
	return []byte(`[]`), nil
}

func newMemoTestStore(t *testing.T, respond func([]string) ([]byte, error)) (*BdStore, *memoTestRunner, *memoTestProbe) {
	t.Helper()
	runner := &memoTestRunner{respond: respond}
	probe := &memoTestProbe{hash: "hash-a"}
	store := NewBdStoreWithPrefix(t.TempDir(), runner.run, "ga")
	store.EnableReadMemo(probe.probe, 64<<20)
	return store, runner, probe
}

func openBothTiers() ListQuery {
	return ListQuery{Status: "open", TierMode: TierBoth}
}

// a) At a constant working hash the second identical List runs no bd at all and
// returns exactly what the first one did.
func TestReadMemoListAtConstantHashRunsBdOnce(t *testing.T) {
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)

	first, err := store.List(openBothTiers())
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if got := runner.dataReads(); got != 2 {
		t.Fatalf("first List bd calls = %d, want 2 (list + query): %v", got, runner.calls)
	}
	runner.reset()

	second, err := store.List(openBothTiers())
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if got := runner.dataReads(); got != 0 {
		t.Fatalf("second List bd calls = %d, want 0 at an unchanged hash: %v", got, runner.calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("memoized List differs:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	stats := store.ReadMemoStats()
	if stats.Hits != 2 || stats.Misses != 2 {
		t.Fatalf("stats = %+v, want 2 hits and 2 misses", stats)
	}
}

// b) A working hash that moved between two List calls makes the second one
// read bd again.
func TestReadMemoListAfterHashChangeRunsBdAgain(t *testing.T) {
	store, runner, probe := newMemoTestStore(t, memoTestListResponder)

	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	probe.set("hash-b")
	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if got := runner.dataReads(); got != 4 {
		t.Fatalf("bd calls = %d, want 2 + 2 across a hash change: %v", got, runner.calls)
	}
}

// c) A hash that moved while bd was reading proves nothing about which state
// the answer describes, so it is not kept and the next call reads bd again.
func TestReadMemoUnstableHashDuringReadIsNotMemoized(t *testing.T) {
	var probe *memoTestProbe
	moved := 0
	respond := func(args []string) ([]byte, error) {
		// Every bd read of the first List lands on a database that changes
		// underneath it.
		if moved < 2 {
			moved++
			probe.set("hash-during-" + args[0])
		}
		return memoTestListResponder(args)
	}
	store, runner, p := newMemoTestStore(t, respond)
	probe = p

	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if got := store.ReadMemoStats(); got.Unstable != 2 || got.Entries != 0 {
		t.Fatalf("stats after unstable reads = %+v, want 2 unstable and no entries", got)
	}
	runner.reset()
	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if got := runner.dataReads(); got != 2 {
		t.Fatalf("bd calls after unstable reads = %d, want 2 (nothing was kept): %v", got, runner.calls)
	}
	runner.reset()
	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("third List: %v", err)
	}
	if got := runner.dataReads(); got != 0 {
		t.Fatalf("bd calls once the hash settled = %d, want 0: %v", got, runner.calls)
	}
}

// d) A probe that cannot answer leaves every read live.
func TestReadMemoProbeErrorReadsBdEveryTime(t *testing.T) {
	store, runner, probe := newMemoTestStore(t, memoTestListResponder)
	probe.mu.Lock()
	probe.err = errors.New("proxy unreachable")
	probe.mu.Unlock()

	for i := 0; i < 2; i++ {
		if _, err := store.List(openBothTiers()); err != nil {
			t.Fatalf("List %d: %v", i, err)
		}
	}
	if got := runner.dataReads(); got != 4 {
		t.Fatalf("bd calls with a failing probe = %d, want 4: %v", got, runner.calls)
	}
	if got := store.ReadMemoStats(); got.ProbeErrors != 4 || got.Hits != 0 || got.Entries != 0 {
		t.Fatalf("stats = %+v, want 4 probe errors, no hits, no entries", got)
	}
}

// e) bd ready depends on the clock (defer_until, the 48h sort), so it is never
// answered from the memo.
func TestReadMemoReadyIsNeverMemoized(t *testing.T) {
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)
	for i := 0; i < 2; i++ {
		if _, err := store.Ready(); err != nil {
			t.Fatalf("Ready %d: %v", i, err)
		}
	}
	if got := runner.count("ready"); got != 2 {
		t.Fatalf("bd ready calls = %d, want 2", got)
	}
}

// f) A write through this store empties its memo even when the probe (here,
// scripted constant) has not seen the database move.
func TestReadMemoWriteEmptiesMemo(t *testing.T) {
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)
	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	title := "renamed"
	if err := store.Update("ga-1", UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.ReadMemoStats(); got.Entries != 0 || got.Bytes != 0 {
		t.Fatalf("stats after a write = %+v, want an empty memo", got)
	}
	runner.reset()
	if _, err := store.List(openBothTiers()); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if got := runner.dataReads(); got != 2 {
		t.Fatalf("bd calls after a write = %d, want 2: %v", got, runner.calls)
	}
}

// g) bd show resolves a foreign prefix through bd's routes, into another
// database the probe does not watch: such a Get always reads bd.
func TestReadMemoGetForeignPrefixIsNeverMemoized(t *testing.T) {
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)
	for i := 0; i < 2; i++ {
		if _, err := store.Get("ga-1"); err != nil {
			t.Fatalf("Get own %d: %v", i, err)
		}
		if _, err := store.Get("hq-1"); err != nil {
			t.Fatalf("Get foreign %d: %v", i, err)
		}
	}
	shows := map[string]int{}
	runner.mu.Lock()
	for _, call := range runner.calls {
		if len(call) > 3 && call[1] == "show" {
			shows[call[3]]++
		}
	}
	runner.mu.Unlock()
	if shows["ga-1"] != 1 {
		t.Fatalf("bd show ga-1 calls = %d, want 1 (own prefix is memoized)", shows["ga-1"])
	}
	if shows["hq-1"] != 2 {
		t.Fatalf("bd show hq-1 calls = %d, want 2 (foreign prefix stays live)", shows["hq-1"])
	}
}

// h) The guard: only the pure read verbs are memoized, and none whose answer
// depends on the clock or that writes.
func TestReadMemoEligibilityGuard(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"list", "--json", "--status=open", "--limit", "0"}, true},
		{[]string{"query", "--json", "ephemeral=true AND status=open", "--limit", "0"}, true},
		{[]string{"show", "--json", "ga-1"}, true},
		{[]string{"dep", "list", "ga-1", "--json"}, true},
		{[]string{"sql", readyProjectionSQL(), "--json"}, true},
		{[]string{"list", "--json", "--metadata-field", "gc.ready=1"}, true},
		{[]string{"list", "--json", "--label=ready"}, true},

		{nil, false},
		{[]string{"ready", "--json", "--limit", "0"}, false},
		{[]string{"list", "--json", "--ready"}, false},
		{[]string{"list", "--json", "--overdue"}, false},
		{[]string{"list", "--json", "--watch"}, false},
		{[]string{"list", "--json", "--defer-after", "2026-01-01"}, false},
		{[]string{"list", "--json", "--defer-before=2026-01-01"}, false},
		{[]string{"list", "--json", "--due-before", "2026-01-01"}, false},
		{[]string{"list", "--json", "--due-after=2026-01-01"}, false},
		{[]string{"show", "ready"}, false},
		{[]string{"query", "--json", "ephemeral=true AND updated>-1h"}, false},
		{[]string{"query", "--json", "status=open AND created<2026-01-01"}, false},
		{[]string{"query", "--json", "defer_until<now"}, false},
		{[]string{"dep", "add", "ga-1", "ga-2"}, false},
		{[]string{"sql", "--json", "UPDATE issues SET status='open'"}, false},
		{[]string{"sql", "select 1; update issues set status='open'", "--json"}, false},
		{[]string{"update", "--json", "ga-1", "--title", "x"}, false},
		{[]string{"create", "--json", "x"}, false},
		{[]string{"close", "ga-1"}, false},
		{[]string{"version"}, false},
		{[]string{"config", "get", "--json", "k"}, false},
		{[]string{"list", "--help"}, false},
		{[]string{"recompute-blocked", "--json"}, false},
		{[]string{"blocked", "--json"}, false},
	} {
		if got := bdReadMemoEligible(tc.args); got != tc.want {
			t.Errorf("bdReadMemoEligible(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}

	// And the guard is the one in front of the memo: an ineligible argv handed
	// to the pure-read path is read live every time.
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)
	for i := 0; i < 2; i++ {
		if _, err := store.runBDPureRead("list", "--json", "--overdue"); err != nil {
			t.Fatalf("pure read %d: %v", i, err)
		}
	}
	if got := runner.count("list"); got != 2 {
		t.Fatalf("bd list --overdue calls = %d, want 2", got)
	}
}

// i) The memo keeps at most maxBytes, evicting least-recently-used first, and
// never keeps an answer larger than the whole bound.
func TestReadMemoRespectsByteBound(t *testing.T) {
	big := strings.Repeat("x", 200)
	respond := func(args []string) ([]byte, error) {
		if args[0] == "show" && args[2] == "ga-big" {
			return []byte(`[{"id":"ga-big","title":"` + big + `"}]`), nil
		}
		return []byte(`[{"id":"` + args[2] + `"}]`), nil
	}
	runner := &memoTestRunner{respond: respond}
	probe := &memoTestProbe{hash: "h"}
	store := NewBdStoreWithPrefix(t.TempDir(), runner.run, "ga")
	// Each small entry is 31 bytes (16-byte key + 15-byte answer): two fit, three do not.
	const bound = 70
	store.EnableReadMemo(probe.probe, bound)

	for _, id := range []string{"ga-1", "ga-2", "ga-3"} {
		if _, err := store.runBDPureRead("show", "--json", id); err != nil {
			t.Fatal(err)
		}
		if got := store.ReadMemoStats(); got.Bytes > bound {
			t.Fatalf("memo bytes = %d after %s, want <= %d", got.Bytes, id, bound)
		}
	}
	// ga-1 was the least recently used: it is the one evicted.
	runner.reset()
	if _, err := store.runBDPureRead("show", "--json", "ga-3"); err != nil {
		t.Fatal(err)
	}
	if got := runner.count("show"); got != 0 {
		t.Fatalf("most recent entry was evicted: %d bd show calls", got)
	}
	if _, err := store.runBDPureRead("show", "--json", "ga-1"); err != nil {
		t.Fatal(err)
	}
	if got := runner.count("show"); got != 1 {
		t.Fatalf("least recent entry was kept past the bound: %d bd show calls", got)
	}

	runner.reset()
	for i := 0; i < 2; i++ {
		if _, err := store.runBDPureRead("show", "--json", "ga-big"); err != nil {
			t.Fatal(err)
		}
	}
	if got := runner.count("show"); got != 2 {
		t.Fatalf("an answer larger than the bound was memoized: %d bd show calls, want 2", got)
	}
	if got := store.ReadMemoStats(); got.Bytes > bound {
		t.Fatalf("memo bytes = %d, want <= %d", got.Bytes, bound)
	}
}

// A memo hit hands back its own copy: a caller scribbling on it cannot reach
// the next caller's answer.
func TestReadMemoHitReturnsACopy(t *testing.T) {
	store, _, _ := newMemoTestStore(t, memoTestListResponder)
	first, err := store.runBDPureRead("list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.runBDPureRead("list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range second {
		second[i] = 'X'
	}
	third, err := store.runBDPureRead("list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if string(third) != string(first) {
		t.Fatalf("memo entry was mutated through a returned slice: %q", third)
	}
}

// Errors are never memoized: a failed read is retried live on the next call.
func TestReadMemoErrorsAreNeverMemoized(t *testing.T) {
	respond := func(args []string) ([]byte, error) {
		if args[0] == "show" {
			return nil, errors.New("Error: no issue found matching \"ga-404\"")
		}
		return memoTestListResponder(args)
	}
	store, runner, _ := newMemoTestStore(t, respond)
	for i := 0; i < 2; i++ {
		if _, err := store.Get("ga-404"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get %d: err = %v, want ErrNotFound", i, err)
		}
	}
	if got := runner.count("show"); got != 2 {
		t.Fatalf("bd show calls for a missing bead = %d, want 2", got)
	}
}

// Without EnableReadMemo nothing changes: every read runs bd.
func TestReadMemoDisabledByDefault(t *testing.T) {
	runner := &memoTestRunner{respond: memoTestListResponder}
	store := NewBdStoreWithPrefix(t.TempDir(), runner.run, "ga")
	for i := 0; i < 2; i++ {
		if _, err := store.List(openBothTiers()); err != nil {
			t.Fatal(err)
		}
	}
	if got := runner.dataReads(); got != 4 {
		t.Fatalf("bd calls without a memo = %d, want 4", got)
	}
	if got := store.ReadMemoStats(); got != (ReadMemoStats{}) {
		t.Fatalf("stats without a memo = %+v, want zero", got)
	}
}

// memoReconcileResponder answers the cache reconcile's three reads (full list,
// ephemeral query, is_blocked projection) plus the one-time version gate.
func memoReconcileResponder(args []string) ([]byte, error) {
	switch args[0] {
	case "version":
		return []byte("bd version 1.3.2\n"), nil
	case "list":
		return []byte(`[
			{"id":"ga-1","title":"one","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{}},
			{"id":"ga-2","title":"two","status":"in_progress","assignee":"w","issue_type":"task","created_at":"2026-01-01T00:00:01Z","metadata":{},"dependencies":[{"issue_id":"ga-2","depends_on_id":"ga-1","type":"blocks"}]}
		]`), nil
	case "query":
		return []byte(`[{"id":"ga-wisp-1","title":"wisp","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:02Z","metadata":{}}]`), nil
	case "sql":
		return []byte(`[{"id":"ga-1","is_blocked":0},{"id":"ga-2","is_blocked":1},{"id":"ga-wisp-1","is_blocked":0}]`), nil
	}
	return []byte(`[]`), nil
}

type memoReconcileRun struct {
	reads     []int
	events    []string
	beads     map[string]Bead
	freshMove []bool
}

// runMemoReconcile runs two cache reconcile passes over a BdStore, memoized or
// not, and reports what each pass cost and what the cache ended up holding.
func runMemoReconcile(t *testing.T, memo bool) memoReconcileRun {
	t.Helper()
	runner := &memoTestRunner{respond: memoReconcileResponder}
	store := NewBdStoreWithPrefix(t.TempDir(), runner.run, "ga")
	if memo {
		probe := &memoTestProbe{hash: "steady"}
		store.EnableReadMemo(probe.probe, 64<<20)
	}
	var (
		mu     sync.Mutex
		events []string
	)
	cache := NewCachingStoreForTestWithPrefix(store, "ga", func(eventType, beadID string, _ json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, eventType+" "+beadID)
	})
	var run memoReconcileRun
	var lastFresh time.Time
	for pass := 0; pass < 2; pass++ {
		runner.reset()
		cache.runReconciliation()
		run.reads = append(run.reads, runner.dataReads())
		fresh := cache.Stats().LastFreshAt
		run.freshMove = append(run.freshMove, fresh.After(lastFresh))
		lastFresh = fresh
	}
	cache.mu.RLock()
	run.beads = make(map[string]Bead, len(cache.beads))
	for id, b := range cache.beads {
		run.beads[id] = cloneBead(b)
	}
	cache.mu.RUnlock()
	mu.Lock()
	run.events = append([]string(nil), events...)
	mu.Unlock()
	sort.Strings(run.events)
	return run
}

// The supervisor's cache reconcile over a memoized BdStore: the first pass pays
// its three reads, the second at an unchanged hash pays none, and the cache, the
// events it emitted and its freshness clock come out exactly as without a memo.
func TestReadMemoCachingStoreReconcileAtConstantHash(t *testing.T) {
	memo := runMemoReconcile(t, true)
	live := runMemoReconcile(t, false)

	if want := []int{3, 0}; !reflect.DeepEqual(memo.reads, want) {
		t.Fatalf("memoized reconcile bd reads per pass = %v, want %v (list, query ephemeral, sql is_blocked; then none)", memo.reads, want)
	}
	if want := []int{3, 3}; !reflect.DeepEqual(live.reads, want) {
		t.Fatalf("unmemoized reconcile bd reads per pass = %v, want %v", live.reads, want)
	}
	if !reflect.DeepEqual(memo.beads, live.beads) {
		t.Fatalf("cache content differs with the memo:\nmemo: %#v\nlive: %#v", memo.beads, live.beads)
	}
	if !reflect.DeepEqual(memo.events, live.events) {
		t.Fatalf("emitted events differ with the memo:\nmemo: %v\nlive: %v", memo.events, live.events)
	}
	if !reflect.DeepEqual(memo.freshMove, live.freshMove) || !reflect.DeepEqual(memo.freshMove, []bool{true, true}) {
		t.Fatalf("LastFreshAt advanced memo=%v live=%v, want it to advance on both passes in both runs", memo.freshMove, live.freshMove)
	}
	if len(memo.beads) != 3 {
		t.Fatalf("cache holds %d beads, want 3", len(memo.beads))
	}
}

// The ready veto's exact-id blocker lookup is a pure bd list, so at a constant
// hash the second Ready reruns only bd ready itself.
func TestReadMemoReadyBlockerLookupIsMemoized(t *testing.T) {
	respond := func(args []string) ([]byte, error) {
		switch args[0] {
		case "ready":
			return []byte(`[{"id":"ga-dep","title":"dep","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","dependency_count":1,"dependencies":[{"issue_id":"ga-dep","depends_on_id":"ga-blocker","type":"blocks"}]}]`), nil
		case "list":
			return []byte(`[{"id":"ga-blocker","title":"blocker","status":"closed","issue_type":"task","created_at":"2026-01-01T00:00:00Z","metadata":{"gc.work_outcome":"blocked"}}]`), nil
		}
		return []byte(`[]`), nil
	}
	store, runner, _ := newMemoTestStore(t, respond)
	for i := 0; i < 2; i++ {
		got, err := store.Ready()
		if err != nil {
			t.Fatalf("Ready %d: %v", i, err)
		}
		if len(got) != 0 {
			t.Fatalf("Ready %d = %+v, want the dependent vetoed", i, got)
		}
	}
	if got := runner.count("ready"); got != 2 {
		t.Fatalf("bd ready calls = %d, want 2", got)
	}
	if got := runner.count("list"); got != 1 {
		t.Fatalf("bd list blocker lookups = %d, want 1 (the second is a memo hit)", got)
	}
}

// bd dep list resolves its ids like bd show does, so the same own-prefix rule
// applies: a batch with any foreign id stays live.
func TestReadMemoDepListForeignPrefixIsNeverMemoized(t *testing.T) {
	store, runner, _ := newMemoTestStore(t, memoTestListResponder)
	for i := 0; i < 2; i++ {
		if _, err := store.DepList("ga-1", "down"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DepListBatch([]string{"ga-1", "ga-2"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DepListBatch([]string{"ga-1", "hq-2"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := runner.count("dep"); got != 4 {
		t.Fatalf("bd dep list calls = %d, want 4 (2 own-prefix memoized once each, the mixed batch live twice)", got)
	}
}
