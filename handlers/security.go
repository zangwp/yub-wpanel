package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/models"

	"github.com/gin-gonic/gin"
)

type SecurityHandler struct{}

var securitySettingsApplyMu sync.Mutex
var errInvalidTelemetrySettings = errors.New("启用遥测时必须配置有效的公网 HTTPS 地址")

var (
	applyFail2banSettings             = executor.ApplyFail2banSettings
	applyRateLimitSettings            = executor.ApplyRateLimitSettings
	ensureLogMap                      = executor.EnsureLogMap
	regenerateAllSitesNginx           = executor.RegenerateAllSitesNginx
	websiteIDsForCDNRealIPGroup       = executor.WebsiteIDsForCDNRealIPGroup
	restoreCDNRealIPGroupWithBindings = executor.RestoreCDNRealIPGroupWithBindings
	withFail2banSettingsLock          = executor.WithFail2banSettingsLock
	enqueueOfficialWhitelistRefresh   = func(ctx context.Context) error {
		_, err := executor.GlobalQueue.EnqueueContext(ctx, executor.TaskRefreshWhitelist, nil)
		return err
	}
)

func (h *SecurityHandler) GetSettings(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query("SELECT id, skey, svalue, description, updated_at FROM security_settings")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("查询失败"))
		return
	}
	defer rows.Close()

	var settings []models.SecuritySetting
	for rows.Next() {
		var s models.SecuritySetting
		if err := rows.Scan(&s.ID, &s.Key, &s.Value, &s.Description, &s.UpdatedAt); err != nil {
			continue
		}
		if s.Key == "fail2ban_bantime" {
			s.Value = "600"
		}
		settings = append(settings, s)
	}
	if settings == nil {
		settings = []models.SecuritySetting{}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(settings))
}

func (h *SecurityHandler) UpdateSettings(c *gin.Context) {
	var raw map[string]interface{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}

	db := database.GetDB()

	normalized := make(map[string]string)
	for key, val := range raw {
		strVal, ok, err := normalizeSecuritySetting(key, val)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
			return
		}
		if !ok {
			continue
		}
		normalized[key] = strVal
	}

	if err := applySecuritySettings(db, normalized); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errInvalidTelemetrySettings) {
			status = http.StatusBadRequest
		}
		c.JSON(status, models.ErrorResponse(err.Error()))
		return
	}
	if _, enabledChanged := normalized["telemetry_enabled"]; enabledChanged {
		executor.NotifyTelemetrySettingsChanged()
	} else if _, endpointChanged := normalized["telemetry_url"]; endpointChanged {
		executor.NotifyTelemetrySettingsChanged()
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "安全设置已更新"}))
}

var sqliSettingKeys = []string{
	"wp_sqli_block_enabled", "wp_sqli_autoban_enabled",
	"wp_sqli_ban_threshold", "wp_sqli_ban_window_seconds",
}

