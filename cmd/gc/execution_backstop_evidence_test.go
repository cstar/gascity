package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Execution-evidence rows for the v1.5.1 backport of the execution backstop's
// drain veto (#7911, #7922, #7984, and the latch clear of #8592).
//
// The incident they pin: a pool seat claimed its graph step, `gc hook --claim`
// recorded execution.step_started from that seat after its wake, and the seat
// then ran a long full test suite whose pane printed nothing. The activity
// clock read the seat as quiet, the three nudges went out, and the seat was
// drained while it was working. Execution evidence must veto that drain and
// leave the nudges alone; a seat without evidence must still be drained.

// stampLifecycleIdentity gives the fixture's claim the hook's lifecycle
// stamps (graph step, holding session) and the session row a wake time and
// incarnation, so it is a step_started candidate.
func (f *executionBackstopFixture) stampLifecycleIdentity(t *testing.T, token string, wokeAt time.Time) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.StepIDMetadataKey:      "evo-feature.implement",
		beadmeta.SessionIDMetadataKey:   f.session.ID,
		beadmeta.SessionNameMetadataKey: f.sessName,
	}); err != nil {
		t.Fatalf("stamping the claim's lifecycle identity: %v", err)
	}
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		"instance_token": token,
		"last_woke_at":   wokeAt.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("stamping the session incarnation: %v", err)
	}
}

// recordStepStarted records the hook's execution.step_started of the claim
// from sessionID at ts.
func (f *executionBackstopFixture) recordStepStarted(sessionID string, ts time.Time) {
	f.rec.Record(events.Event{
		Type:      events.ExecutionStepStarted,
		Ts:        ts,
		Actor:     sessionID,
		Subject:   f.work.ID,
		RunID:     "root-1",
		SessionID: sessionID,
	})
}

// runSilentTurn drives the backstop for d of wall time at a 30s tick while
// the pane prints nothing after quietFrom (a full test suite running in one
// tool call).
func (f *executionBackstopFixture) runSilentTurn(t *testing.T, quietFrom time.Time, d time.Duration) {
	t.Helper()
	f.sp.SetActivity(f.sessName, quietFrom)
	for end := f.now.Add(d); f.now.Before(end); f.now = f.now.Add(30 * time.Second) {
		f.tick(t)
	}
}

func (f *executionBackstopFixture) deliveredNudges() []string {
	var out []string
	for _, c := range f.sp.Calls {
		if c.Method == "Nudge" {
			out = append(out, c.Message)
		}
	}
	return out
}

// TestExecutionEvidenceNeverDrainsAStartedSeatRunningASilentSuite is the
// incident row: started at its own incarnation, then 45 minutes of no pane
// output. The bounded nudges go out (held-claim text, no --drain-ack), and the
// seat is held as executing: no drain, no execution.step_stalled.
func TestExecutionEvidenceNeverDrainsAStartedSeatRunningASilentSuite(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	woke := f.now.Add(-2 * time.Minute)
	f.stampLifecycleIdentity(t, "incarnation-1", woke)
	f.recordStepStarted(f.session.ID, woke.Add(90*time.Second))

	f.runSilentTurn(t, f.now, 45*time.Minute)

	if len(f.drained) != 0 {
		t.Fatalf("drain requests for a started seat = %v, want none; stdout=%s", f.drained, f.stdout.String())
	}
	if got := f.stalledEvents(); got != 0 {
		t.Fatalf("execution.step_stalled events = %d, want 0", got)
	}
	if got := f.sessionMeta(t, executionClaimStartedKey); got != "incarnation-1|"+f.work.ID {
		t.Fatalf("execution evidence stamp = %q, want incarnation-1|%s", got, f.work.ID)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldExecuting+" ") {
		t.Fatalf("hold breadcrumb = %q, want executing", got)
	}
	nudges := f.deliveredNudges()
	if len(nudges) != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges = %d (%q), want the bounded %d", len(nudges), nudges, idleClaimNudgeMaxAttempts)
	}
	for _, n := range nudges {
		if strings.Contains(n, "--drain-ack") || !strings.Contains(n, f.work.ID) {
			t.Fatalf("nudge to a started seat = %q, want the held-claim text naming %s without --drain-ack", n, f.work.ID)
		}
	}
	if got := strings.Count(f.stdout.String(), "holding as executing"); got != 1 {
		t.Fatalf("executing-hold log lines = %d, want 1 (logged once on entry); stdout=%s", got, f.stdout.String())
	}
}

// TestExecutionEvidenceStampsWhileBusyAndOutlivesTheEventTail: the stamp is
// written on the first pass that sees the event, even while the pane is busy,
// so the veto still holds after the event has left the readable tail.
func TestExecutionEvidenceStampsWhileBusyAndOutlivesTheEventTail(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	woke := f.now.Add(-time.Minute)
	f.stampLifecycleIdentity(t, "incarnation-1", woke)
	f.recordStepStarted(f.session.ID, woke.Add(10*time.Second))

	f.sp.SetActivity(f.sessName, f.now) // busy
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimStartedKey); got != "incarnation-1|"+f.work.ID {
		t.Fatalf("stamp after a busy pass = %q, want it persisted", got)
	}
	if got := len(f.deliveredNudges()); got != 0 {
		t.Fatalf("nudges to a busy seat = %d, want 0", got)
	}

	f.rec.Events = nil // the event scrolled out of any bounded tail
	f.runSilentTurn(t, f.now, 40*time.Minute)
	if len(f.drained) != 0 || f.stalledEvents() != 0 {
		t.Fatalf("drains %v / stalled %d after the event left the tail, want none", f.drained, f.stalledEvents())
	}
}

