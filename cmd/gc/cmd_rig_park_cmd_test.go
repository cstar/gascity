package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestRigUnparkServesDatabaseWhileRigStaysSuspended(t *testing.T) {
	cityPath := parkRigCity(t)
	calls := stubDoltRestart(t)
	layout := doltpark.DefaultLayout(cityPath)
	var stdout, stderr bytes.Buffer
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("suspend = %d: %s", code, stderr.String())
	}

	stdout.Reset()
	if code := doRigPark(fsys.OSFS{}, cityPath, "frontend", false, &stdout, &stderr); code != 0 {
		t.Fatalf("unpark = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Restored dolt database of rig 'frontend' (rig stays suspended)") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if st, _ := doltpark.Inspect(layout, "fe"); st != doltpark.Served {
		t.Fatalf("database = %s, want served", st)
	}
	st := loadSuspensionStateBestEffort(cityPath)
	if !suspensionstate.IsRigSuspended(st, "frontend") {
		t.Fatal("rig must stay suspended")
	}
	if !suspensionstate.RigDatabaseServed(st, "frontend") {
		t.Fatal("keep-served preference must be recorded")
	}
	if *calls != 2 {
		t.Fatalf("restart calls = %d, want 2 (suspend + unpark)", *calls)
	}

	// Idempotent, no restart.
	stdout.Reset()
	if code := doRigPark(fsys.OSFS{}, cityPath, "frontend", false, &stdout, &stderr); code != 0 {
		t.Fatalf("second unpark = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already served") || *calls != 2 {
		t.Errorf("second unpark: stdout = %q, restarts = %d", stdout.String(), *calls)
	}

	// Doctor honours the preference: nothing to fix.
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	if drifts, err := newDoltParkedDatabasesCheck(cityPath, cfg).drifts(); err != nil || len(drifts) != 0 {
		t.Fatalf("doctor must accept an unparked suspended rig, got %v %v", drifts, err)
	}

	// park reverses it and clears the preference.
	stdout.Reset()
	if code := doRigPark(fsys.OSFS{}, cityPath, "frontend", true, &stdout, &stderr); code != 0 {
		t.Fatalf("park = %d: %s", code, stderr.String())
	}
	if st, _ := doltpark.Inspect(layout, "fe"); st != doltpark.Parked {
		t.Fatalf("database = %s, want parked", st)
	}
	if suspensionstate.RigDatabaseServed(loadSuspensionStateBestEffort(cityPath), "frontend") {
		t.Fatal("park must clear the keep-served preference")
	}
	if *calls != 3 {
		t.Fatalf("restart calls = %d, want 3", *calls)
	}
}

func TestRigParkAndUnparkAreNoOpsOnActiveRig(t *testing.T) {
	cityPath := parkRigCity(t)
	calls := stubDoltRestart(t)
	var stdout, stderr bytes.Buffer
	for _, park := range []bool{true, false} {
		stdout.Reset()
		if code := doRigPark(fsys.OSFS{}, cityPath, "frontend", park, &stdout, &stderr); code != 0 {
			t.Fatalf("park=%v: code %d: %s", park, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "is active") {
			t.Errorf("park=%v: stdout = %q", park, stdout.String())
		}
	}
	if st, _ := doltpark.Inspect(doltpark.DefaultLayout(cityPath), "fe"); st != doltpark.Served {
		t.Fatalf("active rig database = %s, want served", st)
	}
	if *calls != 0 {
		t.Fatalf("no restart expected, got %d", *calls)
	}
}

func TestRigSuspendKeepDatabaseLeavesItServedAndDoctorAgrees(t *testing.T) {
	cityPath := parkRigCity(t)
	calls := stubDoltRestart(t)
	var stdout, stderr bytes.Buffer
	if code := doRigSuspendWithOptions(fsys.OSFS{}, cityPath, "frontend", true, &stdout, &stderr); code != 0 {
		t.Fatalf("suspend --keep-database = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stays served (--keep-database)") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if st, _ := doltpark.Inspect(doltpark.DefaultLayout(cityPath), "fe"); st != doltpark.Served {
		t.Fatalf("database = %s, want served", st)
	}
	st := loadSuspensionStateBestEffort(cityPath)
	if !suspensionstate.IsRigSuspended(st, "frontend") || !suspensionstate.RigDatabaseServed(st, "frontend") {
		t.Fatalf("state = %+v", st.Rigs["frontend"])
	}
	if *calls != 0 {
		t.Fatalf("nothing moved: no restart expected, got %d", *calls)
	}
	// Resume drops the preference.
	if code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("resume = %d: %s", code, stderr.String())
	}
	if suspensionstate.RigDatabaseServed(loadSuspensionStateBestEffort(cityPath), "frontend") {
		t.Fatal("resume must clear the keep-served preference")
	}
}

func TestRigSuspendWithoutKeepClearsAnEarlierUnpark(t *testing.T) {
	cityPath := parkRigCity(t)
	stubDoltRestart(t)
	var stdout, stderr bytes.Buffer
	if code := doRigSuspendWithOptions(fsys.OSFS{}, cityPath, "frontend", true, &stdout, &stderr); code != 0 {
		t.Fatalf("suspend --keep-database = %d: %s", code, stderr.String())
	}
	if code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("resume = %d: %s", code, stderr.String())
	}
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("suspend = %d: %s", code, stderr.String())
	}
	if st, _ := doltpark.Inspect(doltpark.DefaultLayout(cityPath), "fe"); st != doltpark.Parked {
		t.Fatalf("plain suspend must park: %s", st)
	}
}
