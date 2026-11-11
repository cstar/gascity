package beads

// The read memo: a BdStore that keeps the bytes bd already answered, and only
// runs bd again once the database behind its scope has changed.
//
// Every bd read the supervisor issues per tick, except bd ready, is a pure
// function of the state of its scope's Dolt database: list, query, show, dep
// list and the is_blocked projection SQL. gc builds their argv with no flag
// relative to the clock, and in proxied-server mode bd list routes to no other
// database. So when a caller-supplied probe reports the same working-set hash
// before and after a read, and the same argv is asked again under that hash,
// the bytes bd answered then are the bytes it would answer now. The memo hands
// them back without a subprocess; parsing, latching and filtering still run on
// them exactly as on a fresh answer, so callers see no difference.
//
// bd ready is never memoized: it compares defer_until with UTC_TIMESTAMP() and
// sorts on a 48h window, so its answer moves with the clock at a constant hash.
//
// The memo is off unless EnableReadMemo is called. cmd/gc enables it only for
// the supervisor's long-lived proxied stores, where a probe can read the hash
// in-process (cmd/gc/bd_read_memo_probe.go).
//
// Known limit: a database that changes and changes back to byte-identical
// content (an A->B->A within one read) yields the same content-addressed hash
// before and after, so a read that landed on B could be kept under A. gc's
// writes always move updated_at, so that sequence does not occur on the
// supervisor's ledgers.

import (
	"container/list"
	"context"
	"strings"
	"sync"
)

// ReadMemoProbe reports an opaque token naming the current state of a store's
// database. Two equal tokens mean the database held the same content both
// times. An error means the state is unknown and the read goes to bd.
type ReadMemoProbe func(ctx context.Context) (string, error)

// ReadMemoStats is a snapshot of a read memo's counters. Hits and Misses count
// eligible reads; Unstable counts misses whose hash moved while bd was reading
// (nothing kept); ProbeErrors counts reads where the probe could not answer
// (read live, nothing kept). Entries and Bytes are the current occupancy.
type ReadMemoStats struct {
	Hits        int64
	Misses      int64
	Unstable    int64
	ProbeErrors int64
	Entries     int
	Bytes       int64
}

// Add returns the sum of two snapshots, for callers that aggregate stores.
func (s ReadMemoStats) Add(o ReadMemoStats) ReadMemoStats {
	return ReadMemoStats{
		Hits:        s.Hits + o.Hits,
		Misses:      s.Misses + o.Misses,
		Unstable:    s.Unstable + o.Unstable,
		ProbeErrors: s.ProbeErrors + o.ProbeErrors,
		Entries:     s.Entries + o.Entries,
		Bytes:       s.Bytes + o.Bytes,
	}
}

type bdReadMemo struct {
	probe    ReadMemoProbe
	maxBytes int64

	mu         sync.Mutex
	generation uint64 // bumped by every write through the store
	entries    map[string]*list.Element
	lru        *list.List // front = most recently used; values are *bdReadMemoEntry
	bytes      int64
	stats      ReadMemoStats
}

type bdReadMemoEntry struct {
	key  string
	hash string
	out  []byte
}

func (e *bdReadMemoEntry) size() int64 { return int64(len(e.key) + len(e.out)) }

// EnableReadMemo turns on the read memo for this store: eligible pure reads
// are answered from memory while probe reports the hash they were read at.
// maxBytes bounds the memo (keys plus answers); an answer larger than the bound
// is never kept. A nil probe or a non-positive bound leaves the memo off.
//
// Call it before the store is shared; it is safe to call once.
func (s *BdStore) EnableReadMemo(probe ReadMemoProbe, maxBytes int64) {
	if s == nil || probe == nil || maxBytes <= 0 {
		return
	}
	s.readMemoMu.Lock()
	defer s.readMemoMu.Unlock()
	s.readMemo = &bdReadMemo{
		probe:    probe,
		maxBytes: maxBytes,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
	}
}

// ReadMemoEnabled reports whether EnableReadMemo armed this store.
func (s *BdStore) ReadMemoEnabled() bool {
	return s.activeReadMemo() != nil
}

