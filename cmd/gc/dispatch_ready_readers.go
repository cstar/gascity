package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// controlReadyReader reads a live frontier without exposing write capabilities.
type controlReadyReader struct {
	Ready func(context.Context, bool) ([]beads.Bead, error)
	Close func() error
}

// controlReadyReaders owns handles for a serve invocation, never query results.
// The caller supplies the resolved file/configuration fingerprint; the registry
// also binds it to the exact environment used to open the handle.
type controlReadyReaders struct {
	mu      sync.Mutex
	open    func(context.Context, string, map[string]string) (*controlReadyReader, bool, error)
	entries map[string]controlReadyReaderEntry
	closed  bool
}
type controlReadyReaderEntry struct {
	fingerprint string
	reader      *controlReadyReader
}

func (r *controlReadyReaders) ready(ctx context.Context, root, fingerprint string, env map[string]string, includeEphemeral bool) ([]beads.Bead, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, true, beads.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	if r.entries == nil {
		r.entries = make(map[string]controlReadyReaderEntry)
	}
	fingerprint = controlReadyOpeningFingerprint(env, []byte(fingerprint), nil)
	entry, exists := r.entries[root]
	if exists && entry.fingerprint != fingerprint {
		delete(r.entries, root)
		if err := closeControlReadyReader(entry.reader); err != nil {
			return nil, true, err
		}
		exists = false
	}
	if !exists {
		reader, eligible, err := r.open(ctx, root, env)
		if err != nil || !eligible {
			err = errors.Join(err, closeControlReadyReader(reader))
			if err == nil {
				r.entries[root] = controlReadyReaderEntry{fingerprint: fingerprint}
			}
			return nil, err != nil, err
		}
		if reader == nil || reader.Ready == nil || reader.Close == nil {
			return nil, true, errors.Join(fmt.Errorf("eligible readiness reader is incomplete"), closeControlReadyReader(reader))
		}
		entry = controlReadyReaderEntry{fingerprint: fingerprint, reader: reader}
		r.entries[root] = entry
	}
	if entry.reader == nil {
		return nil, false, nil
	}
	rows, err := entry.reader.Ready(ctx, includeEphemeral)
	if err != nil {
		return nil, true, err
	}
	return rows, true, nil
}

func closeControlReadyReader(reader *controlReadyReader) error {
	if reader == nil {
		return nil
	}
	if reader.Close == nil {
		return fmt.Errorf("readiness reader has no close function")
	}
	return reader.Close()
}

func (r *controlReadyReaders) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for _, entry := range r.entries {
		errs = append(errs, closeControlReadyReader(entry.reader))
	}
	r.entries = nil
	return errors.Join(errs...)
}
