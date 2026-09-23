package executor

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/database"
)

var cronLogFile = "/www/server/panel/logs/cron.log"

const canonicalCronJobLockDir = "/run/yub-wpanel/cron-locks"

var cronJobLockDir = canonicalCronJobLockDir

// ErrCronJobAlreadyRunning reports that execution or mutation already owns a
// job's non-blocking process lock.
var ErrCronJobAlreadyRunning = errors.New("cron job is already running")

var restartCronService = func() (string, error) {
	return executeCommand("systemctl", "restart", "cron")
}

var executeCronFileBackup = executeFileBackupContext
var syncCronParentDirectory = syncDirectory

// cronRenderMu serializes every writer of the managed cron file, including
// synchronous API mutations and the legacy internal render task.
var cronRenderMu sync.Mutex

const (
	cronLogKeepLines        = 1000
	cronLogMaxBytes         = 4 << 20
	cronOutputMaxBytes      = 64 << 10
	cronJobExecutionTimeout = 10 * time.Minute
	managedCronHeader       = "# YUB WPanel Cron Jobs — DO NOT EDIT MANUALLY"
	managedCronPath         = "/etc/cron.d/yub_wpanel_cron"
)

const cronOutputTruncatedMarker = "\n[output truncated by YUB WPanel]\n"

type boundedCronOutput struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedCronOutput(limit int) *boundedCronOutput {
	return &boundedCronOutput{limit: limit}
}

func (w *boundedCronOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written := len(p)
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.buf.Write(p)
	}
	if written > remaining {
		w.truncated = true
	}
	return written, nil
}

func (w *boundedCronOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.limit <= 0 {
		return ""
	}
	data := w.buf.Bytes()
	if !w.truncated {
		return string(data)
	}
	marker := []byte(cronOutputTruncatedMarker)
	if len(marker) >= w.limit {
		return string(marker[:w.limit])
	}
	contentLen := w.limit - len(marker)
	if len(data) < contentLen {
		contentLen = len(data)
	}
	result := make([]byte, 0, contentLen+len(marker))
	result = append(result, data[:contentLen]...)
	result = append(result, marker...)
	return string(result)
}

func boundCronOutput(value string) string {
	if len(value) <= cronOutputMaxBytes {
		return value
	}
	contentLen := cronOutputMaxBytes - len(cronOutputTruncatedMarker)
	if contentLen < 0 {
		contentLen = 0
	}
	return value[:contentLen] + cronOutputTruncatedMarker
}

func runCronCommand(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	configureCronCommandCancellation(cmd)
	output := newBoundedCronOutput(cronOutputMaxBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	return output.String(), err
}

func cronExecutionTimeout(taskType string) time.Duration {
	if taskType == "file_backup" {
		return fileBackupCommandTimeout
	}
	return cronJobExecutionTimeout
}

// ReconcileInterruptedManualCronJobs releases claims owned by the main
// process's in-memory queue. The scheduled Cron CLI never sets running, so a
// running=1 row at main-service startup can only be a manual execution lost
// when the previous panel process exited.
func ReconcileInterruptedManualCronJobs(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, errors.New("database unavailable")
	}
	result, err := db.Exec(`UPDATE cron_jobs SET running=0 WHERE running=1`)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	if f, openErr := os.OpenFile(cronLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); openErr == nil {
		_, _ = fmt.Fprintf(f, "[%s] INTERRUPTED 手动任务因面板进程重启中断，共 %d 个\n", now, count)
		_ = f.Close()
		pruneCronLog(cronLogFile, cronLogKeepLines)
	}
	return count, nil
}

func executeRenderCron(task *Task) TaskResult {
	return renderCronConfig()
}

// RenderCronConfig synchronously reconciles the managed cron file. Callers
// that already serialize their database mutations use this instead of waiting
// behind unrelated long-running work in the global task queue.
func RenderCronConfig() TaskResult {
	return renderCronConfig()
}

func renderCronConfig() TaskResult {
	cronRenderMu.Lock()
	defer cronRenderMu.Unlock()
	return renderCronConfigLocked()
}

