package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zangwp/yub-wpanel/executor"
)

func TestEnqueueTaskMapsAdmissionErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldQueue := executor.GlobalQueue
	t.Cleanup(func() { executor.GlobalQueue = oldQueue })

	t.Run("queue unavailable", func(t *testing.T) {
		executor.GlobalQueue = nil
		ctx, recorder := enqueueTaskTestContext(context.Background())
		if task, ok := enqueueTask(ctx, executor.TaskRenderCron, nil); ok || task != nil {
			t.Fatalf("enqueueTask = (%v, %v), want rejection", task, ok)
		}
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want %d", recorder.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("request canceled", func(t *testing.T) {
		executor.GlobalQueue = nil
		requestCtx, cancel := context.WithCancel(context.Background())
		cancel()
		ctx, recorder := enqueueTaskTestContext(requestCtx)
		if task, ok := enqueueTask(ctx, executor.TaskRenderCron, nil); ok || task != nil {
			t.Fatalf("enqueueTask = (%v, %v), want rejection", task, ok)
		}
		if recorder.Body.Len() != 0 {
			t.Fatalf("canceled request wrote a response: %s", recorder.Body.String())
		}
	})

	t.Run("request deadline", func(t *testing.T) {
		executor.GlobalQueue = nil
		requestCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		ctx, recorder := enqueueTaskTestContext(requestCtx)
		if task, ok := enqueueTask(ctx, executor.TaskRenderCron, nil); ok || task != nil {
			t.Fatalf("enqueueTask = (%v, %v), want rejection", task, ok)
		}
		if recorder.Code != http.StatusGatewayTimeout {
			t.Fatalf("status=%d, want %d; body=%s", recorder.Code, http.StatusGatewayTimeout, recorder.Body.String())
		}
	})
}

func enqueueTaskTestContext(requestCtx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil).WithContext(requestCtx)
	return ctx, recorder
}
