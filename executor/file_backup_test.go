package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

func TestCanonicalFileBackupLockPathUsesPrivateRuntimeDirectory(t *testing.T) {
	if canonicalFileBackupLockPath != "/run/yub-wpanel/file-backup.lock" {
		t.Fatalf("canonicalFileBackupLockPath=%q", canonicalFileBackupLockPath)
	}
}

func TestFileBackupCommandsHonorCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := t.TempDir()
	target := filepath.Join(root, "backup.tar.gz")
	if err := createFullFileArchiveContext(ctx, filepath.Join(root, "site"), target, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("full archive error = %v, want context.Canceled", err)
	}
	if _, err := createIncrementalFileArchiveContext(ctx, filepath.Join(root, "uploads"), filepath.Join(root, "stamp"), target); !errors.Is(err, context.Canceled) {
		t.Fatalf("incremental archive error = %v, want context.Canceled", err)
	}
	if err := verifyFileBackupArchiveContext(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("archive verification error = %v, want context.Canceled", err)
	}
	if _, err := checkDiskSpaceContext(ctx, root, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("disk-space check error = %v, want context.Canceled", err)
	}
}

func TestFileBackupCommandContextHasHardDeadlineAndKeepsEarlierCallerDeadline(t *testing.T) {
	ctx, cancel := withFileBackupCommandTimeout(context.Background())
	deadline, ok := ctx.Deadline()
	if !ok {
		cancel()
		t.Fatal("default file backup context has no deadline")
	}
	remaining := time.Until(deadline)
	cancel()
	if remaining <= fileBackupCommandTimeout-time.Minute || remaining > fileBackupCommandTimeout {
		t.Fatalf("default file backup deadline remaining = %s", remaining)
	}

	parent, parentCancel := context.WithTimeout(context.Background(), time.Minute)
	defer parentCancel()
	bounded, boundedCancel := withFileBackupCommandTimeout(parent)
	defer boundedCancel()
	boundedDeadline, ok := bounded.Deadline()
	if !ok {
		t.Fatal("bounded file backup context has no deadline")
	}
	if boundedDeadline.After(time.Now().Add(time.Minute)) {
		t.Fatalf("file backup context extended caller deadline to %s", boundedDeadline)
	}
}

func TestAcquireFileBackupLockHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lockPath := filepath.Join(t.TempDir(), "backup.lock")
	started := time.Now()
	lock, err := acquireFileBackupLock(ctx, lockPath)
	if lock != nil {
		_ = lock.Close()
		t.Fatal("cancelled lock acquisition unexpectedly returned a lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled lock wait returned too slowly: %s", elapsed)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled lock wait created lock file: %v", statErr)
	}
}

func TestReserveFileBackupArchiveSameInstantNeverOverwrites(t *testing.T) {
	cutoff := time.Date(2026, time.September, 23, 12, 34, 56, 789, time.UTC)
	for _, mode := range []string{"full", "inc"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			firstName, firstPath, err := reserveFileBackupArchive(dir, mode, cutoff)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(firstPath, []byte("first archive"), 0600); err != nil {
				t.Fatal(err)
			}
			secondName, secondPath, err := reserveFileBackupArchive(dir, mode, cutoff)
			if err != nil {
				t.Fatal(err)
			}
			if firstName == secondName || firstPath == secondPath {
				t.Fatalf("same-instant reservations collided: %q", firstName)
			}
			contents, err := os.ReadFile(firstPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "first archive" {
				t.Fatalf("second reservation overwrote first archive: %q", contents)
			}
			if info, err := os.Stat(secondPath); err != nil || info.Size() != 0 {
				t.Fatalf("second reservation is not a distinct empty placeholder: info=%v err=%v", info, err)
			}
		})
	}
}

