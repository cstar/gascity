package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

// gc rig park / gc rig unpark move a suspended rig's Dolt database without
// changing the suspension itself. `unpark` is how an operator reads a
// suspended rig's beads or runs a skipper against it without waking the
// rig's agents; `park` is the way back. Both act locally (they do not go
// through the supervisor API) and restart the managed server once.

func newRigUnparkCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "unpark [name]",
		Short: "Serve a suspended rig's Dolt database again (rig stays suspended)",
		Long: `Restore a suspended rig's Dolt database from .beads/dolt-suspended to the
managed server's data directory and record a keep-served preference, so
"gc doctor --fix" leaves it alone. The rig stays suspended: no agents, no
orders. Use it to read the rig's beads or run a skipper against the rig.
"gc rig park" reverses it; "gc rig suspend" (without --keep-database) also
clears the preference and parks again.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigPark(args, false, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
}

func newRigParkCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "park [name]",
		Short: "Park a suspended rig's Dolt database out of the managed server",
		Long: `Move a suspended rig's Dolt database from the managed server's data
directory to .beads/dolt-suspended and clear any keep-served preference.
The rig must already be suspended; an active rig's database is never parked.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigPark(args, true, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
}

func cmdRigPark(args []string, park bool, stdout, stderr io.Writer) int {
	verb := "unpark"
	if park {
		verb = "park"
	}
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigName := ctx.RigName
	if len(args) > 0 {
		rigName = args[0]
	}
	if rigName == "" {
		fmt.Fprintf(stderr, "gc rig %s: missing rig name\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigPark(fsys.OSFS{}, ctx.CityPath, rigName, park, stdout, stderr)
}

// setRigDatabaseServedPreference records (or clears) only the keep-served
// preference, leaving the rig's suspension preference untouched.
func setRigDatabaseServedPreference(fs fsys.FS, cityPath, rigName string, served *bool) error {
	st, err := loadSuspensionState(fs, cityPath)
	if err != nil {
		return err
	}
	suspensionstate.SetRigDatabaseServed(&st, rigName, served)
	return saveSuspensionState(fs, cityPath, st)
}

// doRigPark parks (park=true) or restores (park=false) the database of a
// suspended rig and records the matching keep-served preference. It refuses
// nothing on an active rig: its database is served by definition, so both
// directions are a no-op there.
func doRigPark(fs fsys.FS, cityPath, rigName string, park bool, stdout, stderr io.Writer) int {
	verb := "unpark"
	if park {
		verb = "park"
	}
	cfg, err := loadCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc rig %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	var rig *config.Rig
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			rig = &cfg.Rigs[i]
			break
		}
	}
	if rig == nil {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig "+verb, rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	st, err := loadSuspensionState(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig %s: reading state: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !buildEffectiveSuspendedRigNames(cfg, st)[rigName] {
		fmt.Fprintf(stdout, "Rig '%s' is active; its dolt database is served\n", rigName) //nolint:errcheck // best-effort stdout
		return 0
	}

	if park {
		// Preference first, then the move: on failure the rig is "parked by
		// preference" and doctor --fix retries the move.
		if err := setRigDatabaseServedPreference(fs, cityPath, rigName, nil); err != nil {
			fmt.Fprintf(stderr, "gc rig park: writing state: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		moved, err := configedit.ParkRigDatabase(cityPath, rig)
		if err != nil {
			fmt.Fprintf(stderr, "gc rig park: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !moved {
			fmt.Fprintf(stdout, "Dolt database of rig '%s' is already parked\n", rigName) //nolint:errcheck // best-effort stdout
			return 0
		}
		fmt.Fprintf(stdout, "Parked dolt database of rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	} else {
		// Move first, then the preference: on failure nothing claims the
		// database is served.
		moved, err := configedit.UnparkRigDatabase(cityPath, rig)
		if err != nil {
			fmt.Fprintf(stderr, "gc rig unpark: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		keep := true
		if err := setRigDatabaseServedPreference(fs, cityPath, rigName, &keep); err != nil {
			fmt.Fprintf(stderr, "gc rig unpark: writing state: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !moved {
			fmt.Fprintf(stdout, "Dolt database of rig '%s' is already served (rig stays suspended)\n", rigName) //nolint:errcheck // best-effort stdout
			return 0
		}
		fmt.Fprintf(stdout, "Restored dolt database of rig '%s' (rig stays suspended)\n", rigName) //nolint:errcheck // best-effort stdout
	}
	if err := restartManagedDoltAfterPark(cityPath, stdout); err != nil {
		fmt.Fprintf(stderr, "gc rig %s: database moved but the server restart failed: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}
