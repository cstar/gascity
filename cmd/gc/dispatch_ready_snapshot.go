package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// controlReadyOpeningSnapshot couples selected opening inputs to their hash.
// Env contains credentials and must never be logged.
type controlReadyOpeningSnapshot struct {
	Env            map[string]string
	Fingerprint    string
	FallbackReason string
}

// controlReadyCaptureOpening covers direct, explicitly scoped SDK openings.
// CLI-only discovery modes are identified for fallback, never approximated.
func controlReadyCaptureOpening(f fsys.FS, scope string, environ []string, configuration []byte) (*controlReadyOpeningSnapshot, error) {
	root, err := filepath.Abs(scope)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(environ)+4)
	for _, item := range environ {
		if key, value, ok := strings.Cut(item, "="); ok {
			env[key] = value
		}
	}
	absolute := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(root, path)
	}
	fallback := ""
	dir := env["BEADS_DIR"]
	if dir == "" {
		fallback = "implicit CLI discovery"
		dir = filepath.Join(root, ".beads")
	}
	dir = absolute(dir)
	if dir != filepath.Join(root, ".beads") {
		fallback = "ledger outside preflight scope"
	}
	env["BEADS_DIR"] = dir
	env["BEADS_DOLT_MAX_CONNS"] = "1"
	env["BEADS_DOLT_AUTO_START"] = "0"
	if strings.TrimSpace(env["BEADS_DOLT_CREDENTIAL_COMMAND"]) != "" {
		fallback = "credential helper requires CLI subprocess context"
	}
	paths := []string{}
	for _, name := range []string{"metadata.json", "config.json", "config.yaml", "config.local.yaml", "dolt-server.port", "redirect", "hooks/on_create", "hooks/on_update", "hooks/on_close"} {
		paths = append(paths, filepath.Join(dir, filepath.FromSlash(name)))
	}
	for ancestor := root; ; ancestor = filepath.Dir(ancestor) {
		paths = append(paths, filepath.Join(ancestor, ".beads/config.yaml"), filepath.Join(ancestor, ".beads/config.local.yaml"))
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	// Worktrees and symlinked ledger roots use discovery rules outside the
	// direct scope contract; leave those reads with the existing CLI.
	for _, path := range []string{dir, filepath.Join(root, ".git")} {
		info, statErr := f.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("stat readiness discovery input: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || (path != dir && !info.IsDir()) {
			fallback = "worktree or symlinked CLI discovery"
		}
		if path != dir && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			paths = append(paths, path)
		}
	}
	// Lstat the complete scope chain: checking only .beads misses a
	// symlinked parent whose retargeting changes the ledger underneath us.
	for ancestor := filepath.Dir(dir); ; ancestor = filepath.Dir(ancestor) {
		info, err := f.Lstat(ancestor)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat readiness scope ancestor: %w", err)
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			fallback = "symlinked CLI scope ancestor"
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	files, err := controlReadyCollectFiles(f, paths)
	if err != nil {
		return nil, err
	}
	redirect := files[filepath.Join(dir, "redirect")]
	if redirect.Exists && len(strings.TrimSpace(string(redirect.Content))) > 0 {
		fallback = "CLI redirect"
	}
	var metadata struct {
		DoltDataDir string `json:"dolt_data_dir"`
		Database    string `json:"database"`
	}
	if input := files[filepath.Join(dir, "metadata.json")]; input.Exists {
		if err := json.Unmarshal(input.Content, &metadata); err != nil {
			return nil, fmt.Errorf("parse readiness metadata: %w", err)
		}
	}
	dataDir := env["BEADS_DOLT_DATA_DIR"]
	if dataDir == "" {
		dataDir = metadata.DoltDataDir
	}
	if dataDir == "" && filepath.IsAbs(metadata.Database) {
		dataDir = metadata.Database
	}
	if dataDir == "" {
		dataDir = "dolt"
	}
	if !filepath.IsAbs(dataDir) {
		dataDir = filepath.Join(dir, dataDir)
	}
	paths = append(paths, filepath.Join(dataDir, "config.yaml"))
	if env["BEADS_DOLT_DATA_DIR"] != "" {
		env["BEADS_DOLT_DATA_DIR"] = dataDir
	}
	home := env["HOME"]
	if runtime.GOOS == "windows" {
		home = env["USERPROFILE"]
	}
	if home == "" {
		fallback = "unresolved SDK home defaults"
	} else {
		paths = append(paths, filepath.Join(home, ".beads/config.yaml"), filepath.Join(home, ".config/bd/config.yaml"))
		nativeDir := env["XDG_CONFIG_HOME"]
		switch runtime.GOOS {
		case "darwin":
			nativeDir = filepath.Join(home, "Library/Application Support")
		case "windows":
			nativeDir = env["APPDATA"]
		default:
			if nativeDir == "" {
				nativeDir = filepath.Join(home, ".config")
			}
		}
		if nativeDir != "" && filepath.IsAbs(nativeDir) {
			paths = append(paths, filepath.Join(nativeDir, "bd/config.yaml"))
		}
		sdkDir := filepath.Join(home, ".config/beads")
		if runtime.GOOS == "windows" && env["APPDATA"] != "" {
			sdkDir = filepath.Join(env["APPDATA"], "beads")
		}
		for key, name := range map[string]string{"BEADS_CENTRAL_CONFIG": "server.json", "BEADS_CREDENTIALS_FILE": "credentials"} {
			path := env[key]
			if path == "" {
				path = filepath.Join(sdkDir, name)
			}
			env[key] = absolute(path)
			paths = append(paths, env[key])
		}
		sharedDir := env["BEADS_SHARED_SERVER_DIR"]
		if sharedDir == "" {
			sharedDir = filepath.Join(home, ".beads/shared-server")
		}
		env["BEADS_SHARED_SERVER_DIR"] = absolute(sharedDir)
		paths = append(paths, filepath.Join(env["BEADS_SHARED_SERVER_DIR"], "dolt-server.port"), filepath.Join(env["BEADS_SHARED_SERVER_DIR"], "dolt/config.yaml"))
	}
	files, err = controlReadyCollectFiles(f, paths)
	if err != nil {
		return nil, err
	}
	// The CLI initializes YAML configuration whereas the SDK opener consults
	// process-global config state. Do not guess shared-server authority, and
	// do not call its directory-creating shared-server resolvers here.
	if value := env["BEADS_DOLT_SHARED_SERVER"]; value == "1" || strings.EqualFold(value, "true") {
		fallback = "shared-server requires CLI configuration resolution"
	}
	for path, input := range files {
		if !input.Exists || (filepath.Base(path) != "config.yaml" && filepath.Base(path) != "config.local.yaml") {
			continue
		}
		var values map[string]any
		if err := yaml.Unmarshal(input.Content, &values); err != nil {
			return nil, fmt.Errorf("parse readiness YAML input: %w", err)
		}
		shared := values["dolt.shared-server"]
		if dolt, ok := values["dolt"].(map[string]any); ok {
			if value, exists := dolt["shared-server"]; exists {
				shared = value
			}
		}
		if shared != nil && !strings.EqualFold(fmt.Sprint(shared), "false") && fmt.Sprint(shared) != "0" {
			fallback = "shared-server requires CLI configuration resolution"
		}
	}
	return &controlReadyOpeningSnapshot{Env: env, Fingerprint: controlReadyOpeningFingerprint(env, append(append([]byte(nil), configuration...), []byte("\x00"+fallback)...), files), FallbackReason: fallback}, nil
}
