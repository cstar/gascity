package main

// The execution backstop: a claim that never becomes execution.
//
// Every backstop that came before this one ends at the CLAIM. nudgeStalledPoolClaims
// clears the instant its trigger bead flips to in_progress ("the slot is doing
// its job"), and nudgeStalledPoolContinuations only ever looks at the OPEN
// successor of a step that already completed. So the one state neither covers is
// the one the incident produced: a pool slot holding an in-progress bead, idle at
// its prompt, having claimed and then ended its turn without starting the work.
// From there nothing in the fleet converges — the bead is not open, so no claim
// probe wants it; the session is alive, so no crash lane touches it — until the
// session is recycled 15-85 minutes later and the dead-assignee reopen releases
// the claim.
//
// This predicate closes that with the same bounded shape as its two siblings:
// observe, nudge, back off, give up. It re-delivers the agent's OWN configured
// claim nudge, which is idempotent by construction — re-running `gc hook --claim`
// on a bead this session already owns returns action=work
// reason=existing_assignment (the ga-i44k invariant), so a slot that was merely
// slow re-reads its assignment instead of being handed a second one.
//
// # The churn guard is provider idleness, and it is the predicate's job
//
// The shared engine does not consult the runtime; it keys purely on bead state.
// That is enough for the sibling predicates, because their outstanding condition
// (a bead still OPEN) is structurally invisible to a working agent — the moment
// it claims, the predicate stops matching. This one's condition is the exact
// opposite: an in-progress bead is what a working agent looks like. So the
// predicate holds unless the runtime itself reports no activity for at least the
// grace window, and an UNKNOWN activity signal (an error, or a runtime that
// reports none) holds too. That is the #312 lesson stated in the one place it
// applies: never nudge on "we cannot tell".
//
// # Execution evidence vetoes the escalation, never the nudge
//
// The activity clock is pane output, so a seat that started its step and is
// running a long silent command (a full test suite, a build) reads exactly
// like one that never started. The predicate therefore persists execution
// evidence on the row (executionClaimStartedKey): an execution.step_started
// of the claimed bead, recorded by `gc hook --claim` from this session at or
// after the row's last_woke_at. A seat with that evidence still gets its
// bounded nudges once quiet past the window (the held-claim text, which
// carries no --drain-ack, when its template configures none or one that
// does), but when they are spent it is held as executing rather than
// drained. A seat with no evidence (never started, another session's event,
// an event from before this incarnation's wake) is unchanged.
//
// The marker also carries the incarnation's instance_token, so a later
// incarnation holding the same claim starts its own grace window and attempt
// count instead of inheriting an expired one.
//
// (Backport to v1.5.1 of #7911, #7922, #7984 and the latch clear of #8592.)
//
// # Exhaustion hands the session to a lane that already converges
//
// At the attempt cap the stall becomes a typed event and the session is handed
// to the ordinary drain path, whose recycle -> dead-assignee-reopen chain is the
// one that converges today — only now in a bounded ~11 minutes rather than on
// recycle roulette. Restart-with-backoff is established framework liveness (the
// same shape as health patrol); the decision about the WORK is still the agent's.
// The escalation is latched on the session bead so it fires once per stalled
// claim and incarnation, not once per tick. The latch is honored only for the
// incarnation it escalated and only for executionStalledLatchRetryAfter: the
// drain it requests lives in the controller's memory, so a supervisor restart
// can lose it, and a latch that outlives its drain would otherwise silence the
// backstop for the rest of the seat's life.
//
// # Holds are named
//
// Every hold leaves a breadcrumb on the session bead (executionClaimHoldKey), so
// a backstop that is holding can be told apart from one that is broken.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Session-bead metadata keys for the execution backstop. Persisted for the same
// reason the sibling backstops persist theirs: a controller restart must resume
// the state machine, never replay it (test-5il).
const (
	executionClaimNudgeWorkKey     = "execution_claim_nudge_work"
	executionClaimNudgeRootKey     = "execution_claim_nudge_root"
	executionClaimNudgeStoreRefKey = "execution_claim_nudge_store_ref"
	executionClaimNudgeCountKey    = "execution_claim_nudge_count"
	executionClaimNudgeAtKey       = "execution_claim_nudge_at"
	// executionClaimNudgeTokenKey records the instance_token of the
	// incarnation the marker observed: a later incarnation holding the same
	// claim starts its own grace window and attempt count.
	executionClaimNudgeTokenKey = "execution_claim_nudge_token"
	// executionClaimStartedKey persists execution evidence as
	// "<instance_token>|<bead id>" once the backstop has seen an
	// execution.step_started of that claimed bead from this session since its
	// last wake. A value for another token or bead is no evidence. The claim
	// marker's clear leaves it: a stamped claim can drop out of the
	// in_progress snapshot for a pass and come back.
	executionClaimStartedKey = "execution_claim_started"
	// executionClaimNudgeStalledKey latches the one-shot escalation so the
	// typed event and the drain request fire once per stalled claim rather than
	// once per tick for as long as the claim is held.
	executionClaimNudgeStalledKey = "execution_claim_nudge_stalled"
	// executionClaimNudgeStalledTokenKey records the instance_token of the
	// incarnation the latch escalated. A later incarnation holding the same
	// claim (the drain stopped the runtime, the supervisor restarted before the
	// claim was released, and boot woke the row again) is not covered by it.
	executionClaimNudgeStalledTokenKey = "execution_claim_nudge_stalled_token"
	// executionClaimHoldKey is the breadcrumb naming the gate that held this
	// session on the most recent evaluation, as "<reason> <RFC3339 time>". It
	// is written only when the reason changes and cleared once the backstop can
	// act again or the claim is gone.
	executionClaimHoldKey = "execution_claim_hold"
)

