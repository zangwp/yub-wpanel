package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/models"

	"github.com/gin-gonic/gin"
)

type UpdateHandler struct {
	CurrentVersion string
	ConfigPath     string
	Config         *config.Config
}

func getGithubProxy() string {
	var v string
	database.GetDB().QueryRow("SELECT svalue FROM security_settings WHERE skey = 'github_proxy'").Scan(&v)
	return v
}

func (h *UpdateHandler) Check(c *gin.Context) {
	latest, err := executor.FetchLatestPanelRelease(getGithubProxy())
	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"current_version": h.CurrentVersion,
			"latest_version":  "",
			"has_update":      false,
			"error":           "获取版本信息失败",
		}))
		return
	}

	hasUpdate := executor.CompareVersions(latest.TagName, h.CurrentVersion) > 0
	notes := latest.Body
	if idx := strings.Index(notes, "**Full Changelog**"); idx >= 0 {
		notes = strings.TrimSpace(notes[:idx])
	}
	if notes == "" {
		notes = "（无更新说明）"
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"current_version": h.CurrentVersion,
		"latest_version":  latest.TagName,
		"release_notes":   notes,
		"has_update":      hasUpdate,
	}))
}

func (h *UpdateHandler) Status(c *gin.Context) {
	c.JSON(http.StatusOK, models.SuccessResponse(executor.SnapshotPanelUpdateStatus()))
}

func (h *UpdateHandler) Update(c *gin.Context) {
	latest, err := executor.ExecutePanelUpdate(executor.PanelUpdateOptions{
		Trigger:        "manual",
		CurrentVersion: h.CurrentVersion,
		Proxy:          getGithubProxy(),
		ConfigPath:     h.ConfigPath,
		Config:         h.Config,
		UseWatchdog:    true,
	})
	if err != nil {
		code := http.StatusInternalServerError
		if strings.Contains(err.Error(), "已有更新任务") {
			code = http.StatusConflict
		} else if strings.Contains(err.Error(), "已经是最新版本") || strings.Contains(err.Error(), "仅支持 Linux") {
			code = http.StatusBadRequest
		}
		c.JSON(code, models.ErrorResponse(err.Error()))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": fmt.Sprintf("正在更新到 %s，面板即将重启并执行健康检查...", latest.TagName),
	}))
}
