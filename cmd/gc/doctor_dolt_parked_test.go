package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// parkedCheckCity builds a city with three rigs: "dormant" (suspended, db
// still served), "awake" (active, db wrongly parked) and "fine" (active, db
// served). Returns the city path and the config with rig paths resolved.
func parkedCheckCity(t *testing.T) (string, *config.City) {
	t.Helper()
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	cityPath := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(parts...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(cityPath, ".beads", "dolt", "dm", ".dolt")
	mk(cityPath, ".beads", "dolt-suspended", "aw", ".dolt")
	mk(cityPath, ".beads", "dolt", "fn", ".dolt")
	mk(cityPath, ".gc")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "dormant", Prefix: "dm", Path: filepath.Join(cityPath, "rigs", "dormant"), SuspendedOnStart: true},
		{Name: "awake", Prefix: "aw", Path: filepath.Join(cityPath, "rigs", "awake")},
		{Name: "fine", Prefix: "fn", Path: filepath.Join(cityPath, "rigs", "fine")},
	}}
	for _, r := range cfg.Rigs {
		mk(r.Path)
	}
	return cityPath, cfg
}

func TestDoltParkedDatabasesCheckReportsBothDriftDirections(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	res := newDoltParkedDatabasesCheck(cityPath, cfg).Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (an active rig's database is parked): %s", res.Status, res.Message)
	}
	for _, want := range []string{"dormant/dm", "awake/aw"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %s", res.Message, want)
		}
	}
	if strings.Contains(res.Message, "fine/fn") {
		t.Errorf("message %q must not flag the healthy rig", res.Message)
	}
	if !strings.Contains(res.FixHint, "--fix") {
		t.Errorf("FixHint = %q", res.FixHint)
	}
}

func TestDoltParkedDatabasesCheckFixMovesBothWaysThenPasses(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	check := newDoltParkedDatabasesCheck(cityPath, cfg)
	var out bytes.Buffer
	if err := check.Fix(&doctor.CheckContext{CityPath: cityPath, Output: &out}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	layout := doltpark.DefaultLayout(cityPath)
	if st, _ := doltpark.Inspect(layout, "dm"); st != doltpark.Parked {
		t.Errorf("dm = %s, want parked", st)
	}
	if st, _ := doltpark.Inspect(layout, "aw"); st != doltpark.Served {
		t.Errorf("aw = %s, want served", st)
	}
	if st, _ := doltpark.Inspect(layout, "fn"); st != doltpark.Served {
		t.Errorf("fn = %s, want served (untouched)", st)
	}
	if !strings.Contains(out.String(), "parked dolt database dm") || !strings.Contains(out.String(), "restored dolt database aw") {
		t.Errorf("fix output = %q", out.String())
	}
	res := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusOK {
		t.Fatalf("after fix: status = %v: %s", res.Status, res.Message)
	}
}

func TestDoltParkedDatabasesCheckHonorsRuntimeSuspensionState(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	// Explicit resume of "dormant" overrides suspended_on_start: its served
	// database is then correct, and only "awake" remains wrong.
	f := false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "dormant", &f); err != nil {
		t.Fatal(err)
	}
	res := newDoltParkedDatabasesCheck(cityPath, cfg).Run(&doctor.CheckContext{CityPath: cityPath})
	if strings.Contains(res.Message, "dormant/dm") {
		t.Errorf("resumed rig must not be flagged: %s", res.Message)
	}
	if !strings.Contains(res.Message, "awake/aw") {
		t.Errorf("parked active rig must be flagged: %s", res.Message)
	}
}

func TestDoltParkedDatabasesCheckLeavesConflictsAndStaysRed(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	// Make "dormant" a conflict: present on both sides.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt-suspended", "dm"), 0o755); err != nil {
		t.Fatal(err)
	}
	check := newDoltParkedDatabasesCheck(cityPath, cfg)
	err := check.Fix(&doctor.CheckContext{CityPath: cityPath, Output: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "dormant/dm") {
		t.Fatalf("Fix should report the conflict, got %v", err)
	}
	layout := doltpark.DefaultLayout(cityPath)
	if st, _ := doltpark.Inspect(layout, "dm"); st != doltpark.Conflict {
		t.Errorf("conflict must be left untouched, got %s", st)
	}
	if st, _ := doltpark.Inspect(layout, "aw"); st != doltpark.Served {
		t.Errorf("the fixable rig must still be fixed, got %s", st)
	}
	res := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusError || !strings.Contains(res.Message, "both sides") {
		t.Fatalf("after fix: %v %s", res.Status, res.Message)
	}
}

func TestDoltParkedDatabasesCheckDisabledIsOK(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	t.Setenv(doltpark.EnvParkOnSuspend, "off")
	res := newDoltParkedDatabasesCheck(cityPath, cfg).Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v: %s", res.Status, res.Message)
	}
}