// Execution-backstop hold reasons recorded in executionClaimHoldKey, alongside
// the engine-level backstopHold* reasons.
const (
	executionHoldMultiClaim = "multi_claim"
	executionHoldNotQuiet   = "not_quiet"
	// executionHoldExecuting names execution evidence vetoing the
	// escalation: the claimed bead's step started this incarnation, so once
	// its nudges are spent, or it has none to send, the seat is held rather
	// than drained.
	executionHoldExecuting = "executing"
)

// executionClaimNudgeLabel names the execution backstop in its log lines.
const executionClaimNudgeLabel = "execution-claim-nudge"

// defaultHeldClaimNudge is the claim nudge for a seat whose claim (the %s)
// has started its step. It carries no --drain-ack: a started seat must never
// be told to acknowledge a drain.
const defaultHeldClaimNudge = "You hold %s and its step has started. Continue it now (run gc hook --claim --json to re-read your assignment). If you are waiting on something, say what, then stop."

// executionStalledLatchRetryAfter bounds how long a latch silences
// re-escalation of the SAME incarnation. The drain it requested normally
// finishes within defaultDrainTimeout; a latch this old whose session is still
// running and still holding the claim means the drain was lost (the drain
// tracker is in-memory and a supervisor restart discards it). Re-requesting is
// safe: beginSessionDrainInfo is a no-op for a session already draining.
const executionStalledLatchRetryAfter = 30 * time.Minute

// nudgeStalledPoolExecution re-delivers the configured claim nudge to a pool slot
// that HOLDS an in-progress claim it never started executing, and escalates once
// the bounded attempts are spent.
//
// work/workStores/workStoreRefs are the reconciler's index-aligned assigned-work
// snapshot; requestDrain is the existing drain request (drainOps.setDrain), taken
// as a function so this file needs no reconciler wiring of its own. A partial
// snapshot is not evidence of a stall, so it disables the predicate for that tick.
func nudgeStalledPoolExecution(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	snapshotPartial bool,
	now time.Time,
	rec events.Recorder,
	requestDrain func(sessionBead beads.Bead) error,
	stdout io.Writer,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return // hot reconcile path: never panic on a half-built dependency
	}
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, "execution-claim-nudge", poolExecutionBackstop{
		cfg:          cfg,
		sp:           sp,
		now:          now,
		rec:          rec,
		requestDrain: requestDrain,
		claims:       newExecutionClaimSnapshot(work, workStores, workStoreRefs),
		store:        store,
		stdout:       stdout,
	})
}

// executionClaim is one in-progress claim from the assigned-work snapshot, kept
// with the store handle its live re-read must use.
type executionClaim struct {
	BeadID   string
	RootID   string
	StoreRef string
	Assignee string
	Store    beads.Store
	// The claimed bead's lifecycle identity, as the hook stamps it: its
	// graph step, the session that holds it, and its kind.
	StepID, SessionID, SessionName, Kind string
}