func renderCronConfigLocked() TaskResult {
	cfg := config.AppConfig
	if cfg == nil {
		return TaskResult{Success: false, Message: "配置未加载"}
	}

	db := database.GetDB()
	rows, err := db.Query(
		`SELECT id,name,cron_expression FROM cron_jobs WHERE enabled=1`,
	)
	if err != nil {
		log.Printf("查询Cron任务失败: %v", err)
		return TaskResult{Success: false, Message: "查询Cron任务失败"}
	}

	cronLines, err := managedCronLines(rows)
	closeErr := rows.Close()
	if err == nil && closeErr != nil {
		err = fmt.Errorf("close Cron task rows: %w", closeErr)
	}
	if err != nil {
		log.Printf("读取Cron任务失败: %v", err)
		return TaskResult{Success: false, Message: "读取Cron任务失败，已保留原配置"}
	}

	cronContent := strings.Join(cronLines, "\n") + "\n"

	if err := writeManagedCronFile(cfg.Paths.CronFile, []byte(cronContent)); err != nil {
		log.Printf("写入Cron文件失败: %v", err)
		return TaskResult{Success: false, Message: "写入Cron文件失败"}
	}

	if output, err := restartCronService(); err != nil {
		log.Printf("重启Cron服务失败: %v, output: %s", err, strings.TrimSpace(output))
		return TaskResult{Success: false, Message: "Cron配置已写入，但重启Cron服务失败"}
	}

	return TaskResult{Success: true, Message: "Cron配置已更新"}
}

type managedCronRowReader interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func managedCronLines(rows managedCronRowReader) ([]string, error) {
	cronLines := []string{
		managedCronHeader,
		"SHELL=/bin/bash",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"",
	}
	for rows.Next() {
		var id int
		var name, cronExpr string
		if err := rows.Scan(&id, &name, &cronExpr); err != nil {
			return nil, fmt.Errorf("scan Cron task: %w", err)
		}
		safeName := sanitizeCronArg(name)
		line := fmt.Sprintf(`%s root /usr/local/bin/yub-wpanel --run-scheduled-cron=%d --config=/www/server/panel/config.json # %s`, cronExpr, id, safeName)
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		cronLines = append(cronLines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Cron tasks: %w", err)
	}
	return cronLines, nil
}

// ValidateManagedCronFileIdentity accepts an absent target or the one exact
// regular file format owned by this distribution. Production's canonical
// /etc/cron.d target must be root-owned. Alternate paths are used by tests and
// must be owned by the current process.
func ValidateManagedCronFileIdentity(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return fmt.Errorf("cron identity path is not absolute")
	}

	expectedUID := uint32(os.Geteuid())
	if path == managedCronPath {
		if os.Geteuid() != 0 {
			return fmt.Errorf("canonical cron identity requires root")
		}
		expectedUID = 0
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect cron parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("cron parent identity is unsafe")
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != expectedUID {
		return fmt.Errorf("cron parent ownership is unsafe")
	}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cron target: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("cron target identity is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedUID || stat.Nlink != 1 {
		return fmt.Errorf("cron target ownership is unsafe")
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open cron target: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256), 4096)
	if !scanner.Scan() || scanner.Text() != managedCronHeader {
		return fmt.Errorf("cron target is not YUB WPanel managed")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read cron target: %w", err)
	}
	return nil
}

func writeManagedCronFile(path string, content []byte) (retErr error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if len(content) == 0 || !strings.HasPrefix(string(content), managedCronHeader+"\n") {
		return fmt.Errorf("cron content is missing the ownership marker")
	}
	if err := ValidateManagedCronFileIdentity(path); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create cron temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("set cron temporary permissions: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write cron temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync cron temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close cron temporary file: %w", err)
	}

	// Re-check immediately before rename. Rename replaces a raced symlink
	// itself instead of following it, and keeps the update atomic for cron.
	if err := ValidateManagedCronFileIdentity(path); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace cron target: %w", err)
	}
	if err := syncCronParentDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync cron parent directory: %w", err)
	}
	if err := ValidateManagedCronFileIdentity(path); err != nil {
		return fmt.Errorf("verify replaced cron target: %w", err)
	}
	return nil
}

