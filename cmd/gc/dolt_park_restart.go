package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	// `gc dolt restart` is a pack command whose script only accepts
	// [--force]; a --city flag would be handed to it verbatim and rejected
	// (exit 64), so the city is passed by cwd and GC_CITY_PATH instead.
	cmd := exec.Command(self, managedDoltRestartArgs()...)
	cmd.Dir = cityPath
	cmd.Env = append(os.Environ(), "GC_CITY_PATH="+cityPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gc dolt restart: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if out != nil {
		fmt.Fprintf(out, "Restarted managed dolt server so it re-reads %s\n", doltpark.DefaultLayout(cityPath).DataDir) //nolint:errcheck // best-effort output
	}
	// The restart clears the published .beads/dolt-server.port files and the
	// supervisor republishes them a few seconds later; until then any bd call
	// dials 127.0.0.1:0. Wait so `gc rig unpark && bd ...` is race-free.
	if !waitForPublishedDoltPort(filepath.Join(cityPath, ".beads", "dolt-server.port"), doltPortRepublishTimeout) {
		if out != nil {
			fmt.Fprintf(out, "warning: %s not republished within %s; bd may need a moment\n", ".beads/dolt-server.port", doltPortRepublishTimeout) //nolint:errcheck // best-effort output
		}
	}
	return nil
}

const doltPortRepublishTimeout = 30 * time.Second

// waitForPublishedDoltPort polls until portFile holds a non-empty, non-zero
// port or the timeout elapses. It reports whether a port was seen.
func waitForPublishedDoltPort(portFile string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(portFile); err == nil {
			if p := strings.TrimSpace(string(data)); p != "" && p != "0" {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// managedDoltRestartArgs is the exact argv (after the binary) used to restart
// the managed server: the pack command with no extra flags.
func managedDoltRestartArgs() []string { return []string{"dolt", "restart"} }

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
