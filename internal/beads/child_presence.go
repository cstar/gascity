package beads

import "errors"

// ChildPresenceProber is the optional capability of a store that can say, for
// many parents in one round trip, which of them might have children.
//
// It exists so a caller that has to answer "list the children of each of these
// N beads" can skip the per-parent List for every parent that provably has
// none. On the bd-CLI provider that List is two subprocesses per parent (the
// issues tier and the wisps tier), and a fan-out over a few hundred convoys
// whose legacy parent-child membership is empty pays for all of them to learn
// nothing.
//
// The contract is one-sided on purpose:
//
//   - The returned set contains every requested id for which
//     List(ListQuery{ParentID: id, IncludeClosed: true, TierMode: TierBoth})
//     could be non-empty. It MAY over-report — a flagged parent simply takes the
//     exact per-parent List — but it must NEVER under-report, because an
//     unflagged parent is answered as childless without any List at all.
//   - A failed read returns a nil map and an error, never a partial set. A
//     partial set would read the parents the failure cost as childless.
//
// Callers treat ErrChildPresenceUnsupported (and any other error) as the cue
// to run the per-parent List for every parent, exactly as they would for a
// store that does not implement the capability.
type ChildPresenceProber interface {
	ParentsWithChildren(parentIDs []string) (map[string]bool, error)
}

// ErrChildPresenceUnsupported reports that the backing store cannot answer a
// batched child-presence probe. Match it with errors.Is.
var ErrChildPresenceUnsupported = errors.New("batched child-presence probe unsupported by backing store")
