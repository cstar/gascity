package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// stubDoltRestart replaces the managed-server restart with a counter for the
// duration of the test and returns a pointer to the call count.
func stubDoltRestart(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := restartManagedDoltAfterPark
	restartManagedDoltAfterPark = func(string, io.Writer) error { calls++; return nil }
	t.Cleanup(func() { restartManagedDoltAfterPark = prev })
	return &calls
}

// The pack script behind `gc dolt restart` accepts only [--force]; any other
// argument (a --city flag, for one) makes it exit 64 and the stale listing
// survives, which is exactly the outage this restart exists to prevent.
func TestManagedDoltRestartArgsCarryNoFlags(t *testing.T) {
	args := managedDoltRestartArgs()
	if len(args) != 2 || args[0] != "dolt" || args[1] != "restart" {
		t.Fatalf("args = %v, want [dolt restart]", args)
	}
}

// After `gc dolt restart` the published port file is empty for a few seconds
// (measured: empty at t+0, 42188 by t+5s); bd dials 127.0.0.1:0 meanwhile.
func TestWaitForPublishedDoltPortWaitsForANonZeroPort(t *testing.T) {
	dir := t.TempDir()
	portFile := filepath.Join(dir, "dolt-server.port")
	if err := os.WriteFile(portFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if waitForPublishedDoltPort(portFile, 300*time.Millisecond) {
		t.Fatal("empty port file must not count as published")
	}
	if err := os.WriteFile(portFile, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if waitForPublishedDoltPort(portFile, 300*time.Millisecond) {
		t.Fatal("port 0 must not count as published")
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(portFile, []byte("42188\n"), 0o644)
	}()
	start := time.Now()
	if !waitForPublishedDoltPort(portFile, 5*time.Second) {
		t.Fatal("port written during the wait must be seen")
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("returned before the port was written")
	}
}

func TestRigsForBeadsLifecycleInitSkipsSuspendedAndPathless(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "active", Path: "/r/active"},
		{Name: "on-start", Path: "/r/on-start", SuspendedOnStart: true},
		{Name: "runtime", Path: "/r/runtime"},
		{Name: "resumed", Path: "/r/resumed", SuspendedOnStart: true},
		{Name: "pathless"},
	}}
	var st suspensionstate.State
	tr, fa := true, false
	suspensionstate.SetRig(&st, "runtime", &tr)
	suspensionstate.SetRig(&st, "resumed", &fa)

	var got []string
	for _, r := range rigsForBeadsLifecycleInit(cfg, st) {
		got = append(got, r.Name)
	}
	want := []string{"active", "resumed"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if rigsForBeadsLifecycleInit(nil, st) != nil {
		t.Fatal("nil config must yield nil")
	}
}

func TestDoRigSuspendRestartsServerOnlyWhenSomethingMoved(t *testing.T) {
	cityPath := parkRigCity(t)
	calls := stubDoltRestart(t)
	var stdout, stderr bytes.Buffer
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("suspend = %d: %s", code, stderr.String())
	}
	if *calls != 1 {
		t.Fatalf("restart calls after suspend = %d, want 1", *calls)
	}
	if code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("resume = %d: %s", code, stderr.String())
	}
	if *calls != 2 {
		t.Fatalf("restart calls after resume = %d, want 2", *calls)
	}
	// Nothing to move: no restart.
	if err := os.RemoveAll(filepath.Join(cityPath, ".beads", "dolt", "fe")); err != nil {
		t.Fatal(err)
	}
	if code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr); code != 0 {
		t.Fatalf("suspend = %d: %s", code, stderr.String())
	}
	if *calls != 2 {
		t.Fatalf("restart calls with no database = %d, want 2", *calls)
	}
}

func TestDoltParkedDatabasesFixRestartsServerOnce(t *testing.T) {
	cityPath, cfg := parkedCheckCity(t)
	calls := stubDoltRestart(t)
	check := newDoltParkedDatabasesCheck(cityPath, cfg)
	if err := check.Fix(&doctor.CheckContext{CityPath: cityPath, Output: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("restart calls = %d, want exactly 1 for two moves", *calls)
	}
	// Second fix has nothing to move: no restart.
	if err := check.Fix(&doctor.CheckContext{CityPath: cityPath, Output: &bytes.Buffer{}}); err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("restart calls after no-op fix = %d, want 1", *calls)
	}
}