func executeRunCron(task *Task) TaskResult {
	payload, ok := task.Payload.(*RunCronPayload)
	if !ok {
		return TaskResult{Success: false, Message: "任务参数类型错误"}
	}
	// The HTTP handler claims running=1 before enqueueing. Always release that
	// claim, including a state change between confirmation and queue execution.
	defer database.GetDB().Exec(`UPDATE cron_jobs SET running=0 WHERE id=?`, payload.JobID)
	lock, err := acquireCronJobExecutionLock(payload.JobID)
	if err != nil {
		if errors.Is(err, ErrCronJobAlreadyRunning) {
			return TaskResult{Success: false, Message: "任务正在执行中，请稍后再试"}
		}
		return TaskResult{Success: false, Message: "取得任务运行锁失败"}
	}
	defer lock.Close()

	suspended, _, reason, err := CronJobRuntimeSuspended(payload.JobID)
	if err != nil {
		return TaskResult{Success: false, Message: "检查任务关联网站状态失败"}
	}
	if suspended && (reason != "paused" || !payload.ConfirmPaused) {
		return TaskResult{Success: false, Message: "关联网站当前不允许运行该任务"}
	}
	return runCronJob(payload.JobID, false)
}

type cronJobExecution struct {
	id, keepCount                                  int
	name, command, runAsUser, taskType, backupMode string
	siteID                                         sql.NullInt64
}

func loadCronJob(jobID int) (cronJobExecution, bool, error) {
	var job cronJobExecution
	var enabled int
	err := database.GetDB().QueryRow(`SELECT id,name,command,run_as_user,task_type,backup_mode,keep_count,site_id,enabled
		FROM cron_jobs WHERE id=?`, jobID).Scan(&job.id, &job.name, &job.command, &job.runAsUser,
		&job.taskType, &job.backupMode, &job.keepCount, &job.siteID, &enabled)
	return job, enabled == 1, err
}

func cronJobSite(job cronJobExecution) (int, string, error) {
	if job.siteID.Valid {
		var domain string
		err := database.GetDB().QueryRow(`SELECT domain FROM websites WHERE id=?`, job.siteID.Int64).Scan(&domain)
		return int(job.siteID.Int64), domain, err
	}
	if strings.TrimSpace(job.runAsUser) == "" {
		return 0, "", nil
	}
	rows, err := database.GetDB().Query(`SELECT id,domain FROM websites WHERE system_user=?`, job.runAsUser)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var id int
	var domain string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id, &domain); err != nil {
			return 0, "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	if count != 1 {
		return 0, "", fmt.Errorf("运行用户的网站归属异常")
	}
	return id, domain, nil
}

func siteScheduledWorkAllowed(siteID int) (bool, string, error) {
	var status string
	if err := database.GetDB().QueryRow(`SELECT status FROM websites WHERE id=?`, siteID).Scan(&status); err != nil {
		return false, "", err
	}
	if status != "active" {
		return false, status, nil
	}
	var locked int
	if err := database.GetDB().QueryRow(`SELECT EXISTS(SELECT 1 FROM site_migration_locks WHERE site_id=? AND status='active')`, siteID).Scan(&locked); err != nil {
		return false, "", err
	}
	return locked == 0, map[bool]string{true: "migration_locked", false: ""}[locked != 0], nil
}

// CronJobRuntimeSuspended reports whether an enabled job is temporarily held by its site's runtime state.
func CronJobRuntimeSuspended(jobID int) (bool, string, string, error) {
	job, _, err := loadCronJob(jobID)
	if err != nil {
		return false, "", "", err
	}
	siteID, domain, err := cronJobSite(job)
	if err != nil {
		return false, "", "", err
	}
	if siteID == 0 {
		return false, "", "", nil
	}
	allowed, reason, err := siteScheduledWorkAllowed(siteID)
	return !allowed, domain, reason, err
}

