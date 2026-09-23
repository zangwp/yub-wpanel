package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

var errScheduledWorkNotAllowed = errors.New("网站当前不允许运行自动任务")

const (
	fileBackupCommandTimeout  = 6 * time.Hour
	fileBackupLockWaitTimeout = 2 * time.Hour
	fileBackupLockRetry       = 250 * time.Millisecond
	fileBackupLockPath        = "/run/yub-wpanel/file-backup.lock"
)

var fileBackupArchiveSequence atomic.Uint64

func ExecuteFileBackup(siteID int, mode string, keepCount int) (string, error) {
	return ExecuteFileBackupContext(context.Background(), siteID, mode, keepCount)
}

// ExecuteFileBackupContext runs a manual file backup that can be cancelled by
// the caller. ExecuteFileBackup remains available for callers without a
// context, preserving the existing public API.
func ExecuteFileBackupContext(ctx context.Context, siteID int, mode string, keepCount int) (string, error) {
	return executeFileBackupContext(ctx, siteID, mode, keepCount, false)
}

func executeFileBackup(siteID int, mode string, keepCount int, scheduled bool) (string, error) {
	return executeFileBackupContext(context.Background(), siteID, mode, keepCount, scheduled)
}

func executeFileBackupContext(ctx context.Context, siteID int, mode string, keepCount int, scheduled bool) (string, error) {
	ctx, cancel := withFileBackupCommandTimeout(ctx)
	defer cancel()
	if err := fileBackupContextError(ctx, "文件备份"); err != nil {
		return "", err
	}
	if keepCount <= 0 {
		keepCount = 3
	}

	// 内核文件锁跨主服务和 cron 进程串行备份。锁文件固定保留在 root-only
	// 运行目录中；不通过 PID 文件推断存活状态，避免 stale-lock TOCTOU。
	backupLock, err := acquireFileBackupLock(ctx, fileBackupLockPath)
	if err != nil {
		if contextErr := fileBackupContextError(ctx, "等待备份锁"); contextErr != nil {
			return "", contextErr
		}
		return "", err
	}
	defer backupLock.Close()
	if scheduled {
		allowed, _, err := siteScheduledWorkAllowed(siteID)
		if err != nil {
			return "", fmt.Errorf("检查网站运行状态失败: %w", err)
		}
		if !allowed {
			return "", errScheduledWorkNotAllowed
		}
	}

	db := database.GetDB()
	var domain, webRoot string
	err = db.QueryRow("SELECT domain, web_root FROM websites WHERE id = ?", siteID).Scan(&domain, &webRoot)
	if err != nil {
		return "", fmt.Errorf("网站不存在")
	}

	backupDir := filepath.Join(backupsRoot, domain, "files")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", fmt.Errorf("创建文件备份目录失败: %w", err)
	}
	stampFile := filepath.Join(backupDir, ".last_backup.stamp")

	// Check disk space: need at least 1GB free after backup
	hasSpace, err := checkDiskSpaceContext(ctx, backupDir, 1024*1024*1024)
	if err != nil {
		return "", err
	}
	if !hasSpace {
		return "", fmt.Errorf("磁盘空间不足，备份取消")
	}
	remoteTarget, err := loadRemoteBackupTarget(ctx)
	if err != nil {
		return "", fmt.Errorf("读取远程备份设置失败: %w", err)
	}

	backupCutoff := time.Now()
	var tarName string
	var fullPath string
	var isFull bool

	if mode == "full" {
		isFull = true
	} else {
		if _, err := os.Stat(stampFile); os.IsNotExist(err) {
			isFull = true
		}
	}

	// 增量备份前确认远程是否已有全量基线：远程服务器被更换或远程数据被清空时，
	// 本地 stamp 文件依然存在，但远程可能只剩增量数据，此时强制转为全量重建基线。
	// 探测失败（连接失败/配置无效）同样按"未确认完整"处理，强制全量。
	forcedFullByRemote := false
	if !isFull {
		if err := fileBackupContextError(ctx, "文件备份"); err != nil {
			return "", err
		}
		hasFull, err := remoteTargetHasFullFileBackupContext(ctx, remoteTarget, domain)
		if err != nil {
			return "", fmt.Errorf("无法确认远程全量基线，已保留现有增量链: %w", err)
		}
		if err := fileBackupContextError(ctx, "文件备份"); err != nil {
			return "", err
		}
		if !hasFull {
			isFull = true
			forcedFullByRemote = true
		}
	}
	oldChain := []string{}
	if isFull {
		oldChain = loadFileBackupChain(siteID)
	}

	tarExcludes := []string{
		"--exclude=wp-content/cache",
		"--exclude=wp-content/upgrade",
		"--exclude=wp-content/debug.log",
		"--exclude=*.tmp",
		"--exclude=*.bak",
		"--exclude=*.backup",
		"--exclude=*.swp",
		"--exclude=wp-content/updraft",
		"--exclude=wp-content/ai1wm-backups",
		"--exclude=wp-content/backups-dup-lite",
		"--exclude=wp-content/backups-dup-pro",
		"--exclude=wp-content/wpvivid_backups",
		"--exclude=wp-content/backups",
		"--exclude=wp-content/backup-db",
	}

	archiveMode := "inc"
	if isFull {
		archiveMode = "full"
	}
	tarName, fullPath, err = reserveFileBackupArchive(backupDir, archiveMode, backupCutoff)
	if err != nil {
		return "", err
	}

	if isFull {
		if err := createFullFileArchiveContext(ctx, webRoot, fullPath, tarExcludes); err != nil {
			return "", err
		}
	} else {
		uploadsDir := filepath.Join(webRoot, "wp-content", "uploads")
		if _, err := os.Stat(uploadsDir); os.IsNotExist(err) {
			_ = os.Remove(fullPath)
			return "", fmt.Errorf("uploads 目录不存在")
		}
		hasFiles, err := createIncrementalFileArchiveContext(ctx, uploadsDir, stampFile, fullPath)
		if err != nil {
			_ = os.Remove(fullPath)
			return "", err
		}
		if !hasFiles {
			_ = os.Remove(fullPath)
			if err := writeBackupStamp(stampFile, backupCutoff); err != nil {
				return "", fmt.Errorf("更新增量备份时间戳失败: %w", err)
			}
			return fmt.Sprintf("%s 文件备份跳过: 无新文件", domain), nil
		}
	}

	if err := verifyFileBackupArchiveContext(ctx, fullPath); err != nil {
		_ = os.Remove(fullPath)
		return "", err
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("读取文件备份结果失败: %w", err)
	}
	size := info.Size()
	modeLabel := "incremental"
	if isFull {
		modeLabel = "full"
	}
	if err := fileBackupContextError(ctx, "文件备份"); err != nil {
		return "", err
	}
	if !recordFileBackup(siteID, tarName, size, modeLabel, domain) {
		return "", fmt.Errorf("文件备份已生成，但记录写入失败，未执行远程换代")
	}

	remoteEnabled := remoteTarget.Enabled
	remoteSynced := false
	if remoteEnabled {
		remoteSynced = syncBackupToRemoteTargetContext(ctx, remoteTarget, fullPath, BackupSourceFile, siteID, tarName)
	}
	if err := fileBackupContextError(ctx, "文件备份"); err != nil {
		return "", err
	}
	if remoteEnabled && !remoteSynced {
		return "", fmt.Errorf("本地文件备份已生成，但远程同步失败，旧备份链已保留")
	}
	if err := writeBackupStamp(stampFile, backupCutoff); err != nil {
		return "", fmt.Errorf("文件备份已生成，但更新增量备份时间戳失败: %w", err)
	}
	if isFull {
		if remoteEnabled && remoteSynced {
			if _, err := cleanupSupersededFileBackupChainContext(ctx, remoteTarget, siteID, domain, backupDir, oldChain); err != nil {
				return "", fmt.Errorf("新全量备份已完成，但旧备份链清理中止: %w", err)
			}
		} else if !remoteEnabled {
			cleanOldBackups(backupDir, keepCount, siteID)
		}
	}
	logMsg := fmt.Sprintf("%s 文件备份成功: %s (%s)", domain, tarName, map[bool]string{true: "全量", false: "增量"}[isFull])
	if forcedFullByRemote {
		logMsg += "；检测到远程无全量基线，已自动转为全量备份"
	}
	appendCronLog(logMsg)
	return logMsg, nil
}

func withFileBackupCommandTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, fileBackupCommandTimeout)
}

// reserveFileBackupArchive atomically claims a unique filename before tar is
// invoked. Nanoseconds, PID and a process-local sequence make collisions rare;
// O_EXCL is the final cross-process guarantee and prevents overwriting an
// existing backup even if clocks or process identifiers repeat.
func reserveFileBackupArchive(backupDir, mode string, cutoff time.Time) (string, string, error) {
	if mode != "full" && mode != "inc" {
		return "", "", fmt.Errorf("文件备份模式无效")
	}
	for attempt := 0; attempt < 128; attempt++ {
		sequence := fileBackupArchiveSequence.Add(1)
		filename := fmt.Sprintf("file_%s_%s_p%d_%016x.tar.gz",
			mode, cutoff.Format("20060102_150405.000000000"), os.Getpid(), sequence)
		fullPath := filepath.Join(backupDir, filename)
		file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(fullPath)
				return "", "", fmt.Errorf("预留文件备份路径失败: %w", closeErr)
			}
			return filename, fullPath, nil
		}
		if !os.IsExist(err) {
			return "", "", fmt.Errorf("预留文件备份路径失败: %w", err)
		}
	}
	return "", "", fmt.Errorf("无法生成唯一文件备份名称")
}

