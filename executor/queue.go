package executor

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zangwp/yub-wpanel/config"

	"github.com/google/uuid"
)

type TaskQueue struct {
	queue           chan *Task
	running         atomic.Bool
	mu              sync.Mutex
	taskCount       int
	tasks           map[string]*Task
	cleanupStop     chan struct{}
	cleanupDone     chan struct{}
	cleanupStopOnce sync.Once
	admissionOnce   sync.Once
	admissionSlots  chan struct{}
	enqueueTimeout  time.Duration
}

const (
	taskQueueCapacity       = 100
	taskEnqueueTimeout      = 500 * time.Millisecond
	completedTaskTTL        = 30 * time.Minute
	taskCleanupInterval     = time.Minute
	maxTaskAdmissionWaiters = 16
	maxCompletedTaskRecords = 256
	maxTaskResultMessageLen = 4096
)

var (
	ErrTaskQueueFull        = errors.New("task queue is full")
	ErrTaskQueueUnavailable = errors.New("task queue is unavailable")
)

var GlobalQueue *TaskQueue

func InitQueue(cfg *config.Config) *TaskQueue {
	q := &TaskQueue{
		queue: make(chan *Task, taskQueueCapacity),
		tasks: make(map[string]*Task),
	}
	GlobalQueue = q
	go q.worker()
	q.startCleanup(taskCleanupInterval)
	log.Println("任务队列已启动(单线程串行模式)")
	return q
}

func (q *TaskQueue) startCleanup(interval time.Duration) {
	if q == nil || interval <= 0 {
		return
	}
	q.cleanupStop = make(chan struct{})
	q.cleanupDone = make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(q.cleanupDone)
		for {
			select {
			case now := <-ticker.C:
				q.mu.Lock()
				q.pruneCompletedLocked(now)
				q.mu.Unlock()
			case <-q.cleanupStop:
				return
			}
		}
	}()
}

// StopCleanup stops the retention janitor. It is primarily useful for clean
// process shutdown and deterministic tests; task execution continues.
func (q *TaskQueue) StopCleanup() {
	if q == nil || q.cleanupStop == nil {
		return
	}
	q.cleanupStopOnce.Do(func() { close(q.cleanupStop) })
	<-q.cleanupDone
}

// Enqueue 把任务交给单 worker 串行执行。
// 注意：任务执行函数内部不要 Enqueue 另一个任务后同步等待 ResultCh；
// 单 worker 无法继续处理后续任务，会造成队列自等待死锁。需要复用逻辑时应抽成普通函数直接调用。
func newTask(taskType TaskType, payload interface{}) *Task {
	now := time.Now()
	return &Task{
		ID:        uuid.New().String(),
		Type:      taskType,
		SiteID:    taskSiteID(payload),
		Payload:   payload,
		Status:    TaskStatusWaiting,
		CreatedAt: now,
		UpdatedAt: now,
		ResultCh:  make(chan TaskResult, 1),
	}
}

func (q *TaskQueue) admissionSemaphore() chan struct{} {
	q.admissionOnce.Do(func() {
		if q.admissionSlots == nil {
			q.admissionSlots = make(chan struct{}, maxTaskAdmissionWaiters)
		}
	})
	return q.admissionSlots
}

