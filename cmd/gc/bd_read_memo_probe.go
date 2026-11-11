package main

// Supervisor bd stores probe their Dolt working hash in-process.
//
// The supervisor holds one long-lived BdStore per scope and, every tick, runs
// the same bd list/query/show reads against ledgers that almost never changed
// since the last tick. internal/beads can keep those answers while the scope's
// database is unchanged (bdstore_read_memo.go); this file supplies the probe
// that says whether it is, and decides which stores get one.
//
// The probe reads @@<db>_working, the content hash of the database's working
// set (uncommitted changes included), with one point query over a pooled
// connection to the scope's bd proxy. It is the same proxy bd itself talks to,
// reached the way bd and proxyendpoint.Probe reach it: root, no password, on
// proxyendpoint.Host. All scopes behind one proxy share one *sql.DB with no
// database selected, so a city's twenty scopes cost one small pool, not twenty.
//
// A store gets the memo only when every condition below holds; otherwise it
// keeps reading bd for every call, as before, and the reason is logged:
//
//   - the store is a BdStore (under the bead-policy wrapper);
//   - metadata.json declares backend dolt, dolt_mode proxied-server and a
//     dolt_database made of [A-Za-z0-9_] (it is spliced into the variable name);
//   - the scope's proxy record is valid and its process is the live proxy for
//     that root (proxyendpoint.Inspect);
//   - the proxy never retires itself (idle policy "never"), so a pooled
//     connection cannot hold open a proxy bd has decided to stop.
//
// Only the supervisor (controllerState) arms memos; one-shot CLI commands never
// do.
//
// ROLLBACK WITHOUT A REBUILD: set GC_BD_READ_MEMO=off in the supervisor's
// environment. It is read whenever the supervisor builds its stores (start and
// every config reload), and leaves every store reading bd exactly as before.
//
// Every five minutes at most, the supervisor logs one line aggregating
// ReadMemoStats over its stores ("bd read memo: stats ..."), for measurement.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/doltpool"
)

const (
	// bdReadMemoEnv is the kill switch: "off" disables every memo.
	bdReadMemoEnv = "GC_BD_READ_MEMO"
	// bdReadMemoMaxBytes bounds each store's memo.
	bdReadMemoMaxBytes int64 = 64 << 20
	// bdReadMemoProbeTimeout bounds one working-hash read.
	bdReadMemoProbeTimeout = 2 * time.Second
	// bdReadMemoEndpointRecheck is how long a resolved proxy port is trusted
	// before the record and process table are inspected again.
	bdReadMemoEndpointRecheck = 30 * time.Second
	// bdReadMemoStatsInterval is the period of the aggregate stats line.
	bdReadMemoStatsInterval = 5 * time.Minute
	// bdReadMemoProbeUser is the account bd and proxyendpoint.Probe use.
	bdReadMemoProbeUser = "root"
)

var bdReadMemoDatabasePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Test seams.
var (
	bdReadMemoGetenv       = os.Getenv
	bdReadMemoProcessTable = proxyendpoint.DefaultProcessTable
	bdReadMemoQueryHash    = queryDoltWorkingHash
	bdReadMemoNow          = time.Now
)

// bdReadMemoTarget is where a scope's probe reads: the proxy root and the
// database whose working hash names the scope's state.
type bdReadMemoTarget struct {
	proxyRoot string
	database  string
}

