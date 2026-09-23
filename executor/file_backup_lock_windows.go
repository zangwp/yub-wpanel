//go:build windows

package executor

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryFileBackupKernelLock(file *os.File) (bool, error) {
	if file == nil {
		return false, os.ErrInvalid
	}
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func releaseFileBackupKernelLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