// ReadMemoStats returns the memo's counters, or the zero value when the memo is
// off.
func (s *BdStore) ReadMemoStats() ReadMemoStats {
	m := s.activeReadMemo()
	if m == nil {
		return ReadMemoStats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := m.stats
	stats.Entries = len(m.entries)
	stats.Bytes = m.bytes
	return stats
}

func (s *BdStore) activeReadMemo() *bdReadMemo {
	if s == nil {
		return nil
	}
	s.readMemoMu.RLock()
	defer s.readMemoMu.RUnlock()
	return s.readMemo
}

// noteBDWrite empties the memo after a write through this store. The hash moves
// on a write anyway; this is the belt that does not depend on the probe having
// seen it, and the generation bump keeps a read that raced the write from
// installing what it read before it.
func (s *BdStore) noteBDWrite() {
	m := s.activeReadMemo()
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation++
	m.entries = make(map[string]*list.Element)
	m.lru.Init()
	m.bytes = 0
}

// runBDPureRead runs a read whose answer depends only on the scope's database
// state, through the memo when it is armed. It retries transient transport
// failures exactly as runBDTransientRead does.
func (s *BdStore) runBDPureRead(args ...string) ([]byte, error) {
	return s.runBDPureReadWith(s.runBDTransientRead, args...)
}

// runBDPureReadWith is runBDPureRead over a caller-chosen live read, for the one
// pure read (the is_blocked projection) that runs bd without the transient
// retry wrapper and must keep doing so.
func (s *BdStore) runBDPureReadWith(live func(args ...string) ([]byte, error), args ...string) ([]byte, error) {
	m := s.activeReadMemo()
	if m == nil || !bdReadMemoEligible(args) {
		return live(args...)
	}
	ctx := context.Background()
	before, err := m.probe(ctx)
	if err != nil {
		m.count(func(st *ReadMemoStats) { st.ProbeErrors++ })
		return live(args...)
	}
	key := strings.Join(args, "\x00")
	out, generation, hit := m.lookup(key, before)
	if hit {
		return out, nil
	}
	out, err = live(args...)
	if err != nil {
		// Never memoized, not-found included: an absent bead is often about to
		// be created, and an error says nothing about the database state.
		return out, err
	}
	after, err := m.probe(ctx)
	switch {
	case err != nil:
		m.count(func(st *ReadMemoStats) { st.ProbeErrors++ })
	case after != before:
		m.count(func(st *ReadMemoStats) { st.Unstable++ })
	default:
		m.store(key, before, generation, out)
	}
	return out, nil
}

// lookup answers key at hash with a private copy of the memoized bytes. On a
// miss it counts the miss and returns the generation a later store must still
// match.
func (m *bdReadMemo) lookup(key, hash string) ([]byte, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		entry := el.Value.(*bdReadMemoEntry)
		if entry.hash == hash {
			m.lru.MoveToFront(el)
			m.stats.Hits++
			return append([]byte(nil), entry.out...), m.generation, true
		}
		// Another hash: this entry can never answer again.
		m.removeLocked(el)
	}
	m.stats.Misses++
	return nil, m.generation, false
}

func (m *bdReadMemo) store(key, hash string, generation uint64, out []byte) {
	entry := &bdReadMemoEntry{key: key, hash: hash, out: append([]byte(nil), out...)}
	if entry.size() > m.maxBytes {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		// A write went through this store while bd was reading.
		return
	}
	if el, ok := m.entries[key]; ok {
		m.removeLocked(el)
	}
	m.entries[key] = m.lru.PushFront(entry)
	m.bytes += entry.size()
	for m.bytes > m.maxBytes {
		oldest := m.lru.Back()
		if oldest == nil {
			break
		}
		m.removeLocked(oldest)
	}
}

func (m *bdReadMemo) removeLocked(el *list.Element) {
	entry := el.Value.(*bdReadMemoEntry)
	m.lru.Remove(el)
	delete(m.entries, entry.key)
	m.bytes -= entry.size()
}

func (m *bdReadMemo) count(fn func(*ReadMemoStats)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.stats)
}

// bdReadMemoLiveOnlyFlags are the argv tokens that keep a read live: the
// ones whose answer moves with the clock at a constant database state, and
// help output, which is not a database read at all.
var bdReadMemoLiveOnlyFlags = map[string]bool{
	"ready":           true,
	"--ready":         true,
	"--overdue":       true,
	"--watch":         true,
	"--defer-after":   true,
	"--defer-before":  true,
	"--due-before":    true,
	"--due-after":     true,
	"--help":          true,
	"-h":              true,
	"--stale":         true,
	"--updated-after": true,
}

// bdReadMemoTemporalQueryFields are the bd query fields whose comparisons can
// be relative to now ("updated>-1h"); gc's own query clauses never use them.
var bdReadMemoTemporalQueryFields = map[string]bool{
	"created":     true,
	"created_at":  true,
	"updated":     true,
	"updated_at":  true,
	"closed_at":   true,
	"defer":       true,
	"defer_until": true,
	"due":         true,
	"due_at":      true,
}

// bdReadMemoEligible is the guard in front of the memo: only list, query, show,
// dep list and a single SELECT statement through bd sql, and none whose argv
// names something that depends on the clock. Anything else, writes included,
// is read live.
func bdReadMemoEligible(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "list", "show":
	case "query":
		if !bdReadMemoQueryIsTimeless(args[1:]) {
			return false
		}
	case "dep":
		if len(args) < 2 || args[1] != "list" {
			return false
		}
	case "sql":
		if !bdReadMemoSQLIsSingleSelect(args[1:]) {
			return false
		}
	default:
		return false
	}
	for _, arg := range args {
		name := arg
		if strings.HasPrefix(arg, "-") {
			if i := strings.IndexByte(arg, '='); i >= 0 {
				name = arg[:i]
			}
		}
		if bdReadMemoLiveOnlyFlags[name] {
			return false
		}
	}
	return true
}

func bdReadMemoQueryIsTimeless(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		for _, token := range strings.Fields(arg) {
			field := token
			if i := strings.IndexAny(token, "=<>!~"); i >= 0 {
				field = token[:i]
			}
			if bdReadMemoTemporalQueryFields[strings.ToLower(strings.Trim(field, "()"))] {
				return false
			}
		}
	}
	return true
}

func bdReadMemoSQLIsSingleSelect(args []string) bool {
	statement := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if statement != "" {
			return false
		}
		statement = arg
	}
	statement = strings.TrimSpace(statement)
	if strings.Contains(statement, ";") {
		return false
	}
	lower := strings.ToLower(statement)
	return strings.HasPrefix(lower, "select ") && !strings.Contains(lower, "now()") && !strings.Contains(lower, "utc_timestamp") && !strings.Contains(lower, "current_timestamp")
}