// bdReadMemoTargetFor decides whether the store rooted at scopeRoot may be
// memoized. It returns the target, or the reason it may not.
func bdReadMemoTargetFor(scopeRoot string) (bdReadMemoTarget, string, bool) {
	if strings.EqualFold(strings.TrimSpace(bdReadMemoGetenv(bdReadMemoEnv)), "off") {
		return bdReadMemoTarget{}, bdReadMemoEnv + "=off", false
	}
	beadsDir := filepath.Join(scopeRoot, ".beads")
	metaPath := filepath.Join(beadsDir, proxyendpoint.MetadataFileName)
	data, err := os.ReadFile(metaPath) // #nosec G304 -- metaPath is the scope's own metadata.json
	if err != nil {
		return bdReadMemoTarget{}, fmt.Sprintf("read %s: %v", metaPath, err), false
	}
	var meta struct {
		Backend      string `json:"backend"`
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return bdReadMemoTarget{}, fmt.Sprintf("parse %s: %v", metaPath, err), false
	}
	if meta.Backend != "dolt" {
		return bdReadMemoTarget{}, fmt.Sprintf("backend %q is not dolt", meta.Backend), false
	}
	if meta.DoltMode != "proxied-server" {
		return bdReadMemoTarget{}, fmt.Sprintf("dolt_mode %q is not proxied-server", meta.DoltMode), false
	}
	if !bdReadMemoDatabasePattern.MatchString(meta.DoltDatabase) {
		return bdReadMemoTarget{}, fmt.Sprintf("dolt_database %q is not [A-Za-z0-9_]+", meta.DoltDatabase), false
	}
	root, err := proxyendpoint.ProviderRoot(scopeRoot)
	if err != nil {
		return bdReadMemoTarget{}, fmt.Sprintf("resolve proxy root: %v", err), false
	}
	ep := proxyendpoint.Inspect(root, bdReadMemoProcessTable())
	if !ep.Verdict.Live() {
		reason := fmt.Sprintf("proxy endpoint %s is %s", root, ep.Verdict)
		if ep.Err != nil {
			reason += ": " + ep.Err.Error()
		}
		return bdReadMemoTarget{}, reason, false
	}
	sidecar, err := proxyendpoint.ReadSidecar(beadsDir)
	if err != nil {
		return bdReadMemoTarget{}, fmt.Sprintf("read proxy sidecar: %v", err), false
	}
	if idle := proxyendpoint.ResolveIdlePolicy(sidecar, ep.Liveness.IdlePolicy); idle.Kind != proxyendpoint.IdleNever {
		return bdReadMemoTarget{}, fmt.Sprintf("proxy idle policy %s can retire the proxy under a pooled connection", idle), false
	}
	return bdReadMemoTarget{proxyRoot: root, database: meta.DoltDatabase}, "", true
}

// enableBdReadMemo arms the read memo on store's BdStore when its scope
// qualifies, and logs why it does not otherwise. Stores that are not bd stores
// run no bd and are skipped silently. It reports whether a memo was armed.
func enableBdReadMemo(store beads.Store, scopeRoot, label string) bool {
	base, _, _ := unwrapBeadPolicyStore(store)
	bd, ok := base.(*beads.BdStore)
	if !ok || bd == nil {
		return false
	}
	if bd.ReadMemoEnabled() {
		return true
	}
	target, reason, ok := bdReadMemoTargetFor(scopeRoot)
	if !ok {
		log.Printf("bd read memo: %s off: %s", label, reason)
		return false
	}
	probe := &bdWorkingHashProbe{target: target, endpoints: bdReadMemoEndpoints}
	bd.EnableReadMemo(probe.read, bdReadMemoMaxBytes)
	log.Printf("bd read memo: %s on (database %s, proxy root %s)", label, target.database, target.proxyRoot)
	return true
}

// bdWorkingHashProbe is one scope's beads.ReadMemoProbe.
type bdWorkingHashProbe struct {
	target    bdReadMemoTarget
	endpoints *bdReadMemoEndpointCache
}

// read returns the proxy generation and the database's working hash. The
// generation makes a restarted proxy a different state even if the data hash
// happens to match.
func (p *bdWorkingHashProbe) read(ctx context.Context) (string, error) {
	ep, err := p.endpoints.resolve(p.target.proxyRoot)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, bdReadMemoProbeTimeout)
	defer cancel()
	hash, err := bdReadMemoQueryHash(ctx, ep.port, p.target.database)
	if err != nil {
		// The proxy may have restarted on another port: read proxy.pid again
		// on the next call.
		p.endpoints.invalidate(p.target.proxyRoot)
		return "", err
	}
	return ep.generation + "/" + hash, nil
}

// queryDoltWorkingHash reads @@<database>_working over the shared pool for the
// proxy on port. database has been checked against bdReadMemoDatabasePattern.
func queryDoltWorkingHash(ctx context.Context, port int, database string) (string, error) {
	if !bdReadMemoDatabasePattern.MatchString(database) {
		return "", fmt.Errorf("bd read memo: invalid database name %q", database)
	}
	db, err := doltpool.Open(proxyendpoint.Host, strconv.Itoa(port), bdReadMemoProbeUser, "", "")
	if err != nil {
		return "", err
	}
	var hash sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT @@"+database+"_working").Scan(&hash); err != nil {
		return "", fmt.Errorf("bd read memo: read @@%s_working: %w", database, err)
	}
	if !hash.Valid || strings.TrimSpace(hash.String) == "" {
		return "", fmt.Errorf("bd read memo: @@%s_working is empty", database)
	}
	return hash.String, nil
}