// executionLifecycleCandidate reports whether claim can carry a step_started
// from session s: the hook's own gate (hookClaimLifecycleCandidate), read off
// the snapshot's metadata. Only such a claim reads the event tail, so a claim
// that can never carry the event does not re-read it every pass.
func executionLifecycleCandidate(claim executionClaim, s beads.Bead) bool {
	if claim.RootID == "" || claim.StepID == "" || beadmeta.IsControlKind(claim.Kind) ||
		claim.SessionID == "" || claim.SessionID != strings.TrimSpace(s.ID) {
		return false
	}
	name := strings.TrimSpace(s.Metadata["session_name"])
	return name == "" || claim.SessionName == name
}

// executionClaimSnapshot indexes in-progress claims by their exact assignee
// string. Resolution is by identity rather than by bead id because the question
// this predicate asks is "what does THIS session hold", and the same bead id can
// exist in independent stores.
type executionClaimSnapshot struct {
	byAssignee map[string][]executionClaim
}

func newExecutionClaimSnapshot(work []beads.Bead, stores []beads.Store, storeRefs []string) executionClaimSnapshot {
	snapshot := executionClaimSnapshot{byAssignee: make(map[string][]executionClaim)}
	for i, wb := range work {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || !strings.EqualFold(strings.TrimSpace(wb.Status), "in_progress") {
			continue
		}
		if strings.TrimSpace(wb.ID) == "" {
			continue
		}
		claim := executionClaim{
			BeadID:      wb.ID,
			RootID:      strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]),
			Assignee:    assignee,
			StepID:      strings.TrimSpace(wb.Metadata[beadmeta.StepIDMetadataKey]),
			SessionID:   strings.TrimSpace(wb.Metadata[beadmeta.SessionIDMetadataKey]),
			SessionName: strings.TrimSpace(wb.Metadata[beadmeta.SessionNameMetadataKey]),
			Kind:        strings.TrimSpace(wb.Metadata[beadmeta.KindMetadataKey]),
		}
		if i < len(storeRefs) {
			claim.StoreRef = normalizeIdleClaimStoreRef(storeRefs[i])
		}
		if i < len(stores) {
			claim.Store = stores[i]
		}
		snapshot.byAssignee[assignee] = append(snapshot.byAssignee[assignee], claim)
	}
	return snapshot
}

// forIdentities returns the deduped claims held by any of the session's current
// identities, in a stable order so a session with several claims resolves the
// same way on every tick.
func (s executionClaimSnapshot) forIdentities(identities []string) []executionClaim {
	seen := make(map[string]struct{})
	var out []executionClaim
	for _, identity := range identities {
		for _, claim := range s.byAssignee[identity] {
			key := claim.StoreRef + "\x00" + claim.BeadID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, claim)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StoreRef != out[j].StoreRef {
			return out[i].StoreRef < out[j].StoreRef
		}
		return out[i].BeadID < out[j].BeadID
	})
	return out
}

// poolExecutionBackstop is the backstopPredicate for a pool slot that claimed a
// bead and never executed it.
type poolExecutionBackstop struct {
	cfg          *config.City
	sp           runtime.Provider
	now          time.Time
	rec          events.Recorder
	requestDrain func(sessionBead beads.Bead) error
	claims       executionClaimSnapshot
	// store and stdout let resolve record which gate held; the engine sees
	// only the hold itself.
	store  beads.Store
	stdout io.Writer
}

func (p poolExecutionBackstop) governs(s beads.Bead) bool {
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true"
}

