package main

import "github.com/gastownhall/gascity/internal/beads"

// controlReadySnapshot is a detached readiness view. Both the general cache
// and the CLI's authoritative ready frontier can serve it after the owned
// source has closed; the registry bounds either view by the same short TTL.
// A deferred bead becoming ready during that TTL appears on the next refresh.
type controlReadySnapshot interface {
	CachedReady() ([]beads.Bead, bool)
}

type controlReadyRows []beads.Bead

// CachedReady returns the detached authoritative frontier within its registry TTL.
func (r controlReadyRows) CachedReady() ([]beads.Bead, bool) {
	return []beads.Bead(r), true
}
