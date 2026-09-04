package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
)

// doltParkedDatabasesCheck verifies that every suspended rig's Dolt database
// is parked out of the managed server's data directory and every active rig's
// database is served. `gc doctor --fix` moves them (same-volume rename, no
// copy, no delete). Rigs that suspended before parking existed are the usual
// reason the served side drifts.
type doltParkedDatabasesCheck struct {
	cityPath string
	cfg      *config.City
}

func newDoltParkedDatabasesCheck(cityPath string, cfg *config.City) *doltParkedDatabasesCheck {
	return &doltParkedDatabasesCheck{cityPath: cityPath, cfg: cfg}
}

func (c *doltParkedDatabasesCheck) Name() string { return "dolt-parked-databases" }

// doltParkDrift is one rig whose database is on the wrong side.
type doltParkDrift struct {
	rig       string
	db        string
	suspended bool
	status    doltpark.Status
}

func (c *doltParkedDatabasesCheck) drifts() ([]doltParkDrift, error) {
	if c.cfg == nil || !doltpark.Enabled() {
		return nil, nil
	}
	st, err := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	if err != nil {
		return nil, fmt.Errorf("reading suspension state: %w", err)
	}
	suspended := buildEffectiveSuspendedRigNames(c.cfg, st)
	var out []doltParkDrift
	for i := range c.cfg.Rigs {
		rig := &c.cfg.Rigs[i]
		layout, db := configedit.RigDatabaseLayout(c.cityPath, rig)
		if db == "" {
			continue
		}
		status, err := doltpark.Inspect(layout, db)
		if err != nil {
			continue // reserved or malformed name: nothing to park, nothing to report
		}
		isSuspended := suspended[rig.Name]
		wrongSide := (isSuspended && status == doltpark.Served) || (!isSuspended && status == doltpark.Parked)
		if wrongSide || status == doltpark.Conflict {
			out = append(out, doltParkDrift{rig: rig.Name, db: db, suspended: isSuspended, status: status})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rig < out[j].rig })
	return out, nil
}

func (c *doltParkedDatabasesCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if !doltpark.Enabled() {
		r.Status = doctor.StatusOK
		r.Message = "parking disabled (" + doltpark.EnvParkOnSuspend + ")"
		return r
	}
	drifts, err := c.drifts()
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = err.Error()
		return r
	}
	if len(drifts) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "suspended rigs' dolt databases are parked; active rigs' are served"
		return r
	}
	var toPark, toRestore, conflicts []string
	for _, d := range drifts {
		switch {
		case d.status == doltpark.Conflict:
			conflicts = append(conflicts, d.rig+"/"+d.db)
		case d.suspended:
			toPark = append(toPark, d.rig+"/"+d.db)
		default:
			toRestore = append(toRestore, d.rig+"/"+d.db)
		}
	}
	var parts []string
	if len(toPark) > 0 {
		parts = append(parts, fmt.Sprintf("%d suspended rig database(s) still served: %s", len(toPark), strings.Join(toPark, ", ")))
	}
	if len(toRestore) > 0 {
		parts = append(parts, fmt.Sprintf("%d active rig database(s) parked: %s", len(toRestore), strings.Join(toRestore, ", ")))
	}
	if len(conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("%d database(s) present on both sides (resolve by hand): %s", len(conflicts), strings.Join(conflicts, ", ")))
	}
	r.Message = strings.Join(parts, "; ")
	// A parked database under an active rig breaks that rig; a served one
	// under a suspended rig only costs server CPU.
	r.Status = doctor.StatusWarning
	if len(toRestore) > 0 || len(conflicts) > 0 {
		r.Status = doctor.StatusError
	}
	if len(toPark)+len(toRestore) > 0 {
		r.FixHint = "run: gc doctor --fix (moves databases between .beads/dolt and .beads/dolt-suspended)"
	}
	return r
}

func (c *doltParkedDatabasesCheck) CanFix() bool { return true }

// WarmupEligible keeps the check out of `gc start`'s warm-up scan: parking is
// an operator repair, not a boot gate.
func (c *doltParkedDatabasesCheck) WarmupEligible() bool { return false }

// Fix parks and restores every drifted database; conflicts are left alone and
// reported through the error so the run stays red.
func (c *doltParkedDatabasesCheck) Fix(ctx *doctor.CheckContext) error {
	drifts, err := c.drifts()
	if err != nil {
		return err
	}
	var failures []string
	for _, d := range drifts {
		rig := findConfigRig(c.cfg, d.rig)
		if rig == nil || d.status == doltpark.Conflict {
			failures = append(failures, fmt.Sprintf("%s/%s: %s, resolve by hand", d.rig, d.db, d.status))
			continue
		}
		var moved bool
		var mvErr error
		verb := "parked"
		if d.suspended {
			moved, mvErr = configedit.ParkRigDatabase(c.cityPath, rig)
		} else {
			verb = "restored"
			moved, mvErr = configedit.UnparkRigDatabase(c.cityPath, rig)
		}
		if mvErr != nil {
			failures = append(failures, fmt.Sprintf("%s/%s: %v", d.rig, d.db, mvErr))
			continue
		}
		if moved && ctx != nil && ctx.Output != nil {
			fmt.Fprintf(ctx.Output, "  %s dolt database %s (rig %s)\n", verb, d.db, d.rig) //nolint:errcheck // best-effort doctor output
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("dolt-parked-databases: %s", strings.Join(failures, "; "))
	}
	return nil
}

func findConfigRig(cfg *config.City, name string) *config.Rig {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name {
			return &cfg.Rigs[i]
		}
	}
	return nil
}