// resolve reports an outstanding stall only when all of it holds: the session
// holds exactly ONE in-progress claim under a current identity, and the runtime
// says the session has been quiet for at least the grace window.
//
// Exactly one, because the pacing state names a single bead: a session juggling
// several claims is demonstrably doing something, and picking one of them to
// nudge about would make the persisted marker a lie. Ambiguity holds rather than
// clears, so a transient multi-claim tick cannot reset a window already running.
func (p poolExecutionBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, sessName string) (backstopTarget, backstopResolution) {
	claims := p.claims.forIdentities(currentSessionAssigneeIdentities(s))
	switch len(claims) {
	case 0:
		return backstopTarget{}, backstopResolutionClear
	case 1:
		// Continue below.
	default:
		// s is resolve's copy, but it shares the engine's Metadata map, so
		// the breadcrumb written here is visible to the rest of the tick.
		p.observeHold(p.store, &s, executionHoldMultiClaim, p.stdout)
		return backstopTarget{}, backstopResolutionHold
	}
	claim := claims[0]
	// Persist execution evidence, busy or quiet: until this incarnation's
	// claim is stamped, each pass looks for it, and the first sighting stamps
	// the row, so the evidence outlives the event log's bounded tail.
	started := p.markedStarted(s, claim.BeadID)
	if !started && executionLifecycleCandidate(claim, s) && p.executedSinceWake(s, claim.BeadID) {
		started = p.markStarted(&s, claim.BeadID)
	}
	if !p.sessionIsQuiet(sessName) {
		p.observeHold(p.store, &s, executionHoldNotQuiet, p.stdout)
		return backstopTarget{}, backstopResolutionHold
	}
	target := backstopTarget{
		ID:       claim.BeadID,
		RootID:   claim.RootID,
		StoreRef: claim.StoreRef,
		Assignee: claim.Assignee,
		Store:    claim.Store,
	}
	// Execution evidence vetoes the escalation, never the nudge: a seat that
	// started its step is not drained, but a seat whose turn ended while it
	// still holds the claim is nudged like any other.
	if same, attempts, last := p.state(s, target); same && started && p.escalatesNext(s, attempts, last) {
		p.holdExecuting(&s, sessName, claim.BeadID, attempts)
		return backstopTarget{}, backstopResolutionHold
	}
	return target, backstopResolutionOutstanding
}

// holdExecuting records the executing hold on s and logs it once as it is
// entered: the breadcrumb is written only when the reason changes.
func (p poolExecutionBackstop) holdExecuting(s *beads.Bead, sessName, beadID string, attempts int) {
	entering := !holdIs(*s, executionHoldExecuting)
	p.observeHold(p.store, s, executionHoldExecuting, p.stdout)
	if entering && holdIs(*s, executionHoldExecuting) {
		fmt.Fprintf(p.stdout, //nolint:errcheck // best-effort
			"execution-claim-nudge: %s holds started %s after %d/%d nudge attempts; holding as executing (no drain)\n",
			sessName, beadID, attempts, idleClaimNudgeMaxAttempts)
	}
}

// holdIs reports whether s's hold breadcrumb names reason.
func holdIs(s beads.Bead, reason string) bool {
	cur := strings.TrimSpace(s.Metadata[executionClaimHoldKey])
	return cur == reason || strings.HasPrefix(cur, reason+" ")
}

// escalatesNext reports whether the engine's next action on s, given its
// pacing, is the terminal escalation (exhausted): the attempts are spent, or a
// nudge is due with no content to deliver, which the engine escalates at once.
// The second mirrors runNudgeBackstop's empty-content shortcut; change the two
// in lockstep.
func (p poolExecutionBackstop) escalatesNext(s beads.Bead, attempts int, last time.Time) bool {
	switch decideBackstopAction(attempts, last, p.now) {
	case backstopActionExhausted:
		return true
	case backstopActionNudge:
		return p.content(s) == ""
	default:
		return false
	}
}

// executionEvidenceTailBytes bounds how far back the execution-evidence read
// walks the event log from its end: the evidence it looks for is from the
// seat's current incarnation, and it is persisted once seen.
const executionEvidenceTailBytes = 8 << 20

// executionEvidenceTailEvents caps the matching events that read returns.
const executionEvidenceTailEvents = 20

// executedSinceWake reports execution evidence for the claim: an
// execution.step_started of beadID, from this session, at or after the row's
// last_woke_at. It answers only from a recorder that can be read, and with a
// wake time to read from; otherwise it reports no evidence and the activity
// clock decides, as before.
func (p poolExecutionBackstop) executedSinceWake(s beads.Bead, beadID string) bool {
	tail, ok := p.rec.(events.TailProvider)
	if !ok || strings.TrimSpace(beadID) == "" {
		return false
	}
	woke := parseRFC3339OrZero(s.Metadata["last_woke_at"])
	if woke.IsZero() {
		return false
	}
	evts, err := tail.ListTail(events.Filter{
		Type: events.ExecutionStepStarted, Subject: beadID, Since: woke, MaxScanBytes: executionEvidenceTailBytes,
	}, executionEvidenceTailEvents)
	if err != nil {
		return false
	}
	for _, e := range evts {
		if e.SessionID == s.ID || e.Actor == s.ID {
			return true
		}
	}
	return false
}

