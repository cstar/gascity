package configedit_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doltpark"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestAfterDoltMoveHookRunsOnlyWhenADatabaseMoved(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	dir, path := parkCity(t)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	calls := 0
	ed.SetAfterDoltMoveHook(func() error { calls++; return nil })

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	if calls != 1 {
		t.Fatalf("hook calls after suspend = %d, want 1", calls)
	}
	if err := ed.ResumeRig("my-rig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}
	if calls != 2 {
		t.Fatalf("hook calls after resume = %d, want 2", calls)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".beads", "dolt", "mr")); err != nil {
		t.Fatal(err)
	}
	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig without database: %v", err)
	}
	if calls != 2 {
		t.Fatalf("hook must not run when nothing moved, calls = %d", calls)
	}
}

func TestResumeRigDoesNotFlipStateWhenHookFails(t *testing.T) {
	t.Setenv(doltpark.EnvParkOnSuspend, "")
	t.Setenv(doltpark.EnvDataDir, "")
	dir, path := parkCity(t)
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}
	ed.SetAfterDoltMoveHook(func() error { return errors.New("restart boom") })

	if err := ed.ResumeRig("my-rig"); err == nil {
		t.Fatal("ResumeRig should surface the hook failure")
	}
	st, _ := suspensionstate.Load(fsys.OSFS{}, dir)
	if !suspensionstate.IsRigSuspended(st, "my-rig") {
		t.Fatal("rig must stay suspended when the server could not be restarted")
	}
	// The database itself was restored to the served side and stays there;
	// the next resume (or doctor --fix) retries the restart.
	if s, _ := doltpark.Inspect(doltpark.DefaultLayout(dir), "mr"); s != doltpark.Served {
		t.Fatalf("database = %s, want served", s)
	}
}