func TestAcquireFileBackupLockIgnoresPreexistingContents(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "backup.lock")
	if err := os.WriteFile(lockPath, []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireFileBackupLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("pre-existing unlocked lock file prevented acquisition: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileBackupLockCoordinatesProcesses(t *testing.T) {
	const (
		helperLockEnv     = "YUB_WPANEL_FILE_BACKUP_LOCK_HELPER_PATH"
		helperStartedEnv  = "YUB_WPANEL_FILE_BACKUP_LOCK_HELPER_STARTED"
		helperAcquiredEnv = "YUB_WPANEL_FILE_BACKUP_LOCK_HELPER_ACQUIRED"
	)
	if lockPath := os.Getenv(helperLockEnv); lockPath != "" {
		if err := os.WriteFile(os.Getenv(helperStartedEnv), nil, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lock, err := acquireFileBackupLock(ctx, lockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := os.WriteFile(os.Getenv(helperAcquiredEnv), nil, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	lockPath := filepath.Join(root, "backup.lock")
	startedPath := filepath.Join(root, "started")
	acquiredPath := filepath.Join(root, "acquired")
	parentLock, err := acquireFileBackupLock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parentLock.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileBackupLockCoordinatesProcesses$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		helperLockEnv+"="+lockPath,
		helperStartedEnv+"="+startedPath,
		helperAcquiredEnv+"="+acquiredPath,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock helper did not start: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2 * fileBackupLockRetry)
	if _, err := os.Stat(acquiredPath); err == nil {
		t.Fatal("second process acquired file-backup lock while parent held it")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := parentLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lock helper failed: %v: %s", err, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("lock helper did not acquire released lock: %s", output.String())
	}
	if _, err := os.Stat(acquiredPath); err != nil {
		t.Fatalf("lock helper never recorded acquisition: %v", err)
	}
}

func TestVerifyFileBackupArchiveContextKillsTimedOutTar(t *testing.T) {
	binDir := t.TempDir()
	tarPath := filepath.Join(binDir, "tar")
	if err := os.WriteFile(tarPath, []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := verifyFileBackupArchiveContext(ctx, filepath.Join(t.TempDir(), "archive.tar.gz"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verification error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timed-out tar was not killed promptly: %s", elapsed)
	}
}

func TestCleanOldBackupsRemovesRotatedFileBackupsRows(t *testing.T) {
	openTestDB(t)
	insertMinimalWebsite(t, "rotate.example.com")

	dir := t.TempDir()
	names := []string{
		"file_full_20260101_000000.tar.gz",
		"file_full_20260102_000000.tar.gz",
		"file_full_20260103_000000.tar.gz",
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		if _, err := database.GetDB().Exec(`INSERT INTO file_backups (site_id, filename, file_size, mode) VALUES (1, ?, 1, 'full')`, name); err != nil {
			t.Fatalf("insert file_backups %s: %v", name, err)
		}
	}

	cleanOldBackups(dir, 2, 1)

	if _, err := os.Stat(filepath.Join(dir, names[0])); !os.IsNotExist(err) {
		t.Fatalf("oldest backup file should have been removed from disk, stat err = %v", err)
	}
	var stillHasOldest int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM file_backups WHERE filename = ?`, names[0]).Scan(&stillHasOldest); err != nil {
		t.Fatalf("query file_backups: %v", err)
	}
	if stillHasOldest != 0 {
		t.Fatal("file_backups row for rotated-out backup should have been deleted alongside the file")
	}

	for _, name := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("kept backup file missing on disk: %s: %v", name, err)
		}
		var count int
		if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM file_backups WHERE filename = ?`, name).Scan(&count); err != nil {
			t.Fatalf("query file_backups: %v", err)
		}
		if count != 1 {
			t.Fatalf("file_backups row for kept backup %s missing", name)
		}
	}
}

func TestCleanOldBackupsKeepsDBRowWhenFileRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based removal failure cannot be simulated")
	}
	openTestDB(t)
	insertMinimalWebsite(t, "rotate-fail.example.com")

	dir := t.TempDir()
	names := []string{
		"file_full_20260101_000000.tar.gz",
		"file_full_20260102_000000.tar.gz",
		"file_full_20260103_000000.tar.gz",
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		if _, err := database.GetDB().Exec(`INSERT INTO file_backups (site_id, filename, file_size, mode) VALUES (1, ?, 1, 'full')`, name); err != nil {
			t.Fatalf("insert file_backups %s: %v", name, err)
		}
	}

	// 去掉目录写权限，让 os.Remove 因权限不足失败（Linux 下删除文件需要父目录的写权限）。
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0755) })

	cleanOldBackups(dir, 2, 1)

	// 文件删除失败，仍留在磁盘上：对应的 file_backups 记录不能被删除，
	// 否则总览页面会漏掉一个实际仍存在的本地备份文件。
	if _, err := os.Stat(filepath.Join(dir, names[0])); err != nil {
		t.Fatalf("oldest backup file should still be on disk after failed removal: %v", err)
	}
	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM file_backups WHERE filename = ?`, names[0]).Scan(&count); err != nil {
		t.Fatalf("query file_backups: %v", err)
	}
	if count != 1 {
		t.Fatal("file_backups row should NOT be deleted when the underlying file removal failed")
	}
}

func TestRecordFileBackupInsertsRow(t *testing.T) {
	openTestDB(t)
	insertMinimalWebsite(t, "record-ok.example.com")

	recordFileBackup(1, "file_full_20260101_000000.tar.gz", 123, "full", "record-ok.example.com")

	var size int64
	var mode string
	if err := database.GetDB().QueryRow(`SELECT file_size, mode FROM file_backups WHERE site_id = 1 AND filename = ?`,
		"file_full_20260101_000000.tar.gz").Scan(&size, &mode); err != nil {
		t.Fatalf("query file_backups: %v", err)
	}
	if size != 123 || mode != "full" {
		t.Fatalf("file_backups row = (%d, %q), want (123, \"full\")", size, mode)
	}
}

func TestRecordFileBackupLogsWithoutPanicOnInsertFailure(t *testing.T) {
	openTestDB(t)
	insertMinimalWebsite(t, "record-fail.example.com")
	if _, err := database.GetDB().Exec(`DROP TABLE file_backups`); err != nil {
		t.Fatalf("drop file_backups: %v", err)
	}

	// file_backups 写入失败不应 panic，也不应影响调用方（已生成的备份文件保留）；
	// 失败情况通过日志可见，而不是静默吞掉。
	recordFileBackup(1, "file_full_20260101_000000.tar.gz", 123, "full", "record-fail.example.com")
}

func TestCleanupOldDBBackupsKeepsRecordWhenFileRemovalFails(t *testing.T) {
	deleted := false
	cleanupOldDBBackupsWith([]oldDBBackup{{id: 7, filename: "old.sql.gz"}},
		func(string) error { return errors.New("disk error") },
		func(int) error { deleted = true; return nil })
	if deleted {
		t.Fatal("database record deleted after file removal failure")
	}
}

func TestCleanupOldDBBackupsDeletesMissingFileRecord(t *testing.T) {
	deleted := false
	cleanupOldDBBackupsWith([]oldDBBackup{{id: 7, filename: "missing.sql.gz"}},
		func(string) error { return os.ErrNotExist },
		func(id int) error { deleted = id == 7; return nil })
	if !deleted {
		t.Fatal("missing file record was not deleted")
	}
}

func TestWriteBackupStampUsesFrozenCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".last_backup.stamp")
	cutoff := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := writeBackupStamp(path, cutoff); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(cutoff) {
		t.Fatalf("stamp mtime = %s, want %s", info.ModTime(), cutoff)
	}
}

func TestCreateIncrementalFileArchivePropagatesFindFailure(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "stamp")
	if err := os.WriteFile(stamp, []byte("stamp"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := createIncrementalFileArchive(filepath.Join(t.TempDir(), "missing"), stamp, filepath.Join(t.TempDir(), "out.tar.gz")); err == nil {
		t.Fatal("missing uploads directory error = nil")
	}
}

func TestCreateAndVerifyIncrementalFileArchive(t *testing.T) {
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	if err := os.Mkdir(uploads, 0755); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(root, "stamp")
	if err := os.WriteFile(stamp, []byte("stamp"), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stamp, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "new.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "incremental.tar.gz")
	hasFiles, err := createIncrementalFileArchive(uploads, stamp, target)
	if err != nil || !hasFiles {
		t.Fatalf("createIncrementalFileArchive = %v, %v", hasFiles, err)
	}
	if err := verifyFileBackupArchive(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("not an archive"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyFileBackupArchive(target); err == nil {
		t.Fatal("corrupt archive verification error = nil")
	}
}