func hasSQLiSettings(settings map[string]string) bool {
	for _, key := range sqliSettingKeys {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

func applySecuritySettings(db *sql.DB, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	securitySettingsApplyMu.Lock()
	defer securitySettingsApplyMu.Unlock()
	if hasSQLiSettings(settings) || needsFail2banApply(settings) {
		return withFail2banSettingsLock(func(apply func() error) error {
			return applySecuritySettingsLocked(db, settings, apply)
		})
	}
	return applySecuritySettingsLocked(db, settings, applyFail2banSettings)
}

func applySecuritySettingsLocked(db *sql.DB, settings map[string]string, applyFail2ban func() error) error {
	if err := validateTelemetrySettingsUpdate(db, settings); err != nil {
		return err
	}

	old := make(map[string]string, len(settings))
	for key := range settings {
		var value string
		if err := db.QueryRow(`SELECT svalue FROM security_settings WHERE skey=?`, key).Scan(&value); err != nil {
			return fmt.Errorf("读取安全设置失败")
		}
		old[key] = value
	}
	if err := writeSecuritySettings(db, settings); err != nil {
		return fmt.Errorf("安全设置保存失败")
	}
	applyErr := applySecuritySettingsRuntime(settings, applyFail2ban)
	if applyErr == nil {
		return nil
	}
	if rollbackErr := writeSecuritySettings(db, old); rollbackErr != nil {
		return fmt.Errorf("安全设置应用失败，数据库回滚失败: %v；原始错误: %v", rollbackErr, applyErr)
	}
	if rollbackRuntimeErr := restoreSecuritySettingsRuntime(settings, applyFail2ban); rollbackRuntimeErr != nil {
		return fmt.Errorf("安全设置应用失败，设置已回滚但服务器配置恢复不完整: %v；原始错误: %v", rollbackRuntimeErr, applyErr)
	}
	return fmt.Errorf("安全设置应用失败，已恢复修改前设置: %v", applyErr)
}

func validateTelemetrySettingsUpdate(db *sql.DB, settings map[string]string) error {
	enabled, enabledChanged := settings["telemetry_enabled"]
	endpoint, endpointChanged := settings["telemetry_url"]
	if !enabledChanged && !endpointChanged {
		return nil
	}

	if !enabledChanged {
		if err := db.QueryRow(`SELECT svalue FROM security_settings WHERE skey='telemetry_enabled'`).Scan(&enabled); err != nil {
			return fmt.Errorf("读取安全设置失败")
		}
	}
	if enabled != "true" {
		return nil
	}

	if !endpointChanged {
		if err := db.QueryRow(`SELECT svalue FROM security_settings WHERE skey='telemetry_url'`).Scan(&endpoint); err != nil {
			return fmt.Errorf("读取安全设置失败")
		}
	}
	if endpoint == "" {
		return errInvalidTelemetrySettings
	}
	// Revalidate the complete effective state while holding the save lock. This
	// also protects direct internal callers and legacy stored values.
	if _, err := executor.NormalizeTelemetryURL(endpoint); err != nil {
		return errInvalidTelemetrySettings
	}
	return nil
}

func restoreSecuritySettingsRuntime(settings map[string]string, applyFail2ban func() error) error {
	var restoreErrors []error
	if _, ok := settings["wp_security_log_whitelist"]; ok {
		if err := ensureLogMap(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("Nginx 日志规则恢复失败: %w", err))
		}
	}
	if hasSQLiSettings(settings) {
		if err := regenerateAllSitesNginx(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("SQL 注入防护恢复失败: %w", err))
		}
	}
	if hasSQLiSettings(settings) || needsFail2banApply(settings) {
		if err := applyFail2ban(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("Fail2ban 配置恢复失败: %w", err))
		}
	}
	if needsRateLimitApply(settings) {
		if err := applyRateLimitSettings(); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("Nginx 限速配置恢复失败: %w", err))
		}
	}
	return errors.Join(restoreErrors...)
}