// RunScheduledCron is the short-lived system Cron entrypoint. It must remain before migrations in main.
func RunScheduledCron(jobID int) TaskResult {
	lock, err := acquireCronJobExecutionLock(jobID)
	if err != nil {
		if errors.Is(err, ErrCronJobAlreadyRunning) {
			appendCronMessage("SKIPPED", jobID, "任务仍在执行，本次自动触发已跳过")
			return TaskResult{Success: true, Message: "任务仍在执行，本次自动触发已跳过"}
		}
		message := "取得任务运行锁失败: " + err.Error()
		appendCronGateError(jobID, message)
		return TaskResult{Success: false, Message: message}
	}
	defer lock.Close()

	job, enabled, err := loadCronJob(jobID)
	if err != nil {
		message := "查询任务失败: " + err.Error()
		appendCronGateError(jobID, message)
		return TaskResult{Success: false, Message: message}
	}
	if !enabled {
		return TaskResult{Success: true, Message: "任务已禁用"}
	}
	siteID, _, err := cronJobSite(job)
	if err != nil {
		message := "解析任务网站失败: " + err.Error()
		appendCronGateError(jobID, message)
		return TaskResult{Success: false, Message: message}
	}
	if siteID > 0 {
		allowed, _, err := siteScheduledWorkAllowed(siteID)
		if err != nil {
			message := "检查网站运行状态失败: " + err.Error()
			appendCronGateError(jobID, message)
			return TaskResult{Success: false, Message: message}
		}
		if !allowed {
			return TaskResult{Success: true, Message: "网站当前不允许运行自动任务"}
		}
	}
	return runCronJob(jobID, true)
}

func runCronJob(jobID int, scheduled bool) TaskResult {
	db := database.GetDB()
	job, _, err := loadCronJob(jobID)
	if err != nil {
		log.Printf("查询任务失败: %v", err)
		return TaskResult{Success: false, Message: "查询任务失败"}
	}

	now := time.Now().Format("2006-01-02 15:04:05")

	var out string
	var execErr error
	ctx, cancel := context.WithTimeout(context.Background(), cronExecutionTimeout(job.taskType))
	defer cancel()

	if job.taskType == "file_backup" {
		if !job.siteID.Valid {
			return TaskResult{Success: false, Message: "关联网站已不存在"}
		}
		var msg string
		msg, execErr = executeCronFileBackup(ctx, int(job.siteID.Int64), job.backupMode, job.keepCount, scheduled)
		if errors.Is(execErr, errScheduledWorkNotAllowed) {
			return TaskResult{Success: true, Message: "网站当前不允许运行自动任务，文件备份已跳过"}
		}
		if execErr != nil {
			out = execErr.Error()
		} else {
			out = msg
		}
	} else if job.taskType == "wp_cron" {
		url := "https://" + job.command + "/wp-cron.php?doing_wp_cron"
		out, execErr = runCronCommand(ctx, "curl", "-k", "-s", "-o", "/dev/null", url)
	} else if job.runAsUser != "" {
		out, execErr = runCronCommand(ctx, "runuser", "-u", job.runAsUser, "--", "bash", "-c", job.command)
	} else {
		out, execErr = runCronCommand(ctx, "bash", "-c", job.command)
	}

	status := "success"
	if execErr != nil {
		status = "failed"
		if out == "" {
			out = execErr.Error()
		}
	}
	out = boundCronOutput(out)

	if scheduled {
		_, _ = db.Exec(
			`UPDATE cron_jobs SET last_run_at = ?, last_status = ?, last_output = ? WHERE id = ?`,
			now, status, out, jobID,
		)
	} else {
		_, _ = db.Exec(
			`UPDATE cron_jobs SET last_run_at = ?, last_status = ?, last_output = ?, running = 0 WHERE id = ?`,
			now, status, out, jobID,
		)
	}

	// Append to cron log file, keep the latest bounded history.
	f, err := os.OpenFile(cronLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		mode := "manual"
		if scheduled {
			mode = "scheduled"
		}
		f.WriteString(fmt.Sprintf("[%s] START %s (%s)\n", now, job.name, mode))
		f.WriteString(out + "\n")
		f.WriteString(fmt.Sprintf("[%s] END %s (exit:%d)\n", now, job.name, map[bool]int{true: 0, false: 1}[execErr == nil]))
		f.Close()
	}
	pruneCronLog(cronLogFile, cronLogKeepLines)

	return TaskResult{
		Success: execErr == nil,
		Message: fmt.Sprintf("任务 %s 执行%s", job.name, map[bool]string{true: "成功", false: "失败"}[execErr == nil]),
		Data:    map[string]interface{}{"output": out, "status": status, "run_at": now},
	}
}

func appendCronGateError(jobID int, message string) {
	appendCronMessage("GATE ERROR", jobID, message)
}

func appendCronMessage(kind string, jobID int, message string) {
	now := time.Now().Format("2006-01-02 15:04:05")
	f, err := os.OpenFile(cronLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "[%s] %s job_id=%d: %s\n", now, kind, jobID, message)
	_ = f.Close()
	pruneCronLog(cronLogFile, cronLogKeepLines)
}

