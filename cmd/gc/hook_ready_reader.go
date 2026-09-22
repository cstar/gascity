package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hookReadyReader lives for one native hook discovery only. Each status is
// read on demand once, preserving the actual serving leg for dependency reads.
// Filtering, ordering, bounding and enrichment remain the ordinary gc ready
// implementation. In particular a ready read never runs before recovery needs
// it, and unrelated owners never incur dependency enrichment.
type hookReadyReader struct {
	read       func(string) ([]beads.Bead, map[string]readyLeg, error)
	candidates map[string]hookReadyCandidates
}

type hookReadyCandidates struct {
	rows   []beads.Bead
	owners map[string]readyLeg
}

func newHookReadyReader(read func(string) ([]beads.Bead, map[string]readyLeg, error)) *hookReadyReader {
	return &hookReadyReader{read: read, candidates: make(map[string]hookReadyCandidates)}
}

func (r *hookReadyReader) query(opts readyOpts) ([]readyBead, error) {
	return readyBeadsUsingReader(opts, func(status string) ([]beads.Bead, map[string]readyLeg, error) {
		if cached, ok := r.candidates[status]; ok {
			return cached.rows, cached.owners, nil
		}
		rows, owners, err := r.read(status)
		if err != nil {
			return nil, nil, err
		}
		r.candidates[status] = hookReadyCandidates{rows: rows, owners: owners}
		return rows, owners, nil
	})
}

// prepareNativeHookReadyQuery adapts only the SDK-generated federated query.
// The reader is a child of the discovery shell, in that shell's process group:
// its backend reads cannot outlive the shell's timeout or race parent cleanup.
func prepareNativeHookReadyQuery(command, cityPath string, a *config.Agent, topo config.QueryTopology) (string, func(), error) {
	noop := func() {}
	if !topo.FederatedReady || strings.TrimSpace(a.WorkQuery) != "" {
		return command, noop, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", noop, err
	}
	dir, err := os.MkdirTemp("", "gcr-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	endpoint := filepath.Join(dir, "reader.sock")
	adapted, err := hookQueryWithReadyReader(command, endpoint, executable, cityPath)
	if err != nil {
		cleanup()
		return "", noop, err
	}
	return adapted, cleanup, nil
}

func serveNativeHookReadyEndpoint(endpoint string, stderr io.Writer) error {
	// This command runs only in the dedicated reader child. A parent hook can
	// disappear without canceling its separate discovery process group. The
	// lease therefore terminates this process even if a backend call never
	// returns; normal CLI cleanup could itself wait on that backend.
	lease := time.AfterFunc(hookWorkQueryTimeout, func() {
		_ = os.Remove(endpoint)
		// Remove only an empty directory; never recursively delete a path supplied
		// through the private CLI flag if the launching hook has disappeared.
		_ = os.Remove(filepath.Dir(endpoint))
		os.Exit(124)
	})
	defer lease.Stop()
	var legs []readyLeg
	reader := newHookReadyReader(func(status string) ([]beads.Bead, map[string]readyLeg, error) {
		if legs == nil {
			cityPath, err := resolveCity()
			if err != nil {
				return nil, nil, err
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				return nil, nil, err
			}
			cityStore, err := openCityStoreAt(cityPath)
			if err != nil {
				return nil, nil, err
			}
			rigStores, err := readyRigLegStores(cfg, cityPath)
			if err != nil {
				return nil, nil, err
			}
			legs, err = readyFederationLegs(cityPath, loadedCityName(cfg, cityPath), cfg, cityStore, rigStores)
			if err != nil {
				return nil, nil, err
			}
		}
		return readReadyCandidates(legs, status)
	})
	cleanup, done, err := startHookReadyServerAt(endpoint, reader)
	if err != nil {
		return err
	}
	defer cleanup()
	<-done
	return nil
}
