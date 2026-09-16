package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// LoadWithIncludesOptions loads a city.toml with its includes and packs, memoized
// per process (fork feat/main-storage-split-fork, 2026-09-16).
//
// Why: every native store open runs the beads preflight, which rebuilds the bd
// runtime environment, which reloads the whole city config — toml parse, pack
// expansion and the content hash of every cached pack tree. On a 14-rig city
// with a 2,300-line city.toml and a 143 MB pack clone that is ~4 s per store,
// so a federated `gc ready` cost 90-130 s and agent hooks timed out.
//
// The memo is keyed by path + options and re-validated on every hit against a
// stamp (existence, size, mtime) of every file the load depends on: the sources
// it read (city.toml, includes, pack.toml, packs.lock) and every file under the
// config watch targets (pack roots and convention directories — the same set
// the supervisor watches for config changes), so a later load sees pack content
// edits exactly as an uncached load would, and Provenance.Revision() stays
// faithful. Loads that carry internal options (deferred rig patches), extra
// includes, or a non-OS filesystem go straight through. GC_CITY_CONFIG_MEMO=0
// disables it, and DisableLoadMemo turns it off for the rest of the process.
func LoadWithIncludesOptions(fs fsys.FS, path string, opts LoadOptions, extraIncludes ...string) (*City, *Provenance, error) {
	if _, ok := fs.(fsys.OSFS); !ok || loadMemoDisabled.Load() || len(extraIncludes) > 0 || opts.deferRigPatches || opts.deferredRigPatches != nil || os.Getenv("GC_CITY_CONFIG_MEMO") == "0" {
		return loadWithIncludesOptionsUncached(fs, path, opts, extraIncludes...)
	}
	key := fmt.Sprintf("%s|%t|%t|%t|%t|%t", path, opts.SuppressDeprecatedOrderWarnings, opts.AllowMissingProviderReferences,
		opts.SkipRevisionSnapshot, opts.RepoCacheNonBlocking, opts.allowLegacyOrderLayouts)
	if v, ok := loadMemo.Load(key); ok {
		e := v.(*loadMemoEntry)
		if loadMemoStampsEqual(e.stamps, loadMemoStampAll(e.files, e.trees)) {
			return e.cfg, e.prov, nil
		}
		loadMemo.Delete(key)
	}
	cfg, prov, err := loadWithIncludesOptionsUncached(fs, path, opts)
	if err != nil {
		return nil, nil, err
	}
	root := filepath.Dir(path)
	files := []string{path, filepath.Join(root, "pack.toml"), filepath.Join(root, "packs.lock")}
	var trees []loadMemoTree
	if prov != nil {
		files = append(files, prov.Sources...)
		for _, t := range WatchTargets(prov, cfg, root) {
			trees = append(trees, loadMemoTree{path: t.Path, recursive: t.Recursive})
		}
	}
	loadMemo.Store(key, &loadMemoEntry{cfg: cfg, prov: prov, files: files, trees: trees, stamps: loadMemoStampAll(files, trees)})
	return cfg, prov, nil
}

type loadMemoEntry struct {
	cfg    *City
	prov   *Provenance
	files  []string
	trees  []loadMemoTree
	stamps map[string]loadMemoStamp
}

type loadMemoTree struct {
	path      string
	recursive bool
}

type loadMemoStamp struct {
	exists bool
	size   int64
	mtime  time.Time
}

var (
	loadMemo         sync.Map // string -> *loadMemoEntry
	loadMemoDisabled atomic.Bool
)

// DisableLoadMemo turns the per-process config memo off for the rest of the
// process. The supervisor calls it: it is the one long-lived gc process, and it
// has its own change detection; the memo exists for short-lived invocations.
func DisableLoadMemo() { loadMemoDisabled.Store(true) }

func loadMemoStampOf(path string) loadMemoStamp {
	st, err := os.Stat(path)
	if err != nil {
		return loadMemoStamp{}
	}
	return loadMemoStamp{exists: true, size: st.Size(), mtime: st.ModTime()}
}

// loadMemoStampAll stamps the named files plus every regular file under the
// watch trees (recursively where the target is recursive, one level otherwise),
// skipping .git directories. Missing paths stamp as absent, so a file appearing
// later invalidates the entry as much as one changing.
func loadMemoStampAll(files []string, trees []loadMemoTree) map[string]loadMemoStamp {
	stamps := make(map[string]loadMemoStamp, len(files)+256)
	for _, f := range files {
		stamps[f] = loadMemoStampOf(f)
	}
	for _, t := range trees {
		if t.recursive {
			_ = filepath.WalkDir(t.path, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if d.Name() == ".git" && p != t.path {
						return filepath.SkipDir
					}
					return nil
				}
				stamps[p] = loadMemoStampOf(p)
				return nil
			})
			continue
		}
		entries, err := os.ReadDir(t.path)
		if err != nil {
			stamps[t.path] = loadMemoStampOf(t.path)
			continue
		}
		for _, d := range entries {
			p := filepath.Join(t.path, d.Name())
			stamps[p] = loadMemoStampOf(p)
		}
	}
	return stamps
}

func loadMemoStampsEqual(a, b map[string]loadMemoStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
