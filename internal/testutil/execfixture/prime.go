// Package execfixture prepares process-backed test fixtures before timing assertions.
package execfixture

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// PrimeExecutable waits for the host to admit a newly written test executable
// before a test starts a sub-second behavioral deadline. On macOS the first
// execution can spend hundreds of milliseconds in OS admission. The fixture
// must handle --gc-test-ready before logging, spawning children, or mutating
// state. This is setup, not a retry of the operation under test.
func PrimeExecutable(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--gc-test-ready")
	cmd.WaitDelay = time.Second
	configurePrimeCommand(cmd)
	out, err := cmd.CombinedOutput()
	cleanupPrimeCommand(cmd)
	if err != nil {
		t.Fatalf("prepare test executable %s: %v: %s", path, err, out)
	}
}
