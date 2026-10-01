package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionBackendProjectionSharesOnlySuccessfulScopeResults(t *testing.T) {
	calls := map[string]int{}
	p := sessionBackendProjection{resolve: func(scope string) (map[string]string, error) {
		calls[scope]++
		if scope == "retry" && calls[scope] == 1 {
			return nil, errors.New("unavailable")
		}
		return map[string]string{"scope": scope, "GC_DOLT_PORT": "3307"}, nil
	}}
	first, err := p.project("rig-a")
	if err != nil {
		t.Fatal(err)
	}
	first["scope"] = "mutated"
	for i := 0; i < 20; i++ {
		got, err := p.project("rig-a")
		if err != nil || got["scope"] != "rig-a" {
			t.Fatalf("projection: %v %v", got, err)
		}
	}
	if calls["rig-a"] != 1 {
		t.Fatalf("same build made %d resolutions, want 1", calls["rig-a"])
	}
	if got, err := p.project("rig-b"); err != nil || got["scope"] != "rig-b" {
		t.Fatalf("scope isolation: %v %v", got, err)
	}
	if _, err := p.project("retry"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := p.project("retry"); err != nil {
		t.Fatal(err)
	}
	if calls["retry"] != 2 {
		t.Fatal("failed resolution must be retried")
	}
	next := sessionBackendProjection{resolve: p.resolve}
	if _, err := next.project("rig-a"); err != nil {
		t.Fatal(err)
	}
	if calls["rig-a"] != 2 {
		t.Fatal("new build must revalidate")
	}
}

func TestSessionBackendProjectionBuildRevalidatesChangedTarget(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTarget := func(port int) {
		t.Helper()
		body := fmt.Sprintf("issue_prefix: test\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.auto-start: false\ndolt.host: canonical-db.example.com\ndolt.port: %d\n", port)
		if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	makeBuild := func() *agentBuildParams {
		return newAgentBuildParams("test", city, cfg, runtime.NewFake(), time.Now(), nil, io.Discard)
	}
	writeTarget(3307)
	bp := makeBuild()
	calls := 0
	original := bp.backendProjection.resolve
	bp.backendProjection.resolve = func(scope string) (map[string]string, error) { calls++; return original(scope) }
	local := *bp
	for i := 0; i < 10; i++ {
		env, err := local.sessionBackendEnv("")
		if err != nil {
			t.Fatal(err)
		}
		if env["GC_DOLT_PORT"] != "3307" {
			t.Fatalf("wrong target: %q", env["GC_DOLT_PORT"])
		}
	}
	if calls != 1 {
		t.Fatalf("per-session copies made %d resolutions, want1", calls)
	}
	writeTarget(4406)
	env, err := makeBuild().sessionBackendEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if env["GC_DOLT_PORT"] != "4406" {
		t.Fatalf("new build retained stale port %q", env["GC_DOLT_PORT"])
	}
}

func TestSessionBackendProjectionRetriesIncompleteDoltTarget(t *testing.T) {
	calls := 0
	p := sessionBackendProjection{resolve: func(string) (map[string]string, error) {
		calls++
		if calls == 1 {
			return map[string]string{"GC_DOLT_PORT": ""}, nil
		}
		return map[string]string{"GC_DOLT_PORT": "3307"}, nil
	}}
	if _, err := p.project(""); err != nil {
		t.Fatal(err)
	}
	env, err := p.project("")
	if err != nil {
		t.Fatal(err)
	}
	if env["GC_DOLT_PORT"] != "3307" || calls != 2 {
		t.Fatalf("incomplete target retained: port=%q calls=%d", env["GC_DOLT_PORT"], calls)
	}
}