// startedValue is executionClaimStartedKey's value for s's incarnation and
// beadID.
func startedValue(s beads.Bead, beadID string) string {
	return strings.TrimSpace(s.Metadata["instance_token"]) + "|" + strings.TrimSpace(beadID)
}

// markedStarted reports the row's persisted execution evidence for its
// current incarnation and beadID.
func (p poolExecutionBackstop) markedStarted(s beads.Bead, beadID string) bool {
	return strings.TrimSpace(beadID) != "" && strings.TrimSpace(s.Metadata[executionClaimStartedKey]) == startedValue(s, beadID)
}

// markStarted stamps the row's execution evidence for its incarnation and
// beadID, and reports that the evidence holds (it does even when the write
// fails: the sighting is this pass's evidence, and the next pass reads again).
func (p poolExecutionBackstop) markStarted(s *beads.Bead, beadID string) bool {
	writeSessionMetadata(p.store, s, map[string]string{executionClaimStartedKey: startedValue(*s, beadID)}, "execution-claim-started", p.stdout)
	return true
}

// sessionIsQuiet reports whether the runtime has observed no activity for at
// least the grace window. An unreadable or unset activity signal is NOT quiet: a
// backstop that treats "unknown" as "idle" nudges working agents, which is
// exactly how the reverted idle-session nudger produced restart storms.
func (p poolExecutionBackstop) sessionIsQuiet(sessName string) bool {
	last, err := p.sp.GetLastActivity(sessName)
	if err != nil || last.IsZero() {
		return false
	}
	return p.now.Sub(last) >= idleClaimNudgeGrace
}

func (p poolExecutionBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	same = strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeTokenKey]) == strings.TrimSpace(s.Metadata["instance_token"])
	return same, atoiOr0(s.Metadata[executionClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[executionClaimNudgeAtKey])
}

// content is the agent's configured claim nudge. A seat whose stamp shows the
// marker's claim started this incarnation gets the held-claim text instead
// when none is configured or the configured one carries --drain-ack
// (heldClaimNudgeFor): its escalation is vetoed, so with nothing to send it
// would wait at its prompt forever. A seat seen started on a pass whose stamp
// write failed gets no default; escalatesNext holds it until the stamp lands.
func (p poolExecutionBackstop) content(s beads.Bead) string {
	if beadID := s.Metadata[executionClaimNudgeWorkKey]; p.markedStarted(s, beadID) {
		return heldClaimNudgeFor(p.cfg, s, beadID)
	}
	return claimNudgeFor(p.cfg, s)
}

// heldClaimNudgeFor is the claim nudge for a seat whose claim beadID has
// started: its configured nudge, else defaultHeldClaimNudge naming the claim,
// and "" when its template names no configured agent. A configured nudge that
// carries --drain-ack is replaced by the default too.
func heldClaimNudgeFor(cfg *config.City, session beads.Bead, beadID string) string {
	nudge, known := configuredClaimNudgeFor(cfg, session)
	if known && (nudge == "" || strings.Contains(nudge, "--drain-ack")) {
		return fmt.Sprintf(defaultHeldClaimNudge, strings.TrimSpace(beadID))
	}
	return nudge
}

// revalidate re-reads the claim through the owning store's authoritative live
// handle immediately before delivery. Assigned-work snapshots are normally
// CachingStore-backed, so a plain read can still show a claim the agent finished
// seconds ago. A failed read HOLDS: it is not proof the claim went away.
func (p poolExecutionBackstop) revalidate(target backstopTarget) backstopResolution {
	if target.Store == nil {
		return backstopResolutionHold
	}
	live := beads.HandlesFor(target.Store).Live
	if live == nil {
		return backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return backstopResolutionHold
	}
	if !strings.EqualFold(strings.TrimSpace(current.Status), "in_progress") ||
		strings.TrimSpace(current.Assignee) != target.Assignee {
		return backstopResolutionClear
	}
	return backstopResolutionOutstanding
}

func (p poolExecutionBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	// A new claim, or a new incarnation, starts without the old latch. A
	// failed clear leaves the window unopened, so the next pass observes
	// again rather than opening the new window under the old latch.
	if s.Metadata[executionClaimNudgeStalledKey] != "" || s.Metadata[executionClaimNudgeStalledTokenKey] != "" {
		if !writeSessionMetadata(store, s, map[string]string{
			executionClaimNudgeStalledKey:      "",
			executionClaimNudgeStalledTokenKey: "",
		}, executionClaimNudgeLabel, stdout) {
			return
		}
	}
	writeExecutionClaimMarker(store, s, target, 0, now, stdout)
}