func writeSecuritySettings(db *sql.DB, settings map[string]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for key, value := range settings {
		if _, err := tx.Exec(`UPDATE security_settings SET svalue=?,updated_at=CURRENT_TIMESTAMP WHERE skey=?`, value, key); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func applySecuritySettingsRuntime(settings map[string]string, applyFail2ban func() error) error {
	if _, ok := settings["wp_security_log_whitelist"]; ok {
		if err := ensureLogMap(); err != nil {
			return fmt.Errorf("Nginx 日志规则应用失败: %w", err)
		}
	}
	if hasSQLiSettings(settings) {
		if err := regenerateAllSitesNginx(); err != nil {
			return fmt.Errorf("SQL 注入防护应用失败: %w", err)
		}
	}
	if hasSQLiSettings(settings) || needsFail2banApply(settings) {
		if err := applyFail2ban(); err != nil {
			return fmt.Errorf("Fail2ban 配置应用失败: %w", err)
		}
	}
	if needsRateLimitApply(settings) {
		if err := applyRateLimitSettings(); err != nil {
			return fmt.Errorf("Nginx 限速配置应用失败: %w", err)
		}
	}
	return nil
}

func needsFail2banApply(settings map[string]string) bool {
	for _, key := range []string{"fail2ban_maxretry", "fail2ban_findtime", "auto_whitelist_enabled", "whitelist_ips"} {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

func needsRateLimitApply(settings map[string]string) bool {
	for _, key := range []string{"rate_limit_enabled", "rate_limit_rpm", "rate_limit_burst", "bot_limit_enabled", "bot_limit_rpm", "bot_limit_burst"} {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

func (h *SecurityHandler) RefreshWhitelist(c *gin.Context) {
	if err := enqueueOfficialWhitelistRefresh(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, models.ErrorResponse("任务队列繁忙，请稍后重试"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "白名单刷新任务已提交"}))
}

func (h *SecurityHandler) ImportGooglebotRanges(c *gin.Context) {
	var req struct {
		IPRanges string `json:"ip_ranges"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}
	ips, err := executor.NormalizeOfficialIPRanges(req.IPRanges)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Googlebot IP 段格式不正确: "+err.Error()))
		return
	}
	db := database.GetDB()
	err = withFail2banSettingsLock(func(apply func() error) error {
		keys := []string{"googlebot_ips", "googlebot_ips_source", "googlebot_ips_last_success_at", "googlebot_ips_last_error", "official_whitelist_ips"}
		old, err := snapshotSecuritySettings(db, keys)
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取现有 Googlebot 白名单失败: "+err.Error())
		}
		cloudflareRaw, err := readSecuritySettingValue(db, "cloudflare_realip_ips")
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取 Cloudflare 白名单失败: "+err.Error())
		}
		bingRaw, err := readSecuritySettingValue(db, "bingbot_ips")
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取 Bingbot 白名单失败: "+err.Error())
		}

		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		googleRaw := strings.Join(ips, "\n")
		official := strings.TrimSpace(strings.Join([]string{cloudflareRaw, googleRaw, bingRaw}, "\n"))
		updates := map[string]string{
			"googlebot_ips":                 googleRaw,
			"googlebot_ips_source":          "manual",
			"googlebot_ips_last_success_at": now,
			"googlebot_ips_last_error":      "",
			"official_whitelist_ips":        official,
		}
		if err := writeImportedGooglebotSettings(db, updates); err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "保存 Googlebot 白名单失败: "+err.Error())
		}

		rollback := func(cause error) error {
			if restoreErr := restoreSecuritySettings(db, old); restoreErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "Googlebot 白名单已保存，但应用失败，且数据库回滚失败: "+restoreErr.Error()+"；原始错误: "+cause.Error())
			}
			if runtimeErr := reapplyImportedGooglebotRuntime(apply); runtimeErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "Googlebot 白名单未生效，数据库已回滚，但服务器配置恢复失败: "+runtimeErr.Error()+"；原始错误: "+cause.Error())
			}
			return cause
		}

		if err := apply(); err != nil {
			return rollback(newSecurityHandlerError(http.StatusInternalServerError, "Fail2ban 白名单应用失败，已回滚: "+err.Error()))
		}
		if err := ensureLogMap(); err != nil {
			return rollback(newSecurityHandlerError(http.StatusInternalServerError, "搜索引擎验证规则应用失败，已回滚: "+err.Error()))
		}
		return nil
	})
	if err != nil {
		respondSecurityHandlerError(c, err, "导入 Googlebot 白名单失败")
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": fmt.Sprintf("已导入 %d 条 Googlebot 官方 IP 段", len(ips))}))
}

func (h *SecurityHandler) ListCDNRealIPGroups(c *gin.Context) {
	groups, err := executor.ListCDNRealIPGroups()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("查询 CDN 配置组失败"))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(groups))
}

func (h *SecurityHandler) CreateCDNRealIPGroup(c *gin.Context) {
	var req struct {
		Name        string `json:"name"`
		HeaderName  string `json:"header_name"`
		IPRanges    string `json:"ip_ranges"`
		Enabled     *bool  `json:"enabled"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}

	name, header, ranges, enabled, desc, err := normalizeCDNRealIPGroupPayload(req.Name, req.HeaderName, req.IPRanges, req.Enabled, req.Description)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	err = withFail2banSettingsLock(func(apply func() error) error {
		var id int64
		if err := database.GetDB().QueryRow(`INSERT INTO cdn_realip_groups (name, provider, header_name, ip_ranges, builtin, enabled, description)
			VALUES (?, 'custom', ?, ?, 0, ?, ?) RETURNING id`, name, header, ranges, boolToInt(enabled), desc).Scan(&id); err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "创建 CDN 配置组失败")
		}
		if err := apply(); err != nil {
			if _, rollbackErr := database.GetDB().Exec(`DELETE FROM cdn_realip_groups WHERE id = ? AND builtin = 0`, id); rollbackErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组已创建，但 Fail2ban 白名单应用失败，且数据库回滚失败: "+rollbackErr.Error()+"；原始错误: "+err.Error())
			}
			if rollbackRuntimeErr := apply(); rollbackRuntimeErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未创建，数据库已回滚，但 Fail2ban 配置恢复失败: "+rollbackRuntimeErr.Error()+"；原始错误: "+err.Error())
			}
			return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未创建，Fail2ban 白名单应用失败，已回滚: "+err.Error())
		}
		return nil
	})
	if err != nil {
		respondSecurityHandlerError(c, err, "创建 CDN 配置组失败")
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN 配置组已创建"}))
}

func (h *SecurityHandler) UpdateCDNRealIPGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的配置组ID"))
		return
	}

	var req struct {
		Name        string `json:"name"`
		HeaderName  string `json:"header_name"`
		IPRanges    string `json:"ip_ranges"`
		Enabled     *bool  `json:"enabled"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}
	name, header, ranges, enabled, desc, err := normalizeCDNRealIPGroupPayload(req.Name, req.HeaderName, req.IPRanges, req.Enabled, req.Description)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	err = withFail2banSettingsLock(func(apply func() error) error {
		group, err := executor.GetCDNRealIPGroup(id)
		if errors.Is(err, sql.ErrNoRows) {
			return newSecurityHandlerError(http.StatusNotFound, "CDN 配置组不存在")
		}
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取 CDN 配置组失败: "+err.Error())
		}
		if group.Builtin {
			return newSecurityHandlerError(http.StatusBadRequest, "内置 CDN 配置组不可修改")
		}
		if _, err := database.GetDB().Exec(`UPDATE cdn_realip_groups
			SET name = ?, header_name = ?, ip_ranges = ?, enabled = ?, description = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, name, header, ranges, boolToInt(enabled), desc, id); err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "保存 CDN 配置组失败")
		}
		rollbackDB := func(cause error) error {
			if _, rollbackErr := database.GetDB().Exec(`UPDATE cdn_realip_groups
				SET name = ?, header_name = ?, ip_ranges = ?, enabled = ?, description = ?, updated_at = ?
				WHERE id = ?`, group.Name, group.HeaderName, group.IPRanges, boolToInt(group.Enabled), group.Description, group.UpdatedAt, id); rollbackErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组已保存，但服务器配置应用失败，且数据库回滚失败: "+rollbackErr.Error()+"；原始错误: "+cause.Error())
			}
			return nil
		}
		if err := apply(); err != nil {
			cause := errors.New("Fail2ban 白名单应用失败: " + err.Error())
			if rollbackErr := rollbackDB(cause); rollbackErr != nil {
				return rollbackErr
			}
			if rollbackRuntimeErr := apply(); rollbackRuntimeErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未生效，数据库已回滚，但 Fail2ban 配置恢复失败: "+rollbackRuntimeErr.Error()+"；原始错误: "+err.Error())
			}
			return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未生效，Fail2ban 白名单应用失败，已回滚: "+err.Error())
		}
		if err := regenerateAllSitesNginx(); err != nil {
			cause := errors.New("部分网站 Nginx 配置更新失败: " + err.Error())
			if rollbackErr := rollbackDB(cause); rollbackErr != nil {
				return rollbackErr
			}
			if rollbackErr := reapplyCDNRealIPRuntime(apply); rollbackErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组已保存，但部分网站 Nginx 配置更新失败，且回滚失败: "+rollbackErr.Error()+"；原始错误: "+err.Error())
			}
			return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未生效，部分网站 Nginx 配置更新失败，已回滚: "+err.Error())
		}
		return nil
	})
	if err != nil {
		respondSecurityHandlerError(c, err, "保存 CDN 配置组失败")
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN 配置组已保存"}))
}