// EnqueueContext waits for at most taskEnqueueTimeout for queue admission. The
// returned Task is an immutable caller handle: worker state must be read through
// GetTask, which avoids races with Status updates.
func (q *TaskQueue) EnqueueContext(ctx context.Context, taskType TaskType, payload interface{}) (*Task, error) {
	if q == nil || q.queue == nil {
		return nil, ErrTaskQueueUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	task := newTask(taskType, payload)
	handle := *task
	handle.Payload = nil
	handle.Result = nil

	// Keep the uncongested path independent from the waiter semaphore. A burst
	// that still fits in the queue must not be rejected merely because other
	// goroutines are between their capacity check and send. Registration and
	// the non-blocking send happen while holding q.mu so the worker cannot
	// observe the task before its polling record exists.
	if q.tryAdmitTask(task) {
		return &handle, nil
	}

	// Queue capacity bounds admitted work; this separate semaphore bounds only
	// callers that actually have to wait for capacity. Do not retain the task or
	// its payload until this slot has been acquired, otherwise a concurrent
	// request spike could briefly create an unbounded task map even though only
	// a bounded number of callers are allowed to wait.
	admissionSlots := q.admissionSemaphore()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case admissionSlots <- struct{}{}:
		defer func() { <-admissionSlots }()
	default:
		// Capacity may have opened after the first fast-path attempt. Give it one
		// final chance before rejecting the request because all waiter slots are
		// occupied.
		if q.tryAdmitTask(task) {
			return &handle, nil
		}
		return nil, ErrTaskQueueFull
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	q.pruneCompletedLocked(time.Now())
	q.taskCount++
	q.tasks[task.ID] = task
	q.mu.Unlock()

	enqueueTimeout := q.enqueueTimeout
	if enqueueTimeout <= 0 {
		enqueueTimeout = taskEnqueueTimeout
	}
	timer := time.NewTimer(enqueueTimeout)
	defer timer.Stop()
	select {
	case q.queue <- task:
	case <-ctx.Done():
		q.removeUnqueuedTask(task.ID)
		return nil, ctx.Err()
	case <-timer.C:
		q.removeUnqueuedTask(task.ID)
		return nil, ErrTaskQueueFull
	}

	return &handle, nil
}

func (q *TaskQueue) tryAdmitTask(task *Task) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneCompletedLocked(time.Now())
	q.taskCount++
	q.tasks[task.ID] = task
	select {
	case q.queue <- task:
		return true
	default:
		delete(q.tasks, task.ID)
		q.taskCount--
		return false
	}
}

func (q *TaskQueue) removeUnqueuedTask(id string) {
	q.mu.Lock()
	if _, ok := q.tasks[id]; ok {
		delete(q.tasks, id)
		if q.taskCount > 0 {
			q.taskCount--
		}
	}
	q.mu.Unlock()
}

// Enqueue preserves the existing API for background jobs. HTTP handlers should
// use EnqueueContext so admission failures can be returned as 503 responses.
func (q *TaskQueue) Enqueue(taskType TaskType, payload interface{}) *Task {
	task, err := q.EnqueueContext(context.Background(), taskType, payload)
	if err == nil {
		return task
	}
	result := TaskResult{Success: false, Message: "任务队列繁忙，请稍后重试"}
	failedTask := &Task{
		ID:        uuid.New().String(),
		Type:      taskType,
		SiteID:    taskSiteID(payload),
		Status:    TaskStatusFailed,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Result:    &result,
		ResultCh:  make(chan TaskResult, 1),
	}
	failedTask.ResultCh <- result
	close(failedTask.ResultCh)
	return failedTask
}

func (q *TaskQueue) GetTask(id string) (*Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneCompletedLocked(time.Now())
	task, ok := q.tasks[id]
	if !ok {
		return nil, false
	}
	copyTask := *task
	copyTask.Payload = nil
	if task.Result != nil {
		result := *task.Result
		copyTask.Result = &result
	}
	copyTask.ResultCh = nil
	return &copyTask, true
}

func (q *TaskQueue) pruneCompletedLocked(now time.Time) {
	type completedTask struct {
		id        string
		updatedAt time.Time
	}
	completed := make([]completedTask, 0)
	for id, task := range q.tasks {
		if task.Status != TaskStatusSuccess && task.Status != TaskStatusFailed {
			continue
		}
		if now.Sub(task.UpdatedAt) >= completedTaskTTL {
			delete(q.tasks, id)
			continue
		}
		completed = append(completed, completedTask{id: id, updatedAt: task.UpdatedAt})
	}
	if len(completed) <= maxCompletedTaskRecords {
		return
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].updatedAt.Before(completed[j].updatedAt) })
	for _, item := range completed[:len(completed)-maxCompletedTaskRecords] {
		delete(q.tasks, item.id)
	}
}

func (q *TaskQueue) QueueLength() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.taskCount
}

func (q *TaskQueue) IsRunning() bool {
	return q.running.Load()
}