type fileBackupLock struct {
	file   *os.File
	closed bool
}

func (lock *fileBackupLock) Close() error {
	if lock == nil || lock.file == nil || lock.closed {
		return nil
	}
	lock.closed = true
	unlockErr := releaseFileBackupKernelLock(lock.file)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func acquireFileBackupLock(ctx context.Context, lockPath string) (*fileBackupLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockDir := filepath.Dir(lockPath)
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("创建备份锁目录失败: %w", err)
	}
	info, err := os.Lstat(lockDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("备份锁目录身份无效")
	}
	if err := os.Chmod(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("设置备份锁目录权限失败: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开备份锁失败: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("设置备份锁权限失败: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, fileBackupLockWaitTimeout)
	defer cancel()
	for {
		locked, lockErr := tryFileBackupKernelLock(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("获取备份锁失败: %w", lockErr)
		}
		if locked {
			return &fileBackupLock{file: file}, nil
		}

		timer := time.NewTimer(fileBackupLockRetry)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			_ = file.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("等待备份锁超时（有其他备份任务未完成），请稍后重试")
		case <-timer.C:
		}
	}
}

func fileBackupContextError(ctx context.Context, operation string) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s超时: %w", operation, ctx.Err())
	}
	return fmt.Errorf("%s已取消: %w", operation, ctx.Err())
}

func createFullFileArchiveContext(ctx context.Context, webRoot, target string, excludes []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	args := []string{"-czf", target, "--warning=no-file-changed"}
	args = append(args, excludes...)
	args = append(args, "-C", filepath.Dir(webRoot), filepath.Base(webRoot))
	cmd := exec.CommandContext(ctx, "tar", args...)
	configureBackupCommandCancellation(cmd)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	_ = os.Remove(target)
	if contextErr := fileBackupContextError(ctx, "全量备份"); contextErr != nil {
		return contextErr
	}
	if len(out) == 0 {
		return fmt.Errorf("全量备份失败: %v", err)
	}
	return fmt.Errorf("全量备份失败: %s", strings.TrimSpace(string(out)))
}

func createIncrementalFileArchive(uploadsDir, stampFile, target string) (bool, error) {
	return createIncrementalFileArchiveContext(context.Background(), uploadsDir, stampFile, target)
}

