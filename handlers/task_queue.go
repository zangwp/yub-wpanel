package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/models"
)

// enqueueTask gives HTTP requests a bounded queue-admission path. It prevents a
// full serialized executor queue from pinning request goroutines indefinitely.
func enqueueTask(c *gin.Context, taskType executor.TaskType, payload interface{}) (*executor.Task, bool) {
	if c == nil || c.Request == nil {
		return nil, false
	}
	task, err := executor.GlobalQueue.EnqueueContext(c.Request.Context(), taskType, payload)
	if err != nil {
		requestErr := c.Request.Context().Err()
		switch {
		case errors.Is(requestErr, context.Canceled), errors.Is(err, context.Canceled):
			// The peer has gone away, so there is no useful response to write.
		case errors.Is(requestErr, context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
			c.JSON(http.StatusGatewayTimeout, models.ErrorResponse("请求已超时，请稍后重试"))
		case errors.Is(err, executor.ErrTaskQueueFull), errors.Is(err, executor.ErrTaskQueueUnavailable):
			c.JSON(http.StatusServiceUnavailable, models.ErrorResponse("任务队列繁忙，请稍后重试"))
		default:
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("任务入队失败"))
		}
		return nil, false
	}
	return task, true
}
