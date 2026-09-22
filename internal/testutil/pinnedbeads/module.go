// Package pinnedbeads preserves the exact Beads source identity in test builds.
package pinnedbeads

import (
	"fmt"
	"runtime/debug"

	"golang.org/x/mod/module"
)

// Path is the module identity declared by upstream and the approved fork.
const Path = "github.com/steveyegge/beads"

// FromBuildInfo finds Beads as either the built command or an imported
// dependency, preserving its original identity and complete replacement.
func FromBuildInfo(info *debug.BuildInfo) (debug.Module, error) {
	if info.Main.Path == Path {
		return info.Main, nil
	}
	for _, dep := range info.Deps {
		if dep.Path == Path {
			return *dep, nil
		}
	}
	return debug.Module{}, fmt.Errorf("%s not found in build info", Path)
}

// Current returns the original requirement and any resolved replacement.
func Current() (debug.Module, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return debug.Module{}, fmt.Errorf("build info unavailable")
	}
	return FromBuildInfo(info)
}

// Manifest builds an isolated module for the CLI's wider dependency closure.
// A replacement retains both identities; a fork commit must never be fetched
// from the original upstream repository. Local replacements are not reproducible.
func Manifest(dep debug.Module) (string, error) {
	if dep.Path != Path {
		return "", fmt.Errorf("unexpected Beads module %q", dep.Path)
	}
	if err := module.Check(dep.Path, dep.Version); err != nil {
		return "", err
	}
	manifest := fmt.Sprintf("module gascity.test/pinned-bd\n\ngo 1.26.0\n\nrequire %s %s\n", dep.Path, dep.Version)
	if replacement := dep.Replace; replacement != nil {
		if err := module.Check(replacement.Path, replacement.Version); err != nil {
			return "", fmt.Errorf("unversioned or invalid Beads replacement: %w", err)
		}
		manifest += fmt.Sprintf("replace %s => %s %s\n", dep.Path, replacement.Path, replacement.Version)
	}
	return manifest, nil
}