type cronJobExecutionLock struct {
	file *os.File
}

type cronJobMutationLocks struct {
	locks []*cronJobExecutionLock
	once  sync.Once
	err   error
}

// AcquireCronJobMutationLocks prevents the selected jobs from starting while
// a handler changes or removes their database rows. IDs are acquired in sorted
// order so overlapping multi-job mutations have deterministic lock behavior.
// Acquisition is deliberately non-blocking: an active job is reported to the
// caller instead of making an HTTP mutation wait for a long-running backup.
func AcquireCronJobMutationLocks(jobIDs []int) (io.Closer, error) {
	unique := make(map[int]struct{}, len(jobIDs))
	ids := make([]int, 0, len(jobIDs))
	for _, id := range jobIDs {
		if id <= 0 {
			return nil, fmt.Errorf("invalid cron job ID %d", id)
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Ints(ids)

	group := &cronJobMutationLocks{locks: make([]*cronJobExecutionLock, 0, len(ids))}
	for _, id := range ids {
		lock, err := acquireCronJobExecutionLock(id)
		if err != nil {
			return nil, errors.Join(err, group.Close())
		}
		group.locks = append(group.locks, lock)
	}
	return group, nil
}

func (l *cronJobMutationLocks) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		for i := len(l.locks) - 1; i >= 0; i-- {
			l.err = errors.Join(l.err, l.locks[i].Close())
		}
		l.locks = nil
	})
	return l.err
}

func acquireCronJobExecutionLock(jobID int) (*cronJobExecutionLock, error) {
	if jobID <= 0 {
		return nil, errors.New("invalid cron job ID")
	}
	if err := ensureCronJobLockDirectory(cronJobLockDir); err != nil {
		return nil, err
	}
	path := filepath.Join(cronJobLockDir, fmt.Sprintf("yub-wpanel-cron-%d.lock", jobID))
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("inspect cron job lock: %w", err)
	}
	expectedUID := uint32(os.Geteuid())
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != expectedUID || stat.Nlink != 1 || stat.Mode&0o777 != 0o600 {
		_ = syscall.Close(fd)
		return nil, errors.New("cron job lock file identity is unsafe")
	}
	file := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrCronJobAlreadyRunning
		}
		return nil, err
	}
	return &cronJobExecutionLock{file: file}, nil
}

func ensureCronJobLockDirectory(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return errors.New("cron job lock directory is not absolute")
	}
	expectedUID := uint32(os.Geteuid())
	if path == canonicalCronJobLockDir {
		if os.Geteuid() != 0 {
			return errors.New("canonical cron job lock directory requires root")
		}
		if err := ensureOwnedPrivateDirectory(filepath.Dir(path), expectedUID); err != nil {
			return fmt.Errorf("secure cron runtime directory: %w", err)
		}
	}
	if err := ensureOwnedPrivateDirectory(path, expectedUID); err != nil {
		return fmt.Errorf("secure cron job lock directory: %w", err)
	}
	return nil
}

func ensureOwnedPrivateDirectory(path string, expectedUID uint32) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != expectedUID {
		return errors.New("directory identity is unsafe")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("directory permissions are %o, want 700", info.Mode().Perm())
	}
	return nil
}

func (l *cronJobExecutionLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func pruneCronLog(path string, keep int) {
	if keep <= 0 {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return
	}
	readSize := info.Size()
	if readSize > cronLogMaxBytes {
		readSize = cronLogMaxBytes
	}
	offset := info.Size() - readSize
	data := make([]byte, int(readSize))
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return
	}
	if _, err := io.ReadFull(f, data); err != nil {
		_ = f.Close()
		return
	}
	_ = f.Close()
	if offset > 0 {
		// The bounded tail normally begins in the middle of a log line. Drop that
		// fragment so the rewritten log never presents partial output as a full
		// event. If the oversized file has no newline in the retained tail, clear
		// it rather than allocating memory proportional to the original file.
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset == 0 && len(lines) <= keep {
		return
	}
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	_ = os.WriteFile(path, []byte(content), 0644)
}

// sanitizeCronArg escapes shell metacharacters for use inside double quotes in bash.
func sanitizeCronArg(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "$", "\\$")
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