// The negative controls: evidence that is not this seat's, or not this
// incarnation's, or absent, is no evidence. Each seat is drained exactly once,
// as before the backport.
func TestExecutionEvidenceNegativeControlsStillDrain(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *executionBackstopFixture, woke time.Time)
	}{
		{"missing step_started", func(_ *testing.T, _ *executionBackstopFixture, _ time.Time) {}},
		{"another session's step_started", func(_ *testing.T, f *executionBackstopFixture, woke time.Time) {
			f.recordStepStarted("gc-other-session", woke.Add(time.Minute))
		}},
		{"step_started from before this wake (old generation)", func(_ *testing.T, f *executionBackstopFixture, woke time.Time) {
			f.recordStepStarted(f.session.ID, woke.Add(-time.Minute))
		}},
		{"stamp from another incarnation", func(t *testing.T, f *executionBackstopFixture, _ time.Time) {
			if err := f.store.SetMetadata(f.session.ID, executionClaimStartedKey, "incarnation-0|"+f.work.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"stamp for another claim", func(t *testing.T, f *executionBackstopFixture, _ time.Time) {
			if err := f.store.SetMetadata(f.session.ID, executionClaimStartedKey, "incarnation-1|other-bead"); err != nil {
				t.Fatal(err)
			}
		}},
		{"claim stamped for another session", func(t *testing.T, f *executionBackstopFixture, woke time.Time) {
			if err := f.store.SetMetadata(f.work.ID, beadmeta.SessionIDMetadataKey, "gc-other-session"); err != nil {
				t.Fatal(err)
			}
			f.recordStepStarted(f.session.ID, woke.Add(time.Minute))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			woke := f.now.Add(-2 * time.Minute)
			f.stampLifecycleIdentity(t, "incarnation-1", woke)
			tc.setup(t, f, woke)

			// Inside executionStalledLatchRetryAfter, so the one escalation
			// is not re-requested by the lost-drain retry.
			f.runSilentTurn(t, f.now, 25*time.Minute)

			if len(f.drained) != 1 {
				t.Fatalf("drain requests = %v, want exactly 1 (never-started detection preserved); stdout=%s", f.drained, f.stdout.String())
			}
			if got := f.stalledEvents(); got != 1 {
				t.Fatalf("execution.step_stalled events = %d, want 1", got)
			}
			for _, n := range f.deliveredNudges() {
				if !strings.Contains(n, "--drain-ack") {
					t.Fatalf("nudge to an unstarted seat = %q, want its configured claim nudge unchanged", n)
				}
			}
		})
	}
}

// TestExecutionEvidenceUnknownActivityStillHolds: the never-nudge-on-unknown
// rule is unchanged. A runtime with no activity reading holds as not_quiet:
// no nudge and no drain, started or not.
func TestExecutionEvidenceUnknownActivityStillHolds(t *testing.T) {
	for _, started := range []bool{false, true} {
		f := newExecutionBackstopFixture(t)
		woke := f.now.Add(-2 * time.Minute)
		f.stampLifecycleIdentity(t, "incarnation-1", woke)
		if started {
			f.recordStepStarted(f.session.ID, woke.Add(time.Minute))
		}
		for end := f.now.Add(30 * time.Minute); f.now.Before(end); f.now = f.now.Add(30 * time.Second) {
			f.tick(t)
		}
		if len(f.drained) != 0 || len(f.deliveredNudges()) != 0 {
			t.Fatalf("started=%v: drains %v nudges %d with unknown activity, want none", started, f.drained, len(f.deliveredNudges()))
		}
		if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldNotQuiet+" ") {
			t.Fatalf("started=%v: hold = %q, want not_quiet", started, got)
		}
	}
}

// TestExecutionEvidenceANewClaimStartsWithoutTheOldLatch (#8592): a seat that
// escalated claim A and, in the same incarnation, holds claim B gets B's own
// window, not A's latch.
func TestExecutionEvidenceANewClaimStartsWithoutTheOldLatch(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setInstanceToken(t, "incarnation-1")
	driveToEscalation(t, f)

	closed := "closed"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.tick(t) // claim A gone: the marker clears
	b, err := f.store.Create(beads.Bead{
		Title: "claim B", Type: "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := f.store.Update(b.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatal(err)
	}
	f.work = b
	driveToEscalationAgain(t, f)
	if len(f.drained) != 2 {
		t.Fatalf("drains after claim B idled out its own window = %v, want 2", f.drained)
	}
}

// driveToEscalationAgain is driveToEscalation for a fixture that has already
// escalated once.
func driveToEscalationAgain(t *testing.T, f *executionBackstopFixture) {
	t.Helper()
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if f.sessionMeta(t, executionClaimNudgeStalledKey) != "" {
		t.Fatal("observing a new claim left the old latch armed")
	}
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	f.now = f.now.Add(idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
}
