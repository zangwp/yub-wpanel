//go:build !linux

package executor

import "os/exec"

// Non-Linux development hosts retain CommandContext's direct-process
// cancellation. Production is Linux and uses process-group termination.
func configureBackupCommandCancellation(_ *exec.Cmd) {}
