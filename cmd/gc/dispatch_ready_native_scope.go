package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

func controlReadyNativeScopeReader(ready func(context.Context, bool, int) ([]beads.Bead, error), closeReader func() error) *controlReadyReader {
	return &controlReadyReader{Ready: func(ctx context.Context, include bool) ([]beads.Bead, error) {
		// The former non-ephemeral cache has no aggregate frontier limit. Apply
		// assignee/route limits after reading it, so the 5001st candidate survives.
		limit := 0
		if include {
			limit = controlReadyFallbackLimit
		}
		return ready(ctx, include, limit)
	}, Close: closeReader}
}