func (q *TaskQueue) worker() {
	for task := range q.queue {
		func() {
			q.running.Store(true)
			q.mu.Lock()
			task.Status = TaskStatusRunning
			task.UpdatedAt = time.Now()
			q.mu.Unlock()

			var result TaskResult
			finalized := false
			defer func() {
				if r := recover(); r != nil {
					log.Printf("task %s panic: %v", task.ID, r)
					if finalized {
						return
					}
					result = TaskResult{Success: false, Message: fmt.Sprintf("task execution panic: %v", r)}
					q.finalizeTask(task, result)
				}
			}()
			switch task.Type {
			case TaskCreateSite:
				result = executeCreateSite(task)
			case TaskDeleteSite:
				result = executeDeleteSite(task)
			case TaskPauseSite:
				result = executePauseSite(task)
			case TaskEnableSite:
				result = executeEnableSite(task)
			case TaskRefreshWhitelist:
				result = executeRefreshWhitelist(task)
			case TaskUnbanIP:
				result = executeUnbanIP(task)
			case TaskEnableSSL:
				result = executeEnableSSL(task)
			case TaskRemoveSSL:
				result = executeRemoveSSL(task)
			case TaskChangeDBPassword:
				result = executeChangeDBPassword(task)
			case TaskUpdateDomains:
				result = executeUpdateDomains(task)
			case TaskSaveNginxCustom:
				result = executeSaveNginxCustom(task)
			case TaskSetAccessLogMode:
				result = executeSetAccessLogMode(task)
			case TaskSetCDNRealIP:
				result = executeSetCDNRealIP(task)
			case TaskSetDocumentRoot:
				result = executeSetDocumentRoot(task)
			case TaskRenewSSL:
				result = executeRenewSSL(task)
			case TaskRenderCron:
				result = executeRenderCron(task)
			case TaskRunCron:
				result = executeRunCron(task)
			case TaskManualBan:
				result = executeManualBan(task)
			case TaskCreateBackup:
				result = executeCreateBackup(task)
			case TaskRestoreBackup:
				result = executeRestoreBackup(task)
			case TaskSetFileLock:
				result = executeSetFileLock(task)
			default:
				result = TaskResult{Success: false, Message: "未知任务类型: " + string(task.Type)}
			}

			finalized = true
			q.finalizeTask(task, result)
		}()
	}
}

func (q *TaskQueue) finalizeTask(task *Task, result TaskResult) {
	defer q.running.Store(false)

	// Logging still needs the payload to identify the target, so do it before
	// clearing sensitive execution state.
	safeResult := sanitizedTaskResult(task.Payload, result)
	safeLogOp(task, safeResult)

	q.mu.Lock()
	resultCh := task.ResultCh
	if result.Success {
		task.Status = TaskStatusSuccess
	} else {
		task.Status = TaskStatusFailed
	}
	task.UpdatedAt = time.Now()
	task.Result = &safeResult
	task.Payload = nil
	task.ResultCh = nil
	if q.taskCount > 0 {
		q.taskCount--
	}
	q.pruneCompletedLocked(task.UpdatedAt)
	q.mu.Unlock()

	deliverTaskResult(task.ID, resultCh, result)
}

func deliverTaskResult(taskID string, resultCh chan TaskResult, result TaskResult) {
	if resultCh == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("deliver result for task %s skipped: %v", taskID, r)
		}
	}()
	select {
	case resultCh <- result:
	default:
		log.Printf("deliver result for task %s skipped: result channel is full", taskID)
	}
	close(resultCh)
}

func sanitizedTaskResult(payload interface{}, result TaskResult) TaskResult {
	message := result.Message
	for _, secret := range taskSecrets(payload) {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > maxTaskResultMessageLen {
		message = message[:maxTaskResultMessageLen] + "…"
	}
	return TaskResult{Success: result.Success, Message: message}
}

func taskSecrets(payload interface{}) []string {
	switch p := payload.(type) {
	case *CreateSitePayload:
		if p == nil {
			return nil
		}
		return []string{p.DBPassword}
	case *EnableSSLPayload:
		if p == nil {
			return nil
		}
		return []string{p.PrivateKey}
	case *ChangeDBPasswordPayload:
		if p == nil {
			return nil
		}
		return []string{p.NewPassword}
	}
	return nil
}

func taskSiteID(payload interface{}) int {
	switch p := payload.(type) {
	case *RestoreBackupPayload:
		if p != nil && p.Site != nil {
			return p.Site.ID
		}
	case *CreateBackupPayload:
		if p != nil && p.Site != nil {
			return p.Site.ID
		}
	}
	return 0
}

func safeLogOp(task *Task, result TaskResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("record operation log for task %s panic: %v", task.ID, r)
		}
	}()
	logOp(task, result)
}

