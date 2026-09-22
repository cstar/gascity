package pinnedbeads

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestManifestPreservesResolvedModule(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  debug.Module
		want string
		bad  bool
	}{
		{name: "upstream", dep: debug.Module{Path: Path, Version: "v1.3.0-rc.2"}, want: "require " + Path + " v1.3.0-rc.2\n"},
		{name: "fork", dep: debug.Module{Path: Path, Version: "v1.3.0-rc.2", Replace: &debug.Module{Path: "github.com/cstar/beads", Version: "v1.0.6-0.20260917105420-f02fff7bb5e1"}}, want: "replace " + Path + " => github.com/cstar/beads v1.0.6-0.20260917105420-f02fff7bb5e1\n"},
		{name: "local replacement", dep: debug.Module{Path: Path, Version: "v1.3.0-rc.2", Replace: &debug.Module{Path: "../beads"}}, bad: true},
		{name: "missing version", dep: debug.Module{Path: Path}, bad: true},
		{name: "wrong module", dep: debug.Module{Path: "example.com/other", Version: "v1.0.0"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Manifest(tc.dep)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted invalid dependency: %+v", tc.dep)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("manifest %q does not contain %q", got, tc.want)
			}
			if tc.dep.Replace != nil && !strings.Contains(got, "require "+Path+" "+tc.dep.Version+"\n") {
				t.Fatalf("lost original module identity: %s", got)
			}
		})
	}
}

func TestModuleFromBuildInfo(t *testing.T) {
	dep := &debug.Module{Path: Path, Version: "v1.3.0-rc.2", Replace: &debug.Module{Path: "github.com/cstar/beads", Version: "v1.0.6-0.20260917105420-f02fff7bb5e1"}}
	for _, bi := range []*debug.BuildInfo{{Deps: []*debug.Module{dep}}, {Main: *dep}} {
		got, err := FromBuildInfo(bi)
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != dep.Path || got.Version != dep.Version || got.Replace != dep.Replace {
			t.Fatalf("lost module identity: %+v", got)
		}
	}
	if _, err := FromBuildInfo(&debug.BuildInfo{}); err == nil {
		t.Fatal("missing dependency accepted")
	}
}
