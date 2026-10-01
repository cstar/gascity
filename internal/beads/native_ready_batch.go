package beads

import (
	"context"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beadmeta"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// Use the guarded edge surface so telemetry and backing-store policy remain
// attached. Both permanent and ephemeral anchors are partitioned by Beads in
// batches; this never opens a new store or nests the adapter's read lock.
func filterNativeReadyByWorkOutcomeBatch(ctx context.Context, storage beadslib.Storage, reader issueops.EdgeReader, candidates []Bead) ([]Bead, error) {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	edges, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: ids})
	if err != nil {
		return nil, fmt.Errorf("reading blocking dependency outcomes: %w", err)
	}
	targets := make(map[string]struct{})
	byAnchor := make(map[string][]string, len(candidates))
	for _, anchor := range edges.Anchors {
		if anchor.Missing {
			continue
		}
		for _, edge := range anchor.Edges {
			if edge == nil || !IsReadyBlockingDependencyType(string(edge.Type)) {
				continue
			}
			targets[edge.DependsOnID] = struct{}{}
			byAnchor[anchor.ID] = append(byAnchor[anchor.ID], edge.DependsOnID)
		}
	}
	if len(targets) == 0 {
		return candidates, nil
	}
	targetIDs := make([]string, 0, len(targets))
	for id := range targets {
		targetIDs = append(targetIDs, id)
	}
	sort.Strings(targetIDs)
	blockers, err := storage.GetIssuesByIDs(ctx, targetIDs)
	if err != nil {
		return nil, fmt.Errorf("hydrating blocking dependency outcomes: %w", err)
	}
	blocked := make(map[string]bool, len(blockers))
	for _, blocker := range blockers {
		if blocker == nil {
			continue
		}
		metadata, err := metadataMapFromNative(blocker.Metadata)
		if err != nil {
			return nil, fmt.Errorf("parsing blocker %s metadata: %w", blocker.ID, err)
		}
		// Preserve the narrow veto: upstream Ready already resolved pinned gates
		// and waits-for children. An open target alone must not veto readiness.
		blocked[blocker.ID] = string(blocker.Status) == "closed" && metadata[beadmeta.WorkOutcomeMetadataKey] == beadmeta.WorkOutcomeBlocked
	}
	result := make([]Bead, 0, len(candidates))
	for _, candidate := range candidates {
		veto := false
		for _, id := range byAnchor[candidate.ID] {
			if blocked[id] {
				veto = true
				break
			}
		}
		if !veto {
			result = append(result, candidate)
		}
	}
	return result, nil
}