func createIncrementalFileArchiveContext(ctx context.Context, uploadsDir, stampFile, target string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	checkCmd := exec.CommandContext(ctx, "find", uploadsDir, "-newer", stampFile, "-type", "f")
	configureBackupCommandCancellation(checkCmd)
	files, err := checkCmd.CombinedOutput()
	if err != nil {
		if contextErr := fileBackupContextError(ctx, "扫描增量文件"); contextErr != nil {
			return false, contextErr
		}
		return false, fmt.Errorf("扫描增量文件失败: %s", strings.TrimSpace(string(files)))
	}
	if contextErr := fileBackupContextError(ctx, "扫描增量文件"); contextErr != nil {
		return false, contextErr
	}
	if len(files) == 0 {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "tar", "-czf", target, "--verbatim-files-from", "-T", "-")
	configureBackupCommandCancellation(cmd)
	cmd.Stdin = bytes.NewReader(files)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(target)
		if contextErr := fileBackupContextError(ctx, "增量备份"); contextErr != nil {
			return false, contextErr
		}
		if len(out) == 0 {
			return false, fmt.Errorf("增量备份失败: %v", err)
		}
		return false, fmt.Errorf("增量备份失败: %s", strings.TrimSpace(string(out)))
	}
	return true, nil
}

func verifyFileBackupArchive(path string) error {
	return verifyFileBackupArchiveContext(context.Background(), path)
}

