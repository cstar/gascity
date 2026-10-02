package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func snapshotFixture(t *testing.T) (*fsys.Fake, string, []string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "scope")
	home := filepath.Join(t.TempDir(), "home")
	env := []string{"BEADS_DIR=" + filepath.Join(root, ".beads"), "HOME=" + home, "USERPROFILE=" + home}
	f := fsys.NewFake()
	return f, root, env
}

func snapshotFile(t *testing.T, f *fsys.Fake, path, content string) {
	t.Helper()
	if err := f.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestControlReadySnapshotTracksLocalGlobalAndMovedDoltInputs(t *testing.T) {
	f, root, env := snapshotFixture(t)
	moved := filepath.Join(t.TempDir(), "moved-dolt")
	env = append(env, "BEADS_DOLT_DATA_DIR="+moved, "BEADS_CENTRAL_CONFIG="+filepath.Join(root, "central.json"), "BEADS_CREDENTIALS_FILE="+filepath.Join(root, "credentials"))
	first, err := controlReadyCaptureOpening(f, root, env, []byte("configuration"))
	if err != nil {
		t.Fatal(err)
	}
	if first.FallbackReason != "" {
		t.Fatal(first.FallbackReason)
	}
	paths := []string{filepath.Join(root, ".beads/config.yaml"), filepath.Join(root, ".beads/config.local.yaml"), filepath.Join(root, ".beads/dolt-server.port"), filepath.Join(root, ".beads/hooks/on_update"), filepath.Join(moved, "config.yaml"), filepath.Join(root, "central.json"), filepath.Join(root, "credentials"), filepath.Join(envListValue(env, "HOME"), ".beads/config.yaml"), filepath.Join(envListValue(env, "HOME"), ".config/bd/config.yaml")}
	for _, path := range paths {
		snapshotFile(t, f, path, "fixture: true")
		next, err := controlReadyCaptureOpening(f, root, env, []byte("configuration"))
		if err != nil {
			t.Fatal(err)
		}
		if next.Fingerprint == first.Fingerprint {
			t.Fatalf("input ignored: %s", path)
		}
		if err := f.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlReadySnapshotTracksMetadataDataDirectory(t *testing.T) {
	f, root, env := snapshotFixture(t)
	snapshotFile(t, f, filepath.Join(root, ".beads/metadata.json"), `{"backend":"dolt","dolt_data_dir":"custom"}`)
	first, err := controlReadyCaptureOpening(f, root, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshotFile(t, f, filepath.Join(root, ".beads/custom/config.yaml"), "fixture: true")
	next, err := controlReadyCaptureOpening(f, root, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Fingerprint == first.Fingerprint {
		t.Fatal("metadata-directed data root ignored")
	}
}

func TestControlReadySnapshotDeclinesImplicitDiscoveryAndRedirects(t *testing.T) {
	f, root, env := snapshotFixture(t)
	noDiscovery, err := controlReadyCaptureOpening(f, root, env[1:], nil)
	if err != nil || noDiscovery.FallbackReason == "" {
		t.Fatalf("implicit discovery accepted: %v %v", noDiscovery, err)
	}
	snapshotFile(t, f, filepath.Join(root, ".beads/redirect"), "../other/.beads")
	redirect, err := controlReadyCaptureOpening(f, root, env, nil)
	if err != nil || redirect.FallbackReason == "" {
		t.Fatalf("redirect accepted: %v %v", redirect, err)
	}
	if noDiscovery.Fingerprint == redirect.Fingerprint {
		t.Fatal("discovery mode change ignored")
	}
}

func TestControlReadySnapshotOpeningEnvironmentAndErrors(t *testing.T) {
	f, root, env := snapshotFixture(t)
	env = append(env, "BEADS_DOLT_SERVER_PORT=1111", "BEADS_DOLT_SERVER_PORT=2222")
	got, err := controlReadyCaptureOpening(f, root, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["BEADS_DOLT_SERVER_PORT"] != "2222" || got.Env["BEADS_DOLT_AUTO_START"] != "0" || got.Env["BEADS_DOLT_MAX_CONNS"] != "1" {
		t.Fatal("opening environment lost authority")
	}
	f.Errors[filepath.Join(root, ".beads/config.local.yaml")] = os.ErrPermission
	bad, err := controlReadyCaptureOpening(f, root, env, nil)
	if !errors.Is(err, os.ErrPermission) || bad != nil {
		t.Fatalf("unreadable input accepted: %v %v", bad, err)
	}
}

func TestControlReadySnapshotDeclinesYAMLSharedServerAuthority(t *testing.T) {
	for _, yaml := range []string{"dolt:\n  shared-server: true\n", "dolt.shared-server: true\n"} {
		t.Run(yaml, func(t *testing.T) {
			f, root, env := snapshotFixture(t)
			snapshotFile(t, f, filepath.Join(root, ".beads/config.yaml"), yaml)
			got, err := controlReadyCaptureOpening(f, root, env, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.FallbackReason == "" {
				t.Fatal("unresolved YAML shared-server authority accepted")
			}
		})
	}
}

func TestControlReadySnapshotDeclinesSymlinkedScopeParent(t *testing.T) {
	f, root, env := snapshotFixture(t)
	f.Symlinks[filepath.Dir(root)] = filepath.Join(t.TempDir(), "other-parent")
	got, err := controlReadyCaptureOpening(f, root, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.FallbackReason == "" {
		t.Fatal("symlinked scope parent accepted")
	}
}
