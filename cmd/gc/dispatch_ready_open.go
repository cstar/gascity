package main

import (
	"context"
	"errors"
	"fmt"
)

// controlReadyOpenVerified never publishes a handle opened across an input
// change. The caller must recapture both resolved configuration and files.
func controlReadyOpenVerified(ctx context.Context, before *controlReadyOpeningSnapshot,
	open func(context.Context, map[string]string) (*controlReadyReader, bool, error),
	capture func() (*controlReadyOpeningSnapshot, error),
) (*controlReadyReader, bool, error) {
	reader, eligible, err := open(ctx, before.Env)
	if err != nil || !eligible {
		return nil, false, errors.Join(err, closeControlReadyReader(reader))
	}
	after, err := capture()
	if err == nil && (after == nil || before.Fingerprint != after.Fingerprint) {
		err = fmt.Errorf("readiness opening inputs changed during open")
	}
	if err != nil {
		return nil, false, errors.Join(err, closeControlReadyReader(reader))
	}
	return reader, true, nil
}