func verifyFileBackupArchiveContext(ctx context.Context, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "tar", "-tzf", path)
	configureBackupCommandCancellation(cmd)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if contextErr := fileBackupContextError(ctx, "文件备份归档校验"); contextErr != nil {
			return contextErr
		}
		return fmt.Errorf("文件备份归档校验失败: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeBackupStamp(path string, cutoff time.Time) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".last-backup-stamp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(cutoff.Format(time.RFC3339Nano)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		return err
	}
	if err := os.Chtimes(tmpPath, cutoff, cutoff); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func remoteBackupEnabled() bool {
	var enabled int
	return database.GetDB().QueryRow(`SELECT enabled FROM remote_backup_settings WHERE id=1`).Scan(&enabled) == nil && enabled == 1
}

func loadFileBackupChain(siteID int) []string {
	rows, err := database.GetDB().Query(`SELECT filename FROM file_backups WHERE site_id=? ORDER BY created_at, id`, siteID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var filename string
		if rows.Scan(&filename) == nil {
			files = append(files, filename)
		}
	}
	return files
}

// cleanupSupersededFileBackupChain 仅清理新全量开始前冻结的旧链名单。远端删除成功后才删除
// 本地文件和数据库记录；失败项保留，供后台维护重试。
func cleanupSupersededFileBackupChain(siteID int, domain, backupDir string, oldChain []string) int {
	target, err := loadRemoteBackupTarget(context.Background())
	if err != nil || !target.Enabled {
		return 0
	}
	cleaned, _ := cleanupSupersededFileBackupChainContext(context.Background(), target, siteID, domain, backupDir, oldChain)
	return cleaned
}

func cleanupSupersededFileBackupChainContext(ctx context.Context, target remoteBackupTarget, siteID int, domain, backupDir string, oldChain []string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return cleanupSupersededFileBackupChainWithContext(ctx, siteID, domain, backupDir, oldChain,
		func(deleteCtx context.Context, relPath string) error {
			return deleteRemoteBackupFileFromTargetContext(deleteCtx, target, relPath)
		},
		func(path string) error { return os.Remove(path) },
		func(siteID int, filename string) error {
			_, err := database.GetDB().ExecContext(ctx, `DELETE FROM file_backups WHERE site_id=? AND filename=?`, siteID, filename)
			return err
		})
}

// cleanupSupersededFileBackupChainWith is retained for focused cleanup tests.
// Production uses the context-aware variant above with a frozen remote target.
func cleanupSupersededFileBackupChainWith(siteID int, domain, backupDir string, oldChain []string,
	deleteRemote func(string) error, removeLocal func(string) error, deleteRecord func(int, string) error) int {
	cleaned, _ := cleanupSupersededFileBackupChainWithContext(context.Background(), siteID, domain, backupDir, oldChain,
		func(_ context.Context, relPath string) error { return deleteRemote(relPath) }, removeLocal, deleteRecord)
	return cleaned
}

func cleanupSupersededFileBackupChainWithContext(ctx context.Context, siteID int, domain, backupDir string, oldChain []string,
	deleteRemote func(context.Context, string) error, removeLocal func(string) error, deleteRecord func(int, string) error) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cleaned := 0
	for _, filename := range oldChain {
		if err := ctx.Err(); err != nil {
			return cleaned, err
		}
		relPath := domain + "/files/" + filename
		if err := deleteRemote(ctx, relPath); err != nil {
			if ctx.Err() != nil {
				return cleaned, ctx.Err()
			}
			log.Printf("清理远程旧文件备份失败 [%s]: %v", relPath, err)
			continue
		}
		localPath := filepath.Join(backupDir, filename)
		if err := removeLocal(localPath); err != nil && !os.IsNotExist(err) {
			log.Printf("清理本地旧文件备份失败 [%s]: %v", localPath, err)
			continue
		}
		if err := deleteRecord(siteID, filename); err != nil {
			if ctx.Err() != nil {
				return cleaned, ctx.Err()
			}
			log.Printf("清理旧文件备份记录失败 [%s]: %v", relPath, err)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

func cleanOldBackups(dir string, keep int, siteID int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type tarEntry struct {
		name    string
		modTime time.Time
	}
	var tars []tarEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "file_full_") && filepath.Ext(e.Name()) == ".gz" {
			info, _ := e.Info()
			mt := time.Time{}
			if info != nil {
				mt = info.ModTime()
			}
			tars = append(tars, tarEntry{name: e.Name(), modTime: mt})
		}
	}
	if len(tars) <= keep {
		return
	}
	sort.Slice(tars, func(i, j int) bool { return tars[i].modTime.Before(tars[j].modTime) })
	db := database.GetDB()
	for i := 0; i < len(tars)-keep; i++ {
		// 文件确实还在磁盘上删除失败（权限/占用/文件系统异常）时，不能删 file_backups 记录，
		// 否则会出现"磁盘有文件但总览查不到"的反向不一致；下次轮转会重试。
		// 文件本就已经不存在（IsNotExist）则视为清理成功，照常删除记录。
		if err := os.Remove(filepath.Join(dir, tars[i].name)); err != nil && !os.IsNotExist(err) {
			log.Printf("清理过期文件备份失败 [%s]: %v", tars[i].name, err)
			continue
		}
		// 保持 file_backups 表和磁盘一致，避免备份总览页面展示已被轮转清理的记录。
		db.Exec(`DELETE FROM file_backups WHERE site_id = ? AND filename = ?`, siteID, tars[i].name)
	}
}

func checkDiskSpace(backupDir string, minFree int64) bool {
	ok, _ := checkDiskSpaceContext(context.Background(), backupDir, minFree)
	return ok
}

func checkDiskSpaceContext(ctx context.Context, backupDir string, minFree int64) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "df", "-B1", backupDir)
	configureBackupCommandCancellation(cmd)
	out, err := cmd.Output()
	if err != nil {
		if contextErr := fileBackupContextError(ctx, "检查备份磁盘空间"); contextErr != nil {
			return false, contextErr
		}
		return true, nil // can't check, allow to proceed
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return true, nil
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return true, nil
	}
	free, _ := strconv.ParseInt(fields[3], 10, 64)
	return free >= minFree, nil
}

// recordFileBackup 把生成的文件备份写入 file_backups 表，供备份总览页面展示。
// 写入失败不影响已经生成的备份文件（文件备份成本较高，不因记录写入失败而丢弃），
// 但必须可见地记录下来，避免总览页面缺记录却无人知晓。
func recordFileBackup(siteID int, filename string, size int64, mode, domain string) bool {
	if _, err := database.GetDB().Exec(`INSERT INTO file_backups (site_id, filename, file_size, mode) VALUES (?, ?, ?, ?)`,
		siteID, filename, size, mode); err != nil {
		msg := fmt.Sprintf("%s 文件备份记录写入失败: %v；该备份不会出现在备份总览页面，后续远程同步状态也无法回写", domain, err)
		log.Printf("文件备份记录写入 file_backups 失败 [%s]: %v", domain, err)
		appendCronLog(msg)
		return false
	}
	return true
}

func appendCronLog(msg string) {
	logFile := "/www/server/panel/logs/cron.log"
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg))
}
