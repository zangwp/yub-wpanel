//go:build linux

package executor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureBackupCommandCancellation makes CommandContext terminate the whole
// process group. rsync/sshpass/tar may spawn children which otherwise outlive
// the command selected by os/exec when the backup context is cancelled.
func configureBackupCommandCancellation(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
}
