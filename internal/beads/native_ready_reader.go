package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	beadslib "github.com/steveyegge/beads"
)

var nativeReadyOpen = beadslib.OpenReadyReader

// ErrNativeReadyCredentialCommand means a CLI credential helper needs its
// original subprocess context; callers must keep the bd fallback for this mode.
var ErrNativeReadyCredentialCommand = errors.New("native readiness cannot reproduce credential helper subprocess context")

// NativeReadyReader owns an SDK read-only capability for the raw bd frontier.
// It deliberately does not implement Store or expose writable storage.
type NativeReadyReader struct{ reader beadslib.ReadyReader }

// OpenNativeReadyReader opens existing storage without provisioning, migration
// or auto-start. The SDK environment is projected under the native-open mutex;
// unselected BEADS_* keys cannot leak in from another scope.
func OpenNativeReadyReader(ctx context.Context, beadsDir string, env map[string]string) (*NativeReadyReader, error) {
	if strings.TrimSpace(env["BEADS_DOLT_CREDENTIAL_COMMAND"]) != "" {
		return nil, ErrNativeReadyCredentialCommand
	}
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()
	restore, err := withWithheldBeadsEnvLocked()
	if err != nil {
		return nil, err
	}
	defer restore()
	selected := make(map[string]string, len(env)+2)
	for key, value := range env {
		if strings.HasPrefix(key, beadsEnvPrefix) {
			selected[key] = value
		}
	}
	selected["BEADS_DOLT_AUTO_START"] = "0"
	selected["BEADS_DOLT_MAX_CONNS"] = "1"
	// The withheld namespace is restored after removing every projected key,
	// including keys that were absent from the original process environment.
	defer func() {
		for key := range selected {
			_ = os.Unsetenv(key)
		}
	}()
	for key, value := range selected {
		if err := os.Setenv(key, value); err != nil {
			return nil, fmt.Errorf("projecting readiness SDK environment %s: %w", key, err)
		}
	}
	reader, err := nativeReadyOpen(ctx, beadsDir)
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("SDK readiness opener returned no reader")
	}
	return &NativeReadyReader{reader: reader}, nil
}

// Ready reads the current raw frontier with the explicit bd ready CLI filter.
// The caller owns control-graph federation and route/assignee filtering.
func (r *NativeReadyReader) Ready(ctx context.Context, includeEphemeral bool, limit int) ([]Bead, error) {
	issues, err := r.reader.GetReadyWork(ctx, beadslib.WorkFilter{Status: beadslib.StatusOpen, SortPolicy: beadslib.SortPolicyPriority, Limit: limit, IncludeEphemeral: includeEphemeral, ExcludeTypes: []beadslib.IssueType{beadslib.TypeEpic}})
	if err != nil {
		return nil, err
	}
	rows := make([]Bead, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			return nil, fmt.Errorf("SDK readiness returned a nil issue")
		}
		row, err := beadFromNativeIssue(issue)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	SortBeadsReadyOrder(rows)
	return rows, nil
}

// Close releases the reader's owned connections. Its owner must close it once.
func (r *NativeReadyReader) Close() error { return r.reader.Close() }
