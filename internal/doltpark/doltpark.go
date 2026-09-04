// Package doltpark moves a rig's Dolt database out of the managed server's
// data directory while the rig is suspended, and back when it resumes.
//
// The managed Dolt server serves every subdirectory of its data directory.
// A suspended rig's database stays open on the server, is enumerated by every
// INFORMATION_SCHEMA probe bd runs on a cold connection, and is exported and
// pushed by the fleet-wide maintenance orders, so a city with many dormant rigs
// pays for them on every call. Parking = a same-volume rename into a sibling
// directory (DataDir + "-suspended"); the server drops the database on its
// next access and picks it up again after the reverse rename, no restart
// needed. Nothing is ever copied or deleted.
package doltpark

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvParkOnSuspend disables parking when set to "0", "false", "no" or "off".
// It is an environment switch rather than a CLI flag because `gc rig suspend`
// normally reaches the supervisor over the API, where a flag would not travel.
const EnvParkOnSuspend = "GC_DOLT_PARK_ON_SUSPEND"

// EnvDataDir mirrors the managed-server data-directory override.
const EnvDataDir = "GC_DOLT_DATA_DIR"

// ParkedDirSuffix is appended to the data directory name to form the parked
// directory: .beads/dolt -> .beads/dolt-suspended.
const ParkedDirSuffix = "-suspended"

// Layout locates the served and parked database roots.
type Layout struct {
	DataDir   string
	ParkedDir string
}

// DefaultLayout resolves the layout for a city, honoring GC_DOLT_DATA_DIR the
// way the managed server does.
func DefaultLayout(cityPath string) Layout {
	dataDir := strings.TrimSpace(os.Getenv(EnvDataDir))
	if dataDir == "" {
		dataDir = filepath.Join(cityPath, ".beads", "dolt")
	}
	dataDir = filepath.Clean(dataDir)
	return Layout{DataDir: dataDir, ParkedDir: dataDir + ParkedDirSuffix}
}

// Enabled reports whether parking is switched on (default true).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvParkOnSuspend))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// DatabaseName returns the Dolt database a rig's beads scope lives in: the
// dolt_database recorded in <rigPath>/.beads/metadata.json when present,
// otherwise the rig prefix. An empty result means the rig has no resolvable
// database and must be skipped.
func DatabaseName(rigPath, prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if rigPath == "" {
		return prefix
	}
	data, err := os.ReadFile(filepath.Join(rigPath, ".beads", "metadata.json"))
	if err != nil {
		return prefix
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return prefix
	}
	if db := strings.TrimSpace(meta.DoltDatabase); db != "" {
		return db
	}
	return prefix
}

// reservedDatabases can never be parked: the HQ store backs the city itself
// and the rest are Dolt/MySQL system schemas.
var reservedDatabases = map[string]struct{}{
	"hq":                 {},
	"information_schema": {},
	"mysql":              {},
	"dolt":               {},
	"dolt_cluster":       {},
	"performance_schema": {},
	"sys":                {},
}

// Reserved reports whether db must never be moved.
func Reserved(db string) bool {
	_, ok := reservedDatabases[strings.ToLower(strings.TrimSpace(db))]
	return ok
}

// Status is where a database currently lives.
type Status int

const (
	// Missing means the database exists in neither location.
	Missing Status = iota
	// Served means the database is under DataDir (visible to the server).
	Served
	// Parked means the database is under ParkedDir.
	Parked
	// Conflict means a directory exists in both locations; nothing is moved
	// until an operator resolves it.
	Conflict
)

func (s Status) String() string {
	switch s {
	case Missing:
		return "missing"
	case Served:
		return "served"
	case Parked:
		return "parked"
	case Conflict:
		return "conflict"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// ErrInvalidDatabase rejects names that are empty, reserved, or not a single
// path segment.
var ErrInvalidDatabase = errors.New("doltpark: invalid database name")

// ErrConflict is returned when the database exists in both locations.
var ErrConflict = errors.New("doltpark: database present in both served and parked directories")

func validate(db string) error {
	db = strings.TrimSpace(db)
	if db == "" || Reserved(db) || db != filepath.Base(db) || strings.HasPrefix(db, ".") {
		return fmt.Errorf("%w: %q", ErrInvalidDatabase, db)
	}
	return nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Inspect reports where db lives under l.
func Inspect(l Layout, db string) (Status, error) {
	if err := validate(db); err != nil {
		return Missing, err
	}
	served := isDir(filepath.Join(l.DataDir, db))
	parked := isDir(filepath.Join(l.ParkedDir, db))
	switch {
	case served && parked:
		return Conflict, nil
	case served:
		return Served, nil
	case parked:
		return Parked, nil
	}
	return Missing, nil
}

// Park moves db from DataDir to ParkedDir. It reports whether a move happened;
// an already-parked or missing database is a no-op, a Conflict is an error.
func Park(l Layout, db string) (bool, error) {
	return move(l, db, Served, l.DataDir, l.ParkedDir)
}

// Unpark moves db from ParkedDir back to DataDir. It reports whether a move
// happened; an already-served or missing database is a no-op, a Conflict is
// an error.
func Unpark(l Layout, db string) (bool, error) {
	return move(l, db, Parked, l.ParkedDir, l.DataDir)
}

func move(l Layout, db string, want Status, from, to string) (bool, error) {
	st, err := Inspect(l, db)
	if err != nil {
		return false, err
	}
	switch st {
	case Conflict:
		return false, fmt.Errorf("%w: %q", ErrConflict, db)
	case want:
	default:
		return false, nil
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return false, fmt.Errorf("doltpark: creating %s: %w", to, err)
	}
	if err := os.Rename(filepath.Join(from, db), filepath.Join(to, db)); err != nil {
		return false, fmt.Errorf("doltpark: moving %q: %w", db, err)
	}
	return true, nil
}
