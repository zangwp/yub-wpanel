//go:build !linux

package executor

import "os/exec"

// Production is Linux. Other platforms retain CommandContext's direct-process
// cancellation so development builds continue to compile.
func configureCronCommandCancellation(_ *exec.Cmd) {}
