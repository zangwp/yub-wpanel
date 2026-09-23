package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/models"
)

func TestQueueStoresTaskResultForPolling(t *testing.T) {
	q := InitQueue(nil)
	t.Cleanup(q.StopCleanup)
	task := q.Enqueue(TaskType("unknown_for_test"), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := q.GetTask(task.ID)
		if !ok {
			t.Fatal("queued task not found")
		}
		if got.Result != nil {
			if got.Status != TaskStatusFailed {
				t.Fatalf("status = %q, want %q", got.Status, TaskStatusFailed)
			}
			if got.Result.Success {
				t.Fatal("result success = true, want false")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for queued task result")
}

func TestQueueClearsSensitiveExecutionStateAfterCompletion(t *testing.T) {
	q := InitQueue(nil)
	t.Cleanup(q.StopCleanup)
	task := q.Enqueue(TaskType("unknown_for_cleanup_test"), &RestoreBackupPayload{
		Site: &models.Website{ID: 42, Domain: "private.example"},
	})
	select {
	case <-task.ResultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task completion")
	}

	q.mu.Lock()
	stored := q.tasks[task.ID]
	if stored == nil {
		q.mu.Unlock()
		t.Fatal("completed polling snapshot missing")
	}
	if stored.Payload != nil || stored.ResultCh != nil {
		q.mu.Unlock()
		t.Fatalf("sensitive execution state retained: payload=%T resultCh=%v", stored.Payload, stored.ResultCh != nil)
	}
	q.mu.Unlock()

	snapshot, ok := q.GetTask(task.ID)
	if !ok || snapshot.SiteID != 42 || snapshot.Payload != nil || snapshot.ResultCh != nil {
		t.Fatalf("unexpected polling snapshot: ok=%v task=%+v", ok, snapshot)
	}
	if snapshot.Result == nil || snapshot.Result.Data != nil {
		t.Fatalf("polling result must be present and data-free: %+v", snapshot.Result)
	}
}

func TestQueueAdmissionIsBoundedAndLeavesNoOrphan(t *testing.T) {
	q := &TaskQueue{queue: make(chan *Task, 1), tasks: make(map[string]*Task), enqueueTimeout: 20 * time.Millisecond}
	first, err := q.EnqueueContext(context.Background(), TaskType("first"), nil)
	if err != nil || first == nil {
		t.Fatalf("first enqueue failed: task=%v err=%v", first, err)
	}

	started := time.Now()
	second, err := q.EnqueueContext(context.Background(), TaskType("second"), nil)
	if !errors.Is(err, ErrTaskQueueFull) || second != nil {
		t.Fatalf("second enqueue = (%v, %v), want queue full", second, err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("queue admission was not bounded: %v", elapsed)
	}
	if got := q.QueueLength(); got != 1 {
		t.Fatalf("queue length=%d, want only admitted task", got)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) != 1 || q.tasks[first.ID] == nil {
		t.Fatalf("failed enqueue left an orphan task: %#v", q.tasks)
	}
}

func TestQueueAdmissionWaitersAreBounded(t *testing.T) {
	q := &TaskQueue{
		queue:          make(chan *Task, 1),
		tasks:          make(map[string]*Task),
		admissionSlots: make(chan struct{}, 1),
		enqueueTimeout: time.Second,
	}
	if _, err := q.EnqueueContext(context.Background(), TaskType("queued"), nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := q.EnqueueContext(ctx, TaskType("waiter"), &ChangeDBPasswordPayload{NewPassword: "retained-only-while-bounded"})
		waiterDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	retained := 0
	for time.Now().Before(deadline) {
		q.mu.Lock()
		retained = len(q.tasks)
		q.mu.Unlock()
		if len(q.admissionSlots) == 1 && retained == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(q.admissionSlots) != 1 || retained != 2 {
		t.Fatalf("first admission waiter was not fully retained: slots=%d tasks=%d", len(q.admissionSlots), retained)
	}

	started := time.Now()
	if task, err := q.EnqueueContext(context.Background(), TaskType("rejected"), &EnableSSLPayload{PrivateKey: "must-not-be-retained"}); task != nil || !errors.Is(err, ErrTaskQueueFull) {
		t.Fatalf("overflow admission = (%v, %v), want ErrTaskQueueFull", task, err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("overflow admission did not fail immediately: %v", elapsed)
	}
	q.mu.Lock()
	retained = len(q.tasks)
	q.mu.Unlock()
	if retained != 2 {
		t.Fatalf("retained tasks=%d, want one queued task plus one bounded waiter", retained)
	}

	cancel()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error=%v, want context.Canceled", err)
	}
}

func TestQueueAvailableCapacityBypassesSaturatedAdmissionWaiters(t *testing.T) {
	q := &TaskQueue{
		queue:          make(chan *Task, taskQueueCapacity),
		tasks:          make(map[string]*Task),
		admissionSlots: make(chan struct{}, maxTaskAdmissionWaiters),
	}
	for i := 0; i < cap(q.admissionSlots); i++ {
		q.admissionSlots <- struct{}{}
	}

	const burst = 17
	start := make(chan struct{})
	errs := make(chan error, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := q.EnqueueContext(context.Background(), TaskType("capacity_available"), nil)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("enqueue with available capacity failed: %v", err)
		}
	}
	if got := len(q.queue); got != burst {
		t.Fatalf("queued tasks=%d, want %d", got, burst)
	}
	if got := q.QueueLength(); got != burst {
		t.Fatalf("queue length=%d, want %d", got, burst)
	}
}

func TestQueuePrunesExpiredAndExcessCompletedSnapshots(t *testing.T) {
	q := &TaskQueue{tasks: make(map[string]*Task)}
	now := time.Now()
	q.tasks["expired"] = &Task{ID: "expired", Status: TaskStatusSuccess, UpdatedAt: now.Add(-completedTaskTTL)}
	for i := 0; i < maxCompletedTaskRecords+3; i++ {
		id := string(rune(i + 1))
		q.tasks[id] = &Task{ID: id, Status: TaskStatusFailed, UpdatedAt: now.Add(time.Duration(i) * time.Second)}
	}
	q.tasks["running"] = &Task{ID: "running", Status: TaskStatusRunning, UpdatedAt: now.Add(-24 * time.Hour)}

	q.pruneCompletedLocked(now)
	if _, ok := q.tasks["expired"]; ok {
		t.Fatal("expired completed snapshot was retained")
	}
	if _, ok := q.tasks["running"]; !ok {
		t.Fatal("active task must never be pruned")
	}
	if got := len(q.tasks); got != maxCompletedTaskRecords+1 {
		t.Fatalf("retained task count=%d, want %d completed + active", got, maxCompletedTaskRecords+1)
	}
}

func TestQueueCleanupJanitorPrunesWhileIdleAndStops(t *testing.T) {
	q := &TaskQueue{tasks: make(map[string]*Task)}
	q.tasks["expired"] = &Task{
		ID:        "expired",
		Status:    TaskStatusSuccess,
		UpdatedAt: time.Now().Add(-completedTaskTTL - time.Minute),
	}
	q.startCleanup(5 * time.Millisecond)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		_, exists := q.tasks["expired"]
		q.mu.Unlock()
		if !exists {
			q.StopCleanup()
			select {
			case <-q.cleanupDone:
				return
			default:
				t.Fatal("cleanup janitor did not stop")
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.StopCleanup()
	t.Fatal("cleanup janitor did not prune an expired snapshot while idle")
}

func TestSanitizedTaskResultRedactsSecretsAndDropsData(t *testing.T) {
	secret := "super-secret-password"
	got := sanitizedTaskResult(&ChangeDBPasswordPayload{NewPassword: secret}, TaskResult{
		Success: true,
		Message: "changed " + secret,
		Data:    map[string]string{"password": secret},
	})
	if got.Data != nil || got.Message != "changed [REDACTED]" {
		t.Fatalf("unsafe result snapshot: %+v", got)
	}
}

func TestFinalizeTaskToleratesUnusableResultChannel(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(chan TaskResult)
	}{
		{
			name: "closed",
			prepare: func(ch chan TaskResult) {
				close(ch)
			},
		},
		{
			name: "full",
			prepare: func(ch chan TaskResult) {
				ch <- TaskResult{Success: false, Message: "external value"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &TaskQueue{tasks: make(map[string]*Task), taskCount: 1}
			q.running.Store(true)
			task := newTask(TaskType("finalize_delivery_test"), &ChangeDBPasswordPayload{NewPassword: "secret"})
			tt.prepare(task.ResultCh)
			q.tasks[task.ID] = task

			done := make(chan struct{})
			go func() {
				q.finalizeTask(task, TaskResult{Success: true, Message: "done"})
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("finalizeTask blocked on an unusable result channel")
			}

			got, ok := q.GetTask(task.ID)
			if !ok || got.Status != TaskStatusSuccess || got.Result == nil || !got.Result.Success {
				t.Fatalf("completed state was not retained: ok=%v task=%+v", ok, got)
			}
			if got.Payload != nil || got.ResultCh != nil {
				t.Fatalf("execution state was retained: payload=%T resultCh=%v", got.Payload, got.ResultCh != nil)
			}
			if q.QueueLength() != 0 || q.IsRunning() {
				t.Fatalf("queue did not finish cleanly: count=%d running=%v", q.QueueLength(), q.IsRunning())
			}
		})
	}
}
