package beads

import (
	"encoding/json"
	"fmt"
)

// childPresenceProbeBatch bounds how many ids one bd invocation of the
// child-presence probe carries, for both the up-direction dep list and the
// show of the children it finds.
const childPresenceProbeBatch = 200

var _ ChildPresenceProber = (*BdStore)(nil)

// ParentsWithChildren implements ChildPresenceProber with one
// `bd dep list <ids...> --direction=up --type=parent-child --json` per batch of
// parents, plus one `bd show --json <children...>` per batch of children when
// there are any.
//
// The probe answers the child issue objects WITHOUT naming which anchor each
// one hangs off, so the children are read back with bd show and attributed by
// their own parent-child edges (bd's "parent" field and every parent-child
// dependency the row carries, because a child can hold more than one). That
// is what makes the answer a superset of List(ParentID): the List keeps a bead
// only when its projected ParentID equals the requested id, and the projected
// ParentID is one of the attributed parents. A child that cannot be read back
// or that names no requested parent flags every requested id rather than
// guessing, so the set can over-report but never under-report.
//
// Any read failure is returned with a nil map. That includes bd reporting a
// requested parent as missing, which DepList swallows as "no edges": here an
// unanswered parent must not read as childless. A backend that cannot run the
// probe at all wraps ErrChildPresenceUnsupported.
func (s *BdStore) ParentsWithChildren(parentIDs []string) (map[string]bool, error) {
	ids := uniqueNonEmptyIDs(parentIDs)
	flagged := make(map[string]bool)
	if len(ids) == 0 {
		return flagged, nil
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	flagAll := func() map[string]bool {
		all := make(map[string]bool, len(ids))
		for _, id := range ids {
			all[id] = true
		}
		return all
	}

	var childIDs []string
	seenChild := make(map[string]bool)
	for _, chunk := range chunkIDsAvoidingSingletons(ids, childPresenceProbeBatch) {
		args := append([]string{"dep", "list"}, chunk...)
		args = append(args, "--direction=up", "--type=parent-child", "--json")
		out, err := s.runBDTransientRead(args...)
		if err != nil {
			if isBdOperationUnsupported(err) {
				return nil, fmt.Errorf("probing children of %d parents: %w: %w", len(chunk), ErrChildPresenceUnsupported, err)
			}
			return nil, fmt.Errorf("probing children of %d parents: %w", len(chunk), err)
		}
		extracted := extractJSON(out)
		if len(extracted) == 0 || string(extracted) == "[]" {
			continue
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(extracted, &rows); err != nil {
			return nil, fmt.Errorf("probing children: parsing JSON: %w", err)
		}
		for _, row := range rows {
			if row.ID == "" {
				// A row that names no child cannot be attributed to a parent.
				return flagAll(), nil
			}
			if seenChild[row.ID] {
				continue
			}
			seenChild[row.ID] = true
			childIDs = append(childIDs, row.ID)
		}
	}
	if len(childIDs) == 0 {
		return flagged, nil
	}

	for start := 0; start < len(childIDs); start += childPresenceProbeBatch {
		chunk := childIDs[start:min(start+childPresenceProbeBatch, len(childIDs))]
		found, unresolved, err := s.GetExactBatch(chunk)
		if err != nil {
			return nil, fmt.Errorf("reading %d probed children: %w", len(chunk), err)
		}
		if len(unresolved) > 0 {
			return flagAll(), nil
		}
		for _, id := range chunk {
			attributed := false
			for _, parent := range parentChildTargets(found[id]) {
				if requested[parent] {
					flagged[parent] = true
					attributed = true
				}
			}
			if !attributed {
				return flagAll(), nil
			}
		}
	}
	return flagged, nil
}

// parentChildTargets returns every parent b names: its projected ParentID and
// the target of each parent-child edge it carries.
func parentChildTargets(b Bead) []string {
	var out []string
	if b.ParentID != "" {
		out = append(out, b.ParentID)
	}
	for _, dep := range b.Dependencies {
		if dep.Type == "parent-child" && dep.IssueID == b.ID && dep.DependsOnID != "" {
			out = append(out, dep.DependsOnID)
		}
	}
	return out
}

// uniqueNonEmptyIDs returns ids without empties and repeats, in first-seen
// order.
func uniqueNonEmptyIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// chunkIDsAvoidingSingletons splits ids into chunks of at most size, except
// that a trailing one-id chunk is folded into the chunk before it. bd answers
// `bd dep list` with a different JSON shape for one id than for several, so a
// multi-id request must never degrade to a one-id invocation.
func chunkIDsAvoidingSingletons(ids []string, size int) [][]string {
	if len(ids) <= size {
		return [][]string{ids}
	}
	var chunks [][]string
	for start := 0; start < len(ids); start += size {
		end := min(start+size, len(ids))
		if len(ids)-end == 1 {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
		if end == len(ids) {
			break
		}
	}
	return chunks
}
