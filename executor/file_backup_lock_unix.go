//go:build !windows

package executor

import (
	"errors"
	"os"
	"syscall"
)

func tryFileBackupKernelLock(file *os.File) (bool, error) {
	if file == nil {
		return false, os.ErrInvalid
	}
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func releaseFileBackupKernelLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
