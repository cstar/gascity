package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// deferSingletonAliasRead applies the existing retry backoff to live alias
// reads, not just conflict-counter writes. The current census may only defer
// a known conflict; it never authorizes acquisition. Once the holder disappears
// from the next census, or the retry is due, the caller checks live under lock.
func deferSingletonAliasRead(cfg *config.City, template, alias, selfOwner string, b beads.Bead, census []beads.Bead, now time.Time) bool {
	if alias == "" || strings.TrimSpace(b.Metadata["alias"]) != "" ||
		strings.TrimSpace(b.Metadata[poolAliasConflictMetadataKey]) != alias {
		return false
	}
	a := findAgentByTemplate(cfg, template)
	if a == nil || !a.UsesCanonicalSingletonPoolIdentity() || alias != a.QualifiedName() {
		return false
	}
	count, _ := strconv.Atoi(strings.TrimSpace(b.Metadata[poolAliasConflictCountMetadataKey]))
	if deferredSingletonAliasRetryDue(b.Metadata[poolAliasConflictAtMetadataKey], count, now) {
		return false
	}
	// Reuse the exact ownership rules, including failed-create and configured
	// owner exceptions. This store is read-only; live mutation stays on the
	// original store behind the city alias lock.
	snapshot := beads.NewMemStoreFrom(len(census), census, nil)
	err := session.EnsureAliasAvailableWithConfigForOwner(snapshot, cfg, alias, b.ID, selfOwner)
	return errors.Is(err, session.ErrSessionAliasExists)
}