func (h *SecurityHandler) DeleteCDNRealIPGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的配置组ID"))
		return
	}
	err = withFail2banSettingsLock(func(apply func() error) error {
		group, err := executor.GetCDNRealIPGroup(id)
		if errors.Is(err, sql.ErrNoRows) {
			return newSecurityHandlerError(http.StatusNotFound, "CDN 配置组不存在")
		}
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取 CDN 配置组失败: "+err.Error())
		}
		if group.Builtin {
			return newSecurityHandlerError(http.StatusBadRequest, "内置 CDN 配置组不可删除")
		}
		boundWebsiteIDs, err := websiteIDsForCDNRealIPGroup(id)
		if err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "读取 CDN 配置组绑定网站失败")
		}
		if _, err := database.GetDB().Exec(`DELETE FROM cdn_realip_groups WHERE id = ?`, id); err != nil {
			return newSecurityHandlerError(http.StatusInternalServerError, "删除 CDN 配置组失败")
		}
		rollbackDB := func(cause error) error {
			if restoreErr := restoreCDNRealIPGroupWithBindings(group, boundWebsiteIDs); restoreErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组已删除，但服务器配置应用失败，且数据库回滚失败: "+restoreErr.Error()+"；原始错误: "+cause.Error())
			}
			return nil
		}
		if err := apply(); err != nil {
			cause := errors.New("Fail2ban 白名单应用失败: " + err.Error())
			if rollbackErr := rollbackDB(cause); rollbackErr != nil {
				return rollbackErr
			}
			if rollbackRuntimeErr := apply(); rollbackRuntimeErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未删除，数据库已回滚，但 Fail2ban 配置恢复失败: "+rollbackRuntimeErr.Error()+"；原始错误: "+err.Error())
			}
			return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未删除，Fail2ban 白名单应用失败，已回滚: "+err.Error())
		}
		if err := regenerateAllSitesNginx(); err != nil {
			cause := errors.New("部分网站 Nginx 配置更新失败: " + err.Error())
			if rollbackErr := rollbackDB(cause); rollbackErr != nil {
				return rollbackErr
			}
			if rollbackErr := reapplyCDNRealIPRuntime(apply); rollbackErr != nil {
				return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组已删除，但部分网站 Nginx 配置更新失败，且回滚失败: "+rollbackErr.Error()+"；原始错误: "+err.Error())
			}
			return newSecurityHandlerError(http.StatusInternalServerError, "CDN 配置组未删除，部分网站 Nginx 配置更新失败，已回滚: "+err.Error())
		}
		return nil
	})
	if err != nil {
		respondSecurityHandlerError(c, err, "删除 CDN 配置组失败")
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN 配置组已删除"}))
}