func (p poolExecutionBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeExecutionClaimMarker(store, s, target, attempts, now, stdout)
}

// observeHold records which gate held s. It writes only when the reason
// changes, so a standing hold costs one write rather than one per tick. A
// session whose runtime is not running is a hold only while it holds a claim:
// an asleep slot with nothing claimed is not something the backstop would act
// on, and naming it would cost a write per sleep for every pool slot.
func (p poolExecutionBackstop) observeHold(store beads.Store, s *beads.Bead, reason string, stdout io.Writer) {
	if store == nil || s == nil || reason == "" {
		return
	}
	if reason == backstopHoldRuntimeNotRunning && len(p.claims.forIdentities(currentSessionAssigneeIdentities(*s))) == 0 {
		p.clearHold(store, s, stdout)
		return
	}
	if holdIs(*s, reason) {
		return
	}
	writeSessionMetadata(store, s, map[string]string{
		executionClaimHoldKey: reason + " " + p.now.UTC().Format(time.RFC3339),
	}, "execution-claim-hold", stdout)
}

// clearHold removes the breadcrumb once the backstop can act again or the
// claim is gone, so a reason never outlives the condition it named. No-op when
// there is nothing to clear.
func (p poolExecutionBackstop) clearHold(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if store == nil || s == nil || strings.TrimSpace(s.Metadata[executionClaimHoldKey]) == "" {
		return
	}
	writeSessionMetadata(store, s, map[string]string{executionClaimHoldKey: ""}, "execution-claim-hold", stdout)
}

// stalledLatchHolds reports whether an existing escalation latch still covers
// s: it was written for the session's current incarnation and is younger than
// executionStalledLatchRetryAfter. An unparseable latch time holds, so a
// corrupt value can never turn the latch into a per-tick drain.
func (p poolExecutionBackstop) stalledLatchHolds(s *beads.Bead) bool {
	latchedAt := strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey])
	if latchedAt == "" {
		return false
	}
	if strings.TrimSpace(s.Metadata[executionClaimNudgeStalledTokenKey]) != strings.TrimSpace(s.Metadata["instance_token"]) {
		return false
	}
	ts, err := time.Parse(time.RFC3339, latchedAt)
	if err != nil {
		return true
	}
	return p.now.Sub(ts) < executionStalledLatchRetryAfter
}

// exhausted turns a spent attempt budget into one observable fact and one drain
// request, latched so both happen exactly once per claim and incarnation
// however many ticks the session survives. A latch from an earlier incarnation,
// or one old enough that its drain must have been lost, escalates again; the
// re-written latch restarts both bounds.
func (p poolExecutionBackstop) exhausted(store beads.Store, s *beads.Bead, stdout io.Writer) {
	if p.stalledLatchHolds(s) {
		return
	}
	beadID := strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey])
	if beadID == "" {
		return
	}
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	// Latch FIRST. A failed event write or a failed drain must not leave the
	// escalation armed to repeat on every subsequent tick; the marker itself is
	// the durable record that this claim was escalated, and the operator sees the
	// failure on stdout.
	if !writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeStalledKey:      p.now.UTC().Format(time.RFC3339),
		executionClaimNudgeStalledTokenKey: strings.TrimSpace(s.Metadata["instance_token"]),
	}, "execution-claim-nudge", stdout) {
		return
	}
	// Report the attempts actually DELIVERED, not the cap. An agent with no
	// configured nudge escalates at 0/3 having never been contacted, and an
	// operator reading "after 3 attempts" there would go looking for three
	// nudges that were never sent.
	attempts := atoiOr0(s.Metadata[executionClaimNudgeCountKey])
	p.emitStepStalled(s, beadID, attempts)
	fmt.Fprintf(stdout, //nolint:errcheck // best-effort
		"execution-claim-nudge: %s still holds %s unexecuted after %d/%d nudge attempts; draining (%s)\n",
		sessName, beadID, attempts, idleClaimNudgeMaxAttempts, executionStalledDrainReason)
	if p.requestDrain == nil || sessName == "" {
		return
	}
	// The drain is the CONVERGENCE step, not a notification. Nothing else will
	// release this claim: the session is alive and awake, so no crash lane
	// touches it, and it holds in_progress work, so the wake machinery keeps it
	// alive by design. The tracked drain is what turns "we gave up nudging" into
	// stop -> close -> dead-assignee reopen -> the row is claimable again.
	if err := p.requestDrain(*s); err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: draining %s failed: %v\n", sessName, err) //nolint:errcheck // best-effort
	}
}

