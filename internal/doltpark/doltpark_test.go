package doltpark

import (
	"os"
	"path/filepath"
	"testing"
)

func layoutWith(t *testing.T, served, parked []string) Layout {
	t.Helper()
	root := t.TempDir()
	l := Layout{DataDir: filepath.Join(root, "dolt"), ParkedDir: filepath.Join(root, "dolt-suspended")}
	for _, db := range served {
		mkdb(t, filepath.Join(l.DataDir, db))
	}
	for _, db := range parked {
		mkdb(t, filepath.Join(l.ParkedDir, db))
	}
	return l
}

func mkdb(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".dolt", "noms", "manifest"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultLayoutUsesBeadsDolt(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	l := DefaultLayout("/city")
	if l.DataDir != filepath.Join("/city", ".beads", "dolt") {
		t.Fatalf("DataDir = %q", l.DataDir)
	}
	if l.ParkedDir != filepath.Join("/city", ".beads", "dolt-suspended") {
		t.Fatalf("ParkedDir = %q", l.ParkedDir)
	}
}

func TestDefaultLayoutHonorsEnvOverride(t *testing.T) {
	t.Setenv(EnvDataDir, "/elsewhere/data")
	l := DefaultLayout("/city")
	if l.DataDir != "/elsewhere/data" || l.ParkedDir != "/elsewhere/data-suspended" {
		t.Fatalf("layout = %+v", l)
	}
}

func TestEnabledDefaultsOnAndHonorsOff(t *testing.T) {
	t.Setenv(EnvParkOnSuspend, "")
	if !Enabled() {
		t.Fatal("default should be enabled")
	}
	for _, v := range []string{"0", "false", "NO", "off"} {
		t.Setenv(EnvParkOnSuspend, v)
		if Enabled() {
			t.Fatalf("%q should disable", v)
		}
	}
}

func TestDatabaseNamePrefersMetadataThenPrefix(t *testing.T) {
	rig := t.TempDir()
	if got := DatabaseName(rig, "ab"); got != "ab" {
		t.Fatalf("no metadata: got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"dolt_database":"custom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DatabaseName(rig, "ab"); got != "custom" {
		t.Fatalf("metadata: got %q", got)
	}
	if got := DatabaseName("", " ab "); got != "ab" {
		t.Fatalf("empty rig path: got %q", got)
	}
}

func TestParkMovesServedDatabaseAndIsIdempotent(t *testing.T) {
	l := layoutWith(t, []string{"vg"}, nil)
	moved, err := Park(l, "vg")
	if err != nil || !moved {
		t.Fatalf("Park = %v, %v", moved, err)
	}
	if st, _ := Inspect(l, "vg"); st != Parked {
		t.Fatalf("after park: %s", st)
	}
	if _, err := os.Stat(filepath.Join(l.ParkedDir, "vg", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("contents did not travel: %v", err)
	}
	moved, err = Park(l, "vg")
	if err != nil || moved {
		t.Fatalf("second Park = %v, %v; want no-op", moved, err)
	}
}

func TestUnparkRestoresAndIsIdempotent(t *testing.T) {
	l := layoutWith(t, nil, []string{"vg"})
	moved, err := Unpark(l, "vg")
	if err != nil || !moved {
		t.Fatalf("Unpark = %v, %v", moved, err)
	}
	if st, _ := Inspect(l, "vg"); st != Served {
		t.Fatalf("after unpark: %s", st)
	}
	moved, err = Unpark(l, "vg")
	if err != nil || moved {
		t.Fatalf("second Unpark = %v, %v; want no-op", moved, err)
	}
}

func TestMissingDatabaseIsNoOpBothWays(t *testing.T) {
	l := layoutWith(t, nil, nil)
	if moved, err := Park(l, "vg"); err != nil || moved {
		t.Fatalf("Park missing = %v, %v", moved, err)
	}
	if moved, err := Unpark(l, "vg"); err != nil || moved {
		t.Fatalf("Unpark missing = %v, %v", moved, err)
	}
	if _, err := os.Stat(l.ParkedDir); !os.IsNotExist(err) {
		t.Fatalf("no-op must not create the parked dir: %v", err)
	}
}

func TestConflictIsReportedNeverResolved(t *testing.T) {
	l := layoutWith(t, []string{"vg"}, []string{"vg"})
	st, err := Inspect(l, "vg")
	if err != nil || st != Conflict {
		t.Fatalf("Inspect = %s, %v", st, err)
	}
	if _, err := Park(l, "vg"); err == nil {
		t.Fatal("Park on conflict should fail")
	}
	if _, err := Unpark(l, "vg"); err == nil {
		t.Fatal("Unpark on conflict should fail")
	}
	for _, dir := range []string{l.DataDir, l.ParkedDir} {
		if !isDir(filepath.Join(dir, "vg")) {
			t.Fatalf("conflict handling must leave %s untouched", dir)
		}
	}
}

func TestReservedAndMalformedNamesAreRejected(t *testing.T) {
	l := layoutWith(t, []string{"hq"}, nil)
	for _, db := range []string{"hq", "HQ", "mysql", "information_schema", "", "../x", "a/b", ".hidden"} {
		if _, err := Park(l, db); err == nil {
			t.Errorf("Park(%q) should be rejected", db)
		}
		if _, err := Unpark(l, db); err == nil {
			t.Errorf("Unpark(%q) should be rejected", db)
		}
	}
	if !isDir(filepath.Join(l.DataDir, "hq")) {
		t.Fatal("hq must never move")
	}
}