func reapplyCDNRealIPRuntime(apply func() error) error {
	var restoreErrors []error
	if err := apply(); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("Fail2ban 回滚失败: %w", err))
	}
	if err := regenerateAllSitesNginx(); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("Nginx 回滚失败: %w", err))
	}
	return errors.Join(restoreErrors...)
}

type securityHandlerError struct {
	status  int
	message string
}

func (e *securityHandlerError) Error() string {
	return e.message
}

func newSecurityHandlerError(status int, message string) error {
	return &securityHandlerError{status: status, message: message}
}

func respondSecurityHandlerError(c *gin.Context, err error, fallback string) {
	var responseErr *securityHandlerError
	if errors.As(err, &responseErr) {
		c.JSON(responseErr.status, models.ErrorResponse(responseErr.message))
		return
	}
	c.JSON(http.StatusInternalServerError, models.ErrorResponse(fallback+": "+err.Error()))
}

type securitySettingSnapshot struct {
	key         string
	value       string
	description string
	exists      bool
}

func snapshotSecuritySettings(db *sql.DB, keys []string) ([]securitySettingSnapshot, error) {
	if db == nil {
		return nil, errors.New("数据库未初始化")
	}
	snapshots := make([]securitySettingSnapshot, 0, len(keys))
	for _, key := range keys {
		snapshot := securitySettingSnapshot{key: key}
		err := db.QueryRow(`SELECT svalue, description FROM security_settings WHERE skey = ?`, key).
			Scan(&snapshot.value, &snapshot.description)
		if errors.Is(err, sql.ErrNoRows) {
			snapshots = append(snapshots, snapshot)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取安全设置 %q 失败: %w", key, err)
		}
		snapshot.exists = true
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func readSecuritySettingValue(db *sql.DB, key string) (string, error) {
	if db == nil {
		return "", errors.New("数据库未初始化")
	}
	var value string
	if err := db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = ?`, key).Scan(&value); err != nil {
		return "", fmt.Errorf("读取安全设置 %q 失败: %w", key, err)
	}
	return value, nil
}

func writeImportedGooglebotSettings(db *sql.DB, updates map[string]string) error {
	if db == nil {
		return errors.New("数据库未初始化")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range updates {
		if _, err := tx.Exec(`INSERT INTO security_settings (skey, svalue, description, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, description = excluded.description, updated_at = excluded.updated_at`,
			key, value, "Googlebot 手动导入与状态"); err != nil {
			return fmt.Errorf("写入安全设置 %q 失败: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 Googlebot 白名单失败: %w", err)
	}
	return nil
}

func restoreSecuritySettings(db *sql.DB, snapshots []securitySettingSnapshot) error {
	if db == nil {
		return errors.New("数据库未初始化")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, snapshot := range snapshots {
		if !snapshot.exists {
			if _, err := tx.Exec(`DELETE FROM security_settings WHERE skey = ?`, snapshot.key); err != nil {
				return fmt.Errorf("删除新增安全设置 %q 失败: %w", snapshot.key, err)
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO security_settings (skey, svalue, description, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, description = excluded.description, updated_at = excluded.updated_at`,
			snapshot.key, snapshot.value, snapshot.description); err != nil {
			return fmt.Errorf("恢复安全设置 %q 失败: %w", snapshot.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交安全设置回滚失败: %w", err)
	}
	return nil
}

func reapplyImportedGooglebotRuntime(apply func() error) error {
	var restoreErrors []error
	if err := apply(); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("Fail2ban 回滚失败: %w", err))
	}
	if err := ensureLogMap(); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("搜索引擎验证规则回滚失败: %w", err))
	}
	return errors.Join(restoreErrors...)
}

