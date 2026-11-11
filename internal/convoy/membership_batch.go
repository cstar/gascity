package convoy

import (
	"fmt"
	"reflect"

	"github.com/gastownhall/gascity/internal/beads"
)

// memberBatchExactChunk bounds how many tracked ids one exact batch read
// carries.
const memberBatchExactChunk = 200

// MemberBatchReaders are the batched reads MembersBatch needs from the one
// store that owns the convoys. Every field is required; a nil field means the
// store cannot be batched and MembersBatch answers nothing.
type MemberBatchReaders struct {
	// Deps reads the convoys' DOWN edges, where the tracks edges live.
	Deps beads.DependencyBatchLister
	// Exact reads the tracked beads by id.
	Exact beads.ExactBatchGetter
	// Children says which convoys might have legacy parent-child members.
	Children beads.ChildPresenceProber
}

// Complete reports whether every reader MembersBatch needs is set.
func (r MemberBatchReaders) Complete() bool {
	return !isNilReader(r.Deps) && !isNilReader(r.Exact) && !isNilReader(r.Children)
}

// MembersBatch answers Members(store, id, includeClosed) for many convoys of
// ONE store in a fixed number of batched reads, for every convoy whose answer
// it can prove equal to the per-convoy one. It is a cost optimization over
// Members, never a second definition of membership.
//
// The returned map holds ONLY the convoys it fully answered. A convoy without
// a key is one the caller must still answer with Members — an ephemeral
// convoy, one that may have legacy parent-child members, or one with a tracks
// target the exact read did not return. A non-nil error means nothing was
// answered (the map is nil) and the caller answers every convoy with Members.
// Fewer than two answerable convoys, or a missing reader, is an empty map and
// no reads at all: the batch would cost what the per-convoy path costs.
//
// # Why an answered convoy is identical to Members
//
// Members reads three things per convoy: the legacy children List, the DOWN
// edges, and one Get per tracks target. The batch replaces each of them only
// where it can show the replacement returns the same thing.
//
//   - Legacy children. A convoy Children does not flag has an empty child
//     List, by ChildPresenceProber's never-under-report contract. A flagged
//     convoy is left to Members, so its List still runs exactly.
//   - Tracks edges. Deps returns the same edges DepList(id, "down") does for
//     every edge whose target has a row. bd's batched `dep list` also keeps
//     edges to targets WITHOUT a row, which the one-id form drops; but such a
//     target is never in the exact read's answer, so its convoy is left to
//     Members and the difference never reaches an answer.
//   - Tracked beads. Exact answers a target only when it returned exactly that
//     id, from the same read Get makes (`bd show --json`, one bead per row,
//     the same projection). Anything else — absent, a wisp only Get's fallback
//     reaches, a substring collision, an unprojectable row — is unresolved,
//     and its convoy is left to Members, which decides between a placeholder
//     and an error exactly as before.
//
// Members are then accumulated by the same memberSet MembersIn uses. Its sort
// is a total order on (CreatedAt, ID) over unique ids, so the order in which
// the batch happens to see the edges does not reach the result.
//
// Like Members with no member classes, this spans only the convoy's own store:
// every tracked bead is read from it.
func MembersBatch(r MemberBatchReaders, convoys []beads.Bead, includeClosed bool) (map[string][]beads.Bead, error) {
	answered := make(map[string][]beads.Bead)
	if !r.Complete() {
		return answered, nil
	}
	ids := make([]string, 0, len(convoys))
	seen := make(map[string]bool, len(convoys))
	for _, c := range convoys {
		if c.ID == "" || c.Ephemeral || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		ids = append(ids, c.ID)
	}
	if len(ids) < 2 {
		return answered, nil
	}

	flagged, err := r.Children.ParentsWithChildren(ids)
	if err != nil {
		return nil, fmt.Errorf("probing legacy convoy children: %w", err)
	}
	edges, err := r.Deps.DepListBatch(ids)
	if err != nil {
		return nil, fmt.Errorf("listing convoy dependencies: %w", err)
	}

	tracked := make(map[string][]string, len(ids))
	var targets []string
	seenTarget := make(map[string]bool)
	for _, id := range ids {
		if flagged[id] {
			continue
		}
		for _, dep := range edges[id] {
			if dep.Type != TrackingDepType || dep.IssueID != id {
				continue
			}
			tracked[id] = append(tracked[id], dep.DependsOnID)
			if !seenTarget[dep.DependsOnID] {
				seenTarget[dep.DependsOnID] = true
				targets = append(targets, dep.DependsOnID)
			}
		}
	}

	found := make(map[string]beads.Bead, len(targets))
	for start := 0; start < len(targets); start += memberBatchExactChunk {
		chunk := targets[start:min(start+memberBatchExactChunk, len(targets))]
		got, _, err := r.Exact.GetExactBatch(chunk)
		if err != nil {
			return nil, fmt.Errorf("getting %d tracked items: %w", len(chunk), err)
		}
		for _, id := range chunk {
			if b, ok := got[id]; ok {
				found[id] = b
			}
		}
	}

convoys:
	for _, id := range ids {
		if flagged[id] {
			continue
		}
		members := newMemberSet(includeClosed, len(tracked[id]))
		for _, target := range tracked[id] {
			item, ok := found[target]
			if !ok {
				continue convoys
			}
			members.add(item)
		}
		answered[id] = members.sorted()
	}
	return answered, nil
}

// isNilReader reports whether a reader is unset, looking through a typed nil
// pointer the way isNilStore does.
func isNilReader(r any) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}
