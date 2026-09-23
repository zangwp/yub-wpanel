//go:build linux

package executor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureCronCommandCancellation makes a Cron timeout terminate the entire
// command tree. Killing only bash/runuser can leave grandchildren running and
// holding stdout/stderr pipes, which would keep the single task worker stuck.
func configureCronCommandCancellation(cmd *exec.Cmd) {
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
