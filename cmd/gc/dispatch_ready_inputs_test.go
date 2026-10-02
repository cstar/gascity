package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestControlReadyCollectFilesReadsContentAndMode(t *testing.T) {
	f := fsys.NewFake()
	if err := f.MkdirAll("/scope", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("/scope/hook", []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/scope/hook", "/scope/missing"}
	first, err := controlReadyCollectFiles(f, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !first[paths[0]].Exists || string(first[paths[0]].Content) != "first" || first[paths[1]].Exists {
		t.Fatalf("file inputs not faithfully collected: %#v", first)
	}
	if err := f.WriteFile(paths[0], []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(paths[0], 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := controlReadyCollectFiles(f, paths)
	if err != nil {
		t.Fatal(err)
	}
	if second[paths[0]].Mode&0o111 == 0 || controlReadyOpeningFingerprint(nil, nil, first) == controlReadyOpeningFingerprint(nil, nil, second) {
		t.Fatal("same-length content change or executable mode was lost")
	}
}

func TestControlReadyCollectFilesFailsClosed(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/secret"] = os.ErrPermission
	inputs, err := controlReadyCollectFiles(f, []string{"/missing", "/secret"})
	if !errors.Is(err, os.ErrPermission) || inputs != nil {
		t.Fatalf("unreadable authority input accepted: inputs=%v err=%v", inputs, err)
	}
}

func TestControlReadyFingerprintOpeningAuthority(t *testing.T) {
	env := map[string]string{"BEADS_DOLT_SERVER_PORT": "42188", "BEADS_DOLT_PASSWORD": "fixture-secret"}
	config := []byte("resolved provider and city configuration")
	files := map[string]controlReadyFileInput{"metadata": {Exists: true, Mode: 0o600, Content: []byte("project identity")}, "config": {Exists: true, Mode: 0o600, Content: []byte("dolt.mode: server")}, "credential": {Exists: true, Mode: 0o600, Content: []byte("fixture-secret")}, "hook": {Exists: true, Mode: 0o644, Content: []byte("hook content")}}
	original := controlReadyOpeningFingerprint(env, config, files)
	if len(original) != 64 || strings.Contains(original, "fixture-secret") {
		t.Fatalf("unsafe fingerprint %q", original)
	}
	reordered := map[string]string{"BEADS_DOLT_PASSWORD": "fixture-secret", "BEADS_DOLT_SERVER_PORT": "42188"}
	if original != controlReadyOpeningFingerprint(reordered, config, files) {
		t.Fatal("map insertion order changed identity")
	}
	if original == controlReadyOpeningFingerprint(map[string]string{"BEADS_DOLT_SERVER_PORT": "12345", "BEADS_DOLT_PASSWORD": "fixture-secret"}, config, files) {
		t.Fatal("endpoint change ignored")
	}
	if original == controlReadyOpeningFingerprint(env, []byte("new provider"), files) {
		t.Fatal("resolved configuration change ignored")
	}
	for _, name := range []string{"metadata", "config", "credential", "hook"} {
		t.Run(name, func(t *testing.T) {
			changed := make(map[string]controlReadyFileInput, len(files))
			for k, v := range files {
				changed[k] = v
			}
			entry := changed[name]
			entry.Content = []byte("changed input")
			changed[name] = entry
			if original == controlReadyOpeningFingerprint(env, config, changed) {
				t.Fatal("file content change ignored")
			}
		})
	}
	changed := make(map[string]controlReadyFileInput, len(files))
	for k, v := range files {
		changed[k] = v
	}
	hook := changed["hook"]
	hook.Mode = 0o755
	changed["hook"] = hook
	if original == controlReadyOpeningFingerprint(env, config, changed) {
		t.Fatal("hook became executable without invalidation")
	}
}

func TestControlReadyFingerprintMissingIsNotEmptyFile(t *testing.T) {
	missing := map[string]controlReadyFileInput{"config": {}}
	empty := map[string]controlReadyFileInput{"config": {Exists: true, Mode: 0o600}}
	if controlReadyOpeningFingerprint(nil, nil, missing) == controlReadyOpeningFingerprint(nil, nil, empty) {
		t.Fatal("created empty input did not invalidate reader")
	}
}
