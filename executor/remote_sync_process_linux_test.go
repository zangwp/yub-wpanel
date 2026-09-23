//go:build linux

package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRsyncCancellationKillsSpawnedProcessGroup(t *testing.T) {
	openTestDB(t)
	binDir := t.TempDir()
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	sshpassPath := filepath.Join(binDir, "sshpass")
	script := "#!/bin/sh\n" +
		"sleep 30 </dev/null >/dev/null 2>&1 &\n" +
		"child=$!\n" +
		"printf '%s' \"$child\" > \"$YUB_WPANEL_RSYNC_CHILD_PID\"\n" +
		"wait \"$child\"\n"
	if err := os.WriteFile(sshpassPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("YUB_WPANEL_RSYNC_CHILD_PID", childPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	ok := syncBackupToRsyncContext(ctx,
		backupsRoot+"/example.com/files/file_full_test.tar.gz",
		BackupSourceFile, 0, "", "backup.example.com", 22, "root", "password", "secret", "/backups", 1)
	if ok {
		t.Fatal("cancelled rsync unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancelled rsync returned too slowly: %s", elapsed)
	}

	rawPID, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatalf("fake sshpass did not record its child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("invalid child PID %q: %v", rawPID, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for processIsRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processIsRunning(pid) {
		t.Fatalf("rsync child process %d survived context cancellation", pid)
	}
}

func processIsRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// A killed child can remain a zombie very briefly while it is reaped. It
	// cannot execute further work and is therefore considered terminated.
	stat, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if readErr == nil {
		fields := strings.Fields(string(stat))
		if len(fields) >= 3 && fields[2] == "Z" {
			return false
		}
	}
	return err == nil || errors.Is(err, syscall.EPERM)
}
