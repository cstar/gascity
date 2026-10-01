package main

import (
	"maps"
	"sync"
)

// sessionBackendProjection belongs to one desired-state build only. Successful
// per-scope Dolt projections share validation; later builds revalidate.
// Callers receive independent maps and failed resolutions are never cached.
type sessionBackendProjection struct {
	mu      sync.Mutex
	resolve func(string) (map[string]string, error)
	values  map[string]map[string]string
}

func (p *sessionBackendProjection) project(scope string) (map[string]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if env, ok := p.values[scope]; ok {
		return maps.Clone(env), nil
	}
	env, err := p.resolve(scope)
	if err != nil {
		return env, err
	}
	// Recoverable managed-runtime errors may be represented as an empty
	// endpoint without an error. Retry these projections within the build.
	if env["GC_DOLT_PORT"] == "" {
		return env, nil
	}
	if p.values == nil {
		p.values = make(map[string]map[string]string)
	}
	p.values[scope] = maps.Clone(env)
	return env, nil
}
