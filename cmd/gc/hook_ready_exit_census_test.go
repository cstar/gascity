package main

import (
	"strings"
	"testing"
)

func TestHookReaderExitCensusPinsAutonomousLease(t *testing.T) {
	const lease = `time.AfterFunc(hookWorkQueryTimeout, func() { _ = os.Remove(endpoint); _ = os.Remove(filepath.Dir(endpoint)); os.Exit(124) })`
	tests := map[string]struct {
		expression string
		valid      bool
	}{
		"bounded reader lease": {lease, true},
		"different deadline":   {strings.Replace(lease, "hookWorkQueryTimeout", "time.Hour", 1), false},
		"different exit code":  {strings.Replace(lease, "Exit(124)", "Exit(0)", 1), false},
		"recursive cleanup":    {strings.Replace(lease, "Remove(filepath.Dir", "RemoveAll(filepath.Dir", 1), false},
		"no socket cleanup":    {strings.Replace(lease, "_ = os.Remove(endpoint);", "", 1), false},
		"direct exit":          {"os.Exit(124)", false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "hook_ready_reader.go", `package main
import ("os"; "time"; "path/filepath")
func serveNativeHookReadyEndpoint() { lease := `+test.expression+`; _ = lease }
`)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 0 || len(sites) != 1 {
				t.Fatalf("scan: sites=%d violations=%v", len(sites), violations)
			}
			validate := allowedGCExitBypassSites[sites[0].key()]
			if validate == nil {
				t.Fatal("reader lease lacks a constrained census validator")
			}
			if err := validate(sites[0]); (err == nil) != test.valid {
				t.Fatalf("validation error=%v, want valid=%v", err, test.valid)
			}
		})
	}
}
