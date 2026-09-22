//go:build !windows

package execfixture

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func configurePrimeCommand(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
	cmd.Cancel = func() error {
		return processgroup.TerminateCommand(cmd, cmd.Process.Pid, 0, processgroup.Options{})
	}
}

// Cleanup also handles a parent that exits while a descendant holds its pipes.
func cleanupPrimeCommand(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = processgroup.TerminateCommand(cmd, cmd.Process.Pid, 0, processgroup.Options{})
	}
}