func normalizeCDNRealIPGroupPayload(name, headerName, rawRanges string, enabled *bool, description string) (string, string, string, bool, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 50 || strings.ContainsAny(name, "\r\n\t") {
		return "", "", "", false, "", fmt.Errorf("CDN 配置组名称格式不正确")
	}
	header, err := executor.NormalizeCDNRealIPHeader(headerName)
	if err != nil {
		return "", "", "", false, "", err
	}
	ranges, err := executor.NormalizeCDNRealIPRanges(rawRanges)
	if err != nil {
		return "", "", "", false, "", err
	}
	isEnabled := true
	if enabled != nil {
		isEnabled = *enabled
	}
	description = strings.TrimSpace(description)
	if len(description) > 200 {
		return "", "", "", false, "", fmt.Errorf("备注过长")
	}
	return name, header, executor.JoinCDNRealIPRanges(ranges), isEnabled, description, nil
}

func normalizeSecuritySetting(key string, val interface{}) (string, bool, error) {
	switch key {
	case "fail2ban_maxretry":
		return normalizeRange(key, val, 1, 20)
	case "fail2ban_findtime":
		return normalizeRange(key, val, 10, 3600)
	case "fail2ban_bantime":
		fixed, _, err := normalizeRange(key, val, 600, 600)
		if err != nil {
			return "", false, fmt.Errorf("首次封禁时长已固定为600秒")
		}
		_ = fixed
		return "", false, nil
	case "rate_limit_rpm":
		return normalizeRange(key, val, 10, 600)
	case "rate_limit_burst":
		return normalizeRange(key, val, 5, 600)
	case "bot_limit_rpm":
		return normalizeRange(key, val, 5, 300)
	case "bot_limit_burst":
		return normalizeRange(key, val, 5, 300)
	case "wp_sqli_ban_threshold":
		return normalizeRange(key, val, 1, 50)
	case "wp_sqli_ban_window_seconds":
		return normalizeRange(key, val, 60, 3600)
	case "auto_whitelist_enabled", "rate_limit_enabled", "bot_limit_enabled", "telemetry_enabled", "wp_sqli_block_enabled", "wp_sqli_autoban_enabled":
		v, err := normalizeBool(val)
		return v, true, err
	case "telemetry_url":
		v, err := normalizeTelemetryURL(val)
		return v, true, err
	case "whitelist_ips":
		v, ok := val.(string)
		if !ok {
			return "", false, fmt.Errorf("白名单格式不正确")
		}
		v = strings.TrimSpace(v)
		if err := validateWhitelistIPs(v); err != nil {
			return "", false, err
		}
		return v, true, nil
	case "wp_security_log_whitelist":
		v, ok := val.(string)
		if !ok {
			return "", false, fmt.Errorf("WordPress安全日志白名单格式不正确")
		}
		patterns, err := executor.NormalizeWPSecurityLogWhitelist(v)
		if err != nil {
			return "", false, err
		}
		return strings.Join(patterns, "\n"), true, nil
	default:
		return "", false, nil
	}
}

