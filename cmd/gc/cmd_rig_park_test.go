package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
)

// parkRigCity writes a schema-2 city with rig "frontend" (prefix "fe") bound
// to a path under the city, and a served Dolt database at .beads/dolt/fe.
func parkRigCity(t *testing.T) string {
	t.Helper()
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"frontend\"\npath = \"" + rigPath + "\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n\n[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n", siteToml)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "fe", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestDoRigSuspendParksDatabaseAndResumeRestoresIt(t *testing.T) {
	cityPath := parkRigCity(t)
	layout := doltpark.DefaultLayout(cityPath)

	var stdout, stderr bytes.Buffer
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("doRigSuspend = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Parked dolt database of rig 'frontend'") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if st, _ := doltpark.Inspect(layout, "fe"); st != doltpark.Parked {
		t.Fatalf("after suspend: %s, want parked", st)
	}

	stdout.Reset()
	if code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("doRigResume = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Restored dolt database of rig 'frontend'") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if st, _ := doltpark.Inspect(layout, "fe"); st != doltpark.Served {
		t.Fatalf("after resume: %s, want served", st)
	}
}

func TestDoRigResumeRefusesToFlipStateWhenDatabaseCannotBeRestored(t *testing.T) {
	cityPath := parkRigCity(t)
	var stdout, stderr bytes.Buffer
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("doRigSuspend = %d, stderr: %s", code, stderr.String())
	}
	// Both sides present: unpark must refuse.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt", "fe"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code == 0 {
		t.Fatal("doRigResume should fail on a conflict")
	}
	if !strings.Contains(stderr.String(), "restoring dolt database") {
		t.Errorf("stderr = %q", stderr.String())
	}
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !buildEffectiveSuspendedRigNames(cfg, loadSuspensionStateBestEffort(cityPath))["frontend"] {
		t.Fatal("rig must remain suspended")
	}
}

func TestDoRigSuspendWithoutDatabaseStaysQuiet(t *testing.T) {
	cityPath := parkRigCity(t)
	if err := os.RemoveAll(filepath.Join(cityPath, ".beads", "dolt", "fe")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("doRigSuspend = %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Parked") {
		t.Errorf("no database: stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "dolt-suspended")); !os.IsNotExist(err) {
		t.Errorf("parked dir must not be created: %v", err)
	}
}
