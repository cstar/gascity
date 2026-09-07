package configedit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// parkCity writes a city with one rig ("my-rig", prefix "mr") whose Dolt
// database is served under .beads/dolt/mr, and returns the city dir.
func parkCity(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	rigDir := filepath.Join(dir, "rigs", "my-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeTOML(t, dir, "[workspace]\nname = \"c\"\n\n[[rigs]]\nname = \"my-rig\"\nprefix = \"mr\"\npath = \"rigs/my-rig\"\n")
	served := filepath.Join(dir, ".beads", "dolt", "mr", ".dolt")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(served, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestSuspendRigParksDoltDatabaseAndResumeRestoresIt(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	dir, path := parkCity(t)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	layout := doltpark.DefaultLayout(dir)

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if st, _ := doltpark.Inspect(layout, "mr"); st != doltpark.Parked {
		t.Fatalf("after suspend: database %s, want parked", st)
	}
	if _, err := os.Stat(filepath.Join(layout.ParkedDir, "mr", ".dolt", "marker")); err != nil {
		t.Fatalf("contents did not travel: %v", err)
	}
	susp, _ := suspensionstate.Load(fsys.OSFS{}, dir)
	if !suspensionstate.IsRigSuspended(susp, "my-rig") {
		t.Fatal("rig should be suspended")
	}

	if err := ed.ResumeRig("my-rig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}
	if st, _ := doltpark.Inspect(layout, "mr"); st != doltpark.Served {
		t.Fatalf("after resume: database %s, want served", st)
	}
	susp, _ = suspensionstate.Load(fsys.OSFS{}, dir)
	if suspensionstate.IsRigSuspended(susp, "my-rig") {
		t.Fatal("rig should be resumed")
	}
}

func TestSuspendRigLeavesDatabaseWhenParkingDisabled(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "0")
	t.Setenv(doltpark.EnvDataDir, "")
	dir, path := parkCity(t)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if st, _ := doltpark.Inspect(doltpark.DefaultLayout(dir), "mr"); st != doltpark.Served {
		t.Fatalf("database %s, want served (parking disabled)", st)
	}
}

func TestResumeRigFailsClosedOnConflict(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	dir, path := parkCity(t)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	// Recreate a served copy by hand: both sides now exist.
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "dolt", "mr"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ed.ResumeRig("my-rig"); err == nil {
		t.Fatal("ResumeRig should fail on a conflict")
	}
	susp, _ := suspensionstate.Load(fsys.OSFS{}, dir)
	if !suspensionstate.IsRigSuspended(susp, "my-rig") {
		t.Fatal("rig must stay suspended when its database cannot be restored")
	}
}

func TestSuspendRigWithoutDatabaseIsSilent(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	dir := t.TempDir()
	path := writeTOML(t, dir, "[workspace]\nname = \"c\"\n\n[[rigs]]\nname = \"my-rig\"\nprefix = \"mr\"\n")
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "dolt-suspended")); !os.IsNotExist(err) {
		t.Fatalf("no database: parked dir must not be created (%v)", err)
	}
}