func normalizeTelemetryURL(val interface{}) (string, error) {
	v, err := normalizePlainString(val, 2048, "telemetry_url")
	if err != nil {
		return "", fmt.Errorf("遥测地址格式不正确")
	}
	if v == "" {
		return "", nil
	}

	normalized, err := executor.NormalizeTelemetryURL(v)
	if err != nil {
		return "", fmt.Errorf("遥测地址必须是使用公网主机的有效 HTTPS URL")
	}
	return normalized, nil
}

func normalizeRange(key string, val interface{}, min int, max int) (string, bool, error) {
	n, err := normalizeInt(val)
	if err != nil {
		return "", false, fmt.Errorf("%s 必须是数字", key)
	}
	if n < min || n > max {
		return "", false, fmt.Errorf("%s 必须在 %d-%d 之间", key, min, max)
	}
	return strconv.Itoa(n), true, nil
}

func normalizeInt(val interface{}) (int, error) {
	switch v := val.(type) {
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("invalid int")
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("invalid int")
	}
}

func normalizeBool(val interface{}) (string, error) {
	switch v := val.(type) {
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case string:
		v = strings.TrimSpace(v)
		if v == "true" || v == "false" {
			return v, nil
		}
	}
	return "", fmt.Errorf("开关值不正确")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func validateWhitelistIPs(raw string) error {
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 500 {
		return fmt.Errorf("白名单数量过大")
	}
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		if strings.ContainsAny(item, " \t\r") {
			return fmt.Errorf("白名单 %s 格式不正确", item)
		}
		if strings.Contains(item, "/") {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return fmt.Errorf("白名单 %s 格式不正确", item)
			}
			continue
		}
		if net.ParseIP(item) == nil {
			return fmt.Errorf("白名单 %s 格式不正确", item)
		}
	}
	return nil
}