func (p poolExecutionBackstop) emitStepStalled(s *beads.Bead, beadID string, attempts int) {
	if p.rec == nil {
		return
	}
	rootID := strings.TrimSpace(s.Metadata[executionClaimNudgeRootKey])
	payload, err := json.Marshal(events.ExecutionStepStalledPayload{
		BeadID:     beadID,
		RootBeadID: rootID,
		SessionID:  s.ID,
		Attempts:   attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.ExecutionStepStalled,
		Actor:     eventActor(),
		Subject:   beadID,
		RunID:     rootID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

func (p poolExecutionBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearExecutionClaimMarker(store, s, stdout)
}

func writeExecutionClaimMarker(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeWorkKey:     target.ID,
		executionClaimNudgeRootKey:     target.RootID,
		executionClaimNudgeStoreRefKey: target.StoreRef,
		executionClaimNudgeCountKey:    strconv.Itoa(attempts),
		executionClaimNudgeAtKey:       now.UTC().Format(time.RFC3339),
		executionClaimNudgeTokenKey:    strings.TrimSpace(s.Metadata["instance_token"]),
	}, "execution-claim-nudge", stdout)
}

// clearExecutionClaimMarker wipes the state machine — including the escalation
// latch and the hold breadcrumb — so the next claim this slot takes starts a
// fresh window. No-op when there is nothing to clear, so steady-state ticks
// stay write-free.
func clearExecutionClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	keys := []string{
		executionClaimNudgeWorkKey,
		executionClaimNudgeRootKey,
		executionClaimNudgeStoreRefKey,
		executionClaimNudgeCountKey,
		executionClaimNudgeAtKey,
		executionClaimNudgeTokenKey,
		executionClaimNudgeStalledKey,
		executionClaimNudgeStalledTokenKey,
		executionClaimHoldKey,
	}
	dirty := false
	for _, key := range keys {
		if s.Metadata[key] != "" {
			dirty = true
			break
		}
	}
	if !dirty {
		return
	}
	kvs := make(map[string]string, len(keys))
	for _, key := range keys {
		kvs[key] = ""
	}
	if !writeSessionMetadata(store, s, kvs, "execution-claim-nudge", stdout) {
		return
	}
	for _, key := range keys {
		delete(s.Metadata, key)
	}
}

// writeSessionMetadata persists a marker patch and mirrors it into the in-memory
// session bead so the rest of this tick reads the just-written values.
func writeSessionMetadata(store beads.Store, s *beads.Bead, kvs map[string]string, label string, stdout io.Writer) bool {
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "%s: marking %s failed: %v\n", label, s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		s.Metadata[k] = v
	}
	return true
}

// requestExecutionStalledDrain begins a TRACKED drain of a seat that claimed
// work and never executed it.
//
// Tracked, not a bare runtime flag: the drainTracker is the machinery that
// actually converges a session — it defers the interrupt one tick so a
// false positive can still be canceled, then advances through stop, close, and
// the dead-assignee reopen that puts the claim back in the demand set. Setting
// the runtime's GC_DRAIN meta alone announces an intention that nothing drives.
//
// The reason is executionStalledDrainReason precisely because this session looks
// exactly like one every keep-alive guard exists to protect (awake, running,
// holding an in_progress claim); a cancelable reason would be canceled by the
// very claim that justified the drain.
//
// It re-reads the session through the front door rather than trusting the
// backstop's snapshot: the drain carries the session's generation, and acting on
// a stale generation is how a drain lands on the wrong incarnation.
func (cr *CityRuntime) requestExecutionStalledDrain(sessionBead beads.Bead) error {
	if cr == nil || cr.sessionDrains == nil {
		return fmt.Errorf("no drain tracker configured for %q", sessionBead.ID)
	}
	info, err := sessionFrontDoor(cr.sessionsBeadStore()).Get(sessionBead.ID)
	if err != nil {
		return fmt.Errorf("reading session %q before draining: %w", sessionBead.ID, err)
	}
	beginSessionDrainInfo(info, cr.sp, cr.sessionDrains, executionStalledDrainReason, clock.Real{}, defaultDrainTimeout)
	return nil
}
