package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Suspended rigs still need eventual stale-tracking recovery, but not the
// active fleet's 30-second polling. Scan them at boot and every 15 minutes.
const orderTrackingColdSweepInterval = 15 * time.Minute

func orderTrackingWatchdogTargets(cityPath string, cfg *config.City, state suspensionstate.State, includeCold bool) []orderTrackingSweepTarget {
	targets := orderTrackingSweepTargetsForConfig(cityPath, cfg)
	if includeCold || cfg == nil {
		return targets
	}
	cold := make(map[string]bool)
	for _, rig := range cfg.Rigs {
		cold[rig.Name] = suspensionstate.EffectiveRigSuspended(state, rig.Name, rig.EffectiveSuspendedOnStart())
	}
	kept := targets[:0]
	for _, target := range targets {
		if target.target.ScopeKind != "rig" || !cold[target.target.RigName] {
			kept = append(kept, target)
		}
	}
	return kept
}