// bdReadMemoEndpointCache remembers the port of each proxy root the probes
// read, so a probe costs one query and not a record read plus a process-table
// inspection. It is process-wide: a proxy is a property of the host, not of a
// controller.
type bdReadMemoEndpointCache struct {
	mu     sync.Mutex
	byRoot map[string]bdReadMemoEndpoint
}

type bdReadMemoEndpoint struct {
	port       int
	generation string
	checkedAt  time.Time
}

var bdReadMemoEndpoints = &bdReadMemoEndpointCache{byRoot: make(map[string]bdReadMemoEndpoint)}

func (c *bdReadMemoEndpointCache) resolve(root string) (bdReadMemoEndpoint, error) {
	now := bdReadMemoNow()
	c.mu.Lock()
	ep, ok := c.byRoot[root]
	c.mu.Unlock()
	if ok && now.Sub(ep.checkedAt) < bdReadMemoEndpointRecheck {
		return ep, nil
	}
	inspected := proxyendpoint.Inspect(root, bdReadMemoProcessTable())
	if !inspected.Verdict.Live() {
		c.invalidate(root)
		err := fmt.Errorf("bd read memo: proxy endpoint %s is %s", root, inspected.Verdict)
		if inspected.Err != nil {
			err = fmt.Errorf("%w: %w", err, inspected.Err)
		}
		return bdReadMemoEndpoint{}, err
	}
	ep = bdReadMemoEndpoint{
		port:       inspected.Record.Port,
		generation: inspected.Record.Birth + ":" + strconv.Itoa(inspected.Record.Port),
		checkedAt:  now,
	}
	if ep.port <= 0 {
		return bdReadMemoEndpoint{}, errors.New("bd read memo: proxy record has no port")
	}
	c.mu.Lock()
	c.byRoot[root] = ep
	c.mu.Unlock()
	return ep, nil
}

func (c *bdReadMemoEndpointCache) invalidate(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byRoot, root)
}

// bdReadMemoStatsOf sums the read-memo counters of every memoized BdStore
// under stores, each counted once.
func bdReadMemoStatsOf(stores []beads.Store) (beads.ReadMemoStats, int) {
	var total beads.ReadMemoStats
	seen := make(map[*beads.BdStore]bool)
	for _, store := range stores {
		bd, ok := uncachedBeadStore(store).(*beads.BdStore)
		if !ok || bd == nil || seen[bd] || !bd.ReadMemoEnabled() {
			continue
		}
		seen[bd] = true
		total = total.Add(bd.ReadMemoStats())
	}
	return total, len(seen)
}

// formatBdReadMemoStats renders the aggregate stats line.
func formatBdReadMemoStats(stats beads.ReadMemoStats, stores int) string {
	ratio := 0.0
	if lookups := stats.Hits + stats.Misses; lookups > 0 {
		ratio = float64(stats.Hits) / float64(lookups)
	}
	return fmt.Sprintf("bd read memo: stats stores=%d hits=%d misses=%d hit_ratio=%.3f unstable=%d probe_errors=%d entries=%d bytes=%d",
		stores, stats.Hits, stats.Misses, ratio, stats.Unstable, stats.ProbeErrors, stats.Entries, stats.Bytes)
}

// runBdReadMemoStatsLog logs the aggregate stats of the controller's stores
// every bdReadMemoStatsInterval until ctx ends. A city with no memoized store
// logs nothing.
func (cs *controllerState) runBdReadMemoStatsLog(ctx context.Context) {
	ticker := time.NewTicker(bdReadMemoStatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		stores := make([]beads.Store, 0, 1)
		if city := cs.CityBeadStore(); city != nil {
			stores = append(stores, city)
		}
		for _, store := range cs.BeadStores() {
			stores = append(stores, store)
		}
		if stats, n := bdReadMemoStatsOf(stores); n > 0 {
			log.Print(formatBdReadMemoStats(stats, n))
		}
	}
}