func logOp(task *Task, result TaskResult) {
	status := "success"
	if !result.Success {
		status = "failed"
	}
	target := ""
	switch task.Type {
	case TaskCreateSite:
		if p, ok := task.Payload.(*CreateSitePayload); ok {
			target = p.Domain
		}
	case TaskDeleteSite:
		if p, ok := task.Payload.(*DeleteSitePayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskPauseSite:
		if p, ok := task.Payload.(*PauseSitePayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskEnableSite:
		if p, ok := task.Payload.(*EnableSitePayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskEnableSSL:
		if p, ok := task.Payload.(*EnableSSLPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskRemoveSSL:
		if p, ok := task.Payload.(*RemoveSSLPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskChangeDBPassword:
		if p, ok := task.Payload.(*ChangeDBPasswordPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskUpdateDomains:
		if p, ok := task.Payload.(*UpdateDomainsPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskSaveNginxCustom:
		if p, ok := task.Payload.(*SaveNginxCustomPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskSetAccessLogMode:
		if p, ok := task.Payload.(*SetAccessLogModePayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskSetCDNRealIP:
		if p, ok := task.Payload.(*SetCDNRealIPPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskSetDocumentRoot:
		if p, ok := task.Payload.(*SetDocumentRootPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskRenewSSL:
		target = "ssl_renewal"
	case TaskManualBan:
		if p, ok := task.Payload.(*ManualBanPayload); ok {
			target = p.IP
		}
	case TaskRenderCron:
		target = "cron_config"
	case TaskRunCron:
		if p, ok := task.Payload.(*RunCronPayload); ok {
			target = p.Name
		}
	case TaskCreateBackup:
		if p, ok := task.Payload.(*CreateBackupPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskRestoreBackup:
		if p, ok := task.Payload.(*RestoreBackupPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	case TaskSetFileLock:
		if p, ok := task.Payload.(*SetFileLockPayload); ok && p.Site != nil {
			target = p.Site.Domain
		}
	}

	recordOperationLog(string(task.Type), target, status, result.Message)
}

func buildSiteName(domain string) string {
	normalized := strings.TrimSpace(domain)
	normalized = strings.TrimSuffix(normalized, ".")
	normalized = strings.ToLower(normalized)
	sum := sha1.Sum([]byte(normalized))
	suffix := hex.EncodeToString(sum[:])[:8]

	var b strings.Builder
	lastUnderscore := false
	for _, c := range normalized {
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if isAlphaNum {
			b.WriteRune(c)
			lastUnderscore = false
		} else if c == '.' || c == '-' {
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}

	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "site"
	}

	const maxSiteNameLen = 27
	const hashSeparatorLen = 1
	maxReadableLen := maxSiteNameLen - hashSeparatorLen - len(suffix)
	if len(name) > maxReadableLen {
		name = strings.TrimRight(name[:maxReadableLen], "_")
		if name == "" {
			name = "site"
		}
	}

	return name + "_" + suffix
}

func fileExists(path string) bool {
	_, err := executeCommand("test", "-f", path)
	return err == nil
}

func dirExists(path string) bool {
	_, err := executeCommand("test", "-d", path)
	return err == nil
}

var shellExec = func(binary string, args ...string) (string, error) {
	result, err := Execute(binary, args...)
	if err != nil {
		if result != nil && result.Stderr != "" {
			log.Printf("命令 %s stderr: %s", binary, result.Stderr)
		}
		return "", fmt.Errorf("命令 %s 执行失败", binary)
	}
	return result.Stdout, nil
}

func executeCommand(binary string, args ...string) (string, error) {
	return shellExec(binary, args...)
}
