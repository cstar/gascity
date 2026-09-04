package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// restartManagedDoltAfterPark restarts the managed Dolt server so it forgets
// parked databases and discovers restored ones. Dolt (2.3) enumerates its
// data directory only at startup: a moved-out database lingers in the listing
// and breaks every INFORMATION_SCHEMA query with "no root value found in
// session", and a moved-in one is invisible until the next start. The pack
// command `gc dolt restart` already owns the lifecycle lock, watchdog and
// published state, so run it rather than re-implementing that sequence.
var restartManagedDoltAfterPark = func(cityPath string, out io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating gc binary: %w", err)
	}
	cmd := exec.Command(self, "--city", cityPath, "dolt", "restart")
	cmd.Env = append(os.Environ(), "GC_CITY_PATH="+cityPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gc dolt restart: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if out != nil {
		fmt.Fprintf(out, "Restarted managed dolt server so it re-reads %s\n", doltpark.DefaultLayout(cityPath).DataDir) //nolint:errcheck // best-effort output
	}
	return nil
}

// newControllerConfigEditor builds the supervisor's city.toml editor with the
// managed-server restart installed, so `gc rig suspend/resume` over the API
// behaves like the CLI fallback.
func newControllerConfigEditor(cityPath, tomlPath string) *configedit.Editor {
	ed := configedit.NewEditor(fsys.OSFS{}, tomlPath)
	ed.SetAfterDoltMoveHook(func() error { return restartManagedDoltAfterPark(cityPath, os.Stderr) })
	return ed
}

// rigsForBeadsLifecycleInit returns the rigs whose beads scope the supervisor
// must initialize at start: every rig with a path that is not effectively
// suspended. A suspended rig's Dolt database is parked, so running bd init
// against it would either fail bd's init-safety check or create a fresh,
// empty database beside the parked one.
func rigsForBeadsLifecycleInit(cfg *config.City, st suspensionstate.State) []*config.Rig {
	if cfg == nil {
		return nil
	}
	suspended := buildEffectiveSuspendedRigNames(cfg, st)
	var out []*config.Rig
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) == "" || suspended[rig.Name] {
			continue
		}
		out = append(out, rig)
	}
	return out
}
