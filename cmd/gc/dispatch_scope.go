package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Scope filtering consumes the canonical snapshot already on the event. It must
// never open a store to decide whether opening a store is necessary. Unknown
// scopes and unobserved cross-rig dependencies retain the five-second fallback.
func workflowEventForRig(evt events.Event, rig string, known map[string]bool) bool {
	if !workflowEventRelevant(evt) {
		return false
	}
	if rig == "" {
		return true
	}
	if (evt.RunID != "" && known[evt.RunID]) || (evt.Subject != "" && known[evt.Subject]) {
		return true
	}
	scope := ""
	b, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if ok {
		scope = b.Metadata[beadmeta.RootStoreRefMetadataKey]
	}
	if scope == "rig:"+rig {
		return true
	}
	if ok {
		if b.Metadata[beadmeta.ScopeKindMetadataKey] == "rig" && b.Metadata[beadmeta.ScopeRefMetadataKey] == rig {
			return true
		}
		if strings.HasPrefix(b.Metadata[beadmeta.RoutedToMetadataKey], rig+"/") {
			return true
		}
	}
	return false
}

func workflowScopedEventFilter(qualified string) func(events.Event) bool {
	rig, _, scoped := strings.Cut(qualified, "/")
	if !scoped {
		rig = ""
	}
	known := make(map[string]bool)
	return func(evt events.Event) bool {
		if !workflowEventRelevant(evt) {
			return true
		} // Preserve diagnostics and stream errors.
		if !workflowEventForRig(evt, rig, known) {
			workflowTracef("serve ignore-foreign-or-unknown subject=%s rig=%s", evt.Subject, rig)
			return false
		}
		// This index is populated from event snapshots, not extra SQL. Bounding it
		// can only postpone a wake to the safety sweep, never lose work.
		if len(known) > 4096 {
			clear(known)
		}
		if evt.RunID != "" {
			known[evt.RunID] = true
		}
		if b, ok := beads.DecodeBeadEventPayload(evt.Payload); ok {
			if root := b.Metadata[beadmeta.RootBeadIDMetadataKey]; root != "" {
				known[root] = true
			}
			for _, dep := range b.Dependencies {
				if dep.DependsOnID != "" {
					known[dep.DependsOnID] = true
				}
			}
		}
		workflowTracef("serve scoped-wake subject=%s rig=%s", evt.Subject, rig)
		return true
	}
}
