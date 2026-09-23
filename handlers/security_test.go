package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zangwp/yub-wpanel/database"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/models"
)

func TestRefreshWhitelistReportsQueuePressure(t *testing.T) {
	previousEnqueue := enqueueOfficialWhitelistRefresh
	enqueueOfficialWhitelistRefresh = func(context.Context) error {
		return executor.ErrTaskQueueFull
	}
	t.Cleanup(func() { enqueueOfficialWhitelistRefresh = previousEnqueue })

	rec := performSecurityRequest(http.MethodPost, "/refresh", "", func(router *gin.Engine, h *SecurityHandler) {
		router.POST("/refresh", h.RefreshWhitelist)
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "任务队列繁忙") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestImportGooglebotRangesPropagatesRequiredCacheReadFailure(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	if _, err := database.GetDB().Exec(`DELETE FROM security_settings WHERE skey = 'bingbot_ips'`); err != nil {
		t.Fatal(err)
	}
	applyFail2banSettings = func() error {
		t.Fatal("Fail2ban must not be applied when a required cache read fails")
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPost,
		"/googlebot",
		`{"ip_ranges":"66.249.64.0/19"}`,
		func(router *gin.Engine, h *SecurityHandler) { router.POST("/googlebot", h.ImportGooglebotRanges) },
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(decodeAPIResponse(t, rec).Message, "bingbot_ips") {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
	if got := securitySettingValue(t, "googlebot_ips"); got != "" {
		t.Fatalf("googlebot cache changed after failed snapshot: %q", got)
	}
}

func TestImportGooglebotRangesWritesAtomically(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	if _, err := database.GetDB().Exec(`CREATE TRIGGER reject_manual_googlebot_official
		BEFORE UPDATE ON security_settings
		WHEN NEW.skey = 'official_whitelist_ips' AND NEW.svalue LIKE '%66.249.64.0/19%'
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	applyFail2banSettings = func() error {
		t.Fatal("Fail2ban must not be applied after an atomic database write failure")
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPost,
		"/googlebot",
		`{"ip_ranges":"66.249.64.0/19"}`,
		func(router *gin.Engine, h *SecurityHandler) { router.POST("/googlebot", h.ImportGooglebotRanges) },
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"googlebot_ips", "googlebot_ips_source", "googlebot_ips_last_success_at", "googlebot_ips_last_error", "official_whitelist_ips"} {
		if got := securitySettingValue(t, key); got != "" {
			t.Fatalf("%s changed after failed transaction: %q", key, got)
		}
	}
}

func TestImportGooglebotRangesRollsBackDatabaseAndRuntimeInsideLock(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	if _, err := database.GetDB().Exec(`UPDATE security_settings SET svalue = '203.0.113.0/24' WHERE skey = 'googlebot_ips'`); err != nil {
		t.Fatal(err)
	}
	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		if applyCalls == 1 {
			if got := securitySettingValue(t, "googlebot_ips"); got != "66.249.64.0/19" {
				t.Fatalf("runtime apply observed googlebot_ips = %q, want imported value", got)
			}
			return errors.New("injected Fail2ban failure")
		}
		if got := securitySettingValue(t, "googlebot_ips"); got != "203.0.113.0/24" {
			t.Fatalf("rollback apply observed googlebot_ips = %q, want old value", got)
		}
		return nil
	}
	logMapCalls := 0
	ensureLogMap = func() error {
		logMapCalls++
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPost,
		"/googlebot",
		`{"ip_ranges":"66.249.64.0/19"}`,
		func(router *gin.Engine, h *SecurityHandler) { router.POST("/googlebot", h.ImportGooglebotRanges) },
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "googlebot_ips"); got != "203.0.113.0/24" {
		t.Fatalf("googlebot_ips after rollback = %q", got)
	}
	if applyCalls != 2 || logMapCalls != 1 {
		t.Fatalf("Fail2ban/log-map calls = %d/%d, want 2/1", applyCalls, logMapCalls)
	}
}

func TestUpdateCDNRealIPGroupFail2banFailureRollsBackDB(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		if applyCalls == 1 {
			return errors.New("fail2ban failed")
		}
		return nil
	}
	regenerateAllSitesNginx = func() error {
		t.Fatal("nginx regenerate should not run after fail2ban failure")
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPut,
		"/groups/99",
		`{"name":"New","header_name":"X-Real-IP","ip_ranges":"198.51.100.0/24","enabled":true,"description":"new desc"}`,
		func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/groups/:id", h.UpdateCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "未生效") || !strings.Contains(resp.Message, "已回滚") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}

	var name, header, ranges string
	var enabled int
	if err := database.GetDB().QueryRow(`SELECT name, header_name, ip_ranges, enabled FROM cdn_realip_groups WHERE id = 99`).
		Scan(&name, &header, &ranges, &enabled); err != nil {
		t.Fatalf("query group: %v", err)
	}
	if name != "Old" || header != "X-Forwarded-For" || ranges != "203.0.113.0/24" || enabled != 1 {
		t.Fatalf("group was not rolled back: name=%q header=%q ranges=%q enabled=%d", name, header, ranges, enabled)
	}
	if applyCalls != 2 {
		t.Fatalf("apply calls = %d, want initial apply and rollback apply", applyCalls)
	}
}

func TestCreateCDNRealIPGroupFail2banFailureDeletesGroup(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		if applyCalls == 1 {
			return errors.New("fail2ban failed")
		}
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPost,
		"/groups",
		`{"name":"New","header_name":"X-Forwarded-For","ip_ranges":"198.51.100.0/24","enabled":true,"description":"new desc"}`,
		func(router *gin.Engine, h *SecurityHandler) {
			router.POST("/groups", h.CreateCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "未创建") || !strings.Contains(resp.Message, "已回滚") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}

	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM cdn_realip_groups WHERE name = 'New'`).Scan(&count); err != nil {
		t.Fatalf("query group count: %v", err)
	}
	if count != 0 {
		t.Fatalf("created group was not rolled back, count=%d", count)
	}
	if applyCalls != 2 {
		t.Fatalf("apply calls = %d, want initial apply and rollback apply", applyCalls)
	}
}

func TestUpdateCDNRealIPGroupTakesSnapshotInsideFail2banLock(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		if applyCalls == 1 {
			return errors.New("injected apply failure")
		}
		return nil
	}
	withFail2banSettingsLock = func(fn func(apply func() error) error) error {
		if _, err := database.GetDB().Exec(`UPDATE cdn_realip_groups SET name = 'Locked snapshot' WHERE id = 99`); err != nil {
			return err
		}
		return fn(func() error { return applyFail2banSettings() })
	}

	rec := performSecurityRequest(
		http.MethodPut,
		"/groups/99",
		`{"name":"New","header_name":"X-Real-IP","ip_ranges":"198.51.100.0/24","enabled":true,"description":"new desc"}`,
		func(router *gin.Engine, h *SecurityHandler) { router.PUT("/groups/:id", h.UpdateCDNRealIPGroup) },
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var name string
	if err := database.GetDB().QueryRow(`SELECT name FROM cdn_realip_groups WHERE id = 99`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Locked snapshot" {
		t.Fatalf("rollback restored pre-lock value %q; want snapshot taken inside lock", name)
	}
}

func TestUpdateCDNRealIPGroupNginxFailureRollsBackRuntime(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		return nil
	}
	nginxCalls := 0
	regenerateAllSitesNginx = func() error {
		nginxCalls++
		if nginxCalls == 1 {
			return errors.New("nginx failed")
		}
		return nil
	}

	rec := performSecurityRequest(
		http.MethodPut,
		"/groups/99",
		`{"name":"New","header_name":"X-Real-IP","ip_ranges":"198.51.100.0/24","enabled":true,"description":"new desc"}`,
		func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/groups/:id", h.UpdateCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "nginx failed") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}
	if applyCalls != 2 || nginxCalls != 2 {
		t.Fatalf("apply/nginx calls = %d/%d, want 2/2", applyCalls, nginxCalls)
	}
	assertTestCDNRealIPGroupRolledBack(t)
}

func TestUpdateCDNRealIPGroupNginxFailureReportsRollbackFailure(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyFail2banSettings = func() error { return nil }
	nginxCalls := 0
	regenerateAllSitesNginx = func() error {
		nginxCalls++
		if nginxCalls == 1 {
			return errors.New("nginx failed")
		}
		return errors.New("rollback nginx failed")
	}

	rec := performSecurityRequest(
		http.MethodPut,
		"/groups/99",
		`{"name":"New","header_name":"X-Real-IP","ip_ranges":"198.51.100.0/24","enabled":true,"description":"new desc"}`,
		func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/groups/:id", h.UpdateCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "rollback nginx failed") || !strings.Contains(resp.Message, "nginx failed") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}
	if nginxCalls != 2 {
		t.Fatalf("nginx calls = %d, want 2", nginxCalls)
	}
	assertTestCDNRealIPGroupRolledBack(t)
}

func TestDeleteCDNRealIPGroupNginxFailureRollsBackRuntime(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyCalls := 0
	applyFail2banSettings = func() error {
		applyCalls++
		return nil
	}
	nginxCalls := 0
	regenerateAllSitesNginx = func() error {
		nginxCalls++
		if nginxCalls == 1 {
			return errors.New("nginx failed")
		}
		return nil
	}

	rec := performSecurityRequest(
		http.MethodDelete,
		"/groups/99",
		"",
		func(router *gin.Engine, h *SecurityHandler) {
			router.DELETE("/groups/:id", h.DeleteCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "nginx failed") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}
	if applyCalls != 2 || nginxCalls != 2 {
		t.Fatalf("apply/nginx calls = %d/%d, want 2/2", applyCalls, nginxCalls)
	}
	assertTestCDNRealIPGroupRolledBack(t)
}

func TestDeleteCDNRealIPGroupNginxFailureReportsRollbackFailure(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyFail2banSettings = func() error { return nil }
	nginxCalls := 0
	regenerateAllSitesNginx = func() error {
		nginxCalls++
		if nginxCalls == 1 {
			return errors.New("nginx failed")
		}
		return errors.New("rollback nginx failed")
	}

	rec := performSecurityRequest(
		http.MethodDelete,
		"/groups/99",
		"",
		func(router *gin.Engine, h *SecurityHandler) {
			router.DELETE("/groups/:id", h.DeleteCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "rollback nginx failed") || !strings.Contains(resp.Message, "nginx failed") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}
	if nginxCalls != 2 {
		t.Fatalf("nginx calls = %d, want 2", nginxCalls)
	}
	assertTestCDNRealIPGroupRolledBack(t)
}

func TestDeleteCDNRealIPGroupReportsRestoreFailure(t *testing.T) {
	setupSecurityTestDB(t)
	insertTestCDNRealIPGroup(t)
	restoreSecurityExecutorHooks(t)

	applyFail2banSettings = func() error { return errors.New("fail2ban failed") }
	websiteIDsForCDNRealIPGroup = func(int) ([]int, error) { return []int{101}, nil }
	restoreCDNRealIPGroupWithBindings = func(models.CDNRealIPGroup, []int) error {
		return errors.New("restore failed")
	}
	regenerateAllSitesNginx = func() error {
		t.Fatal("nginx regenerate should not run after fail2ban failure")
		return nil
	}

	rec := performSecurityRequest(
		http.MethodDelete,
		"/groups/99",
		"",
		func(router *gin.Engine, h *SecurityHandler) {
			router.DELETE("/groups/:id", h.DeleteCDNRealIPGroup)
		},
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeAPIResponse(t, rec)
	if !strings.Contains(resp.Message, "数据库回滚失败") || !strings.Contains(resp.Message, "原始错误") {
		t.Fatalf("unexpected message: %s", resp.Message)
	}
}

func TestUpdateSQLiSettingsAppliesAndPersists(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	nginxCalls, fail2banCalls := 0, 0
	regenerateAllSitesNginx = func() error { nginxCalls++; return nil }
	applyFail2banSettings = func() error { fail2banCalls++; return nil }

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"wp_sqli_block_enabled":"false","wp_sqli_autoban_enabled":"false","wp_sqli_ban_threshold":"7","wp_sqli_ban_window_seconds":"900"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if nginxCalls != 1 || fail2banCalls != 1 {
		t.Fatalf("nginx/fail2ban calls = %d/%d, want 1/1", nginxCalls, fail2banCalls)
	}
	for key, want := range map[string]string{
		"wp_sqli_block_enabled": "false", "wp_sqli_autoban_enabled": "false",
		"wp_sqli_ban_threshold": "7", "wp_sqli_ban_window_seconds": "900",
	} {
		var got string
		if err := database.GetDB().QueryRow(`SELECT svalue FROM security_settings WHERE skey=?`, key).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q, err=%v, want %q", key, got, err, want)
		}
	}
}

func TestUpdateSQLiSettingsRollsBackDatabaseAndRuntime(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	nginxCalls, fail2banCalls := 0, 0
	regenerateAllSitesNginx = func() error { nginxCalls++; return nil }
	applyFail2banSettings = func() error {
		fail2banCalls++
		if fail2banCalls == 1 {
			return errors.New("injected fail2ban failure")
		}
		return nil
	}

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"wp_sqli_ban_threshold":"9"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if nginxCalls != 2 || fail2banCalls != 2 {
		t.Fatalf("nginx/fail2ban calls = %d/%d, want 2/2", nginxCalls, fail2banCalls)
	}
	var got string
	if err := database.GetDB().QueryRow(`SELECT svalue FROM security_settings WHERE skey='wp_sqli_ban_threshold'`).Scan(&got); err != nil || got != "5" {
		t.Fatalf("threshold after rollback = %q, err=%v, want 5", got, err)
	}
}

func TestUpdateSQLiSettingsDatabaseWriteIsAtomic(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	if _, err := database.GetDB().Exec(`CREATE TRIGGER reject_sqli_threshold
		BEFORE UPDATE ON security_settings
		WHEN NEW.skey='wp_sqli_ban_threshold' AND NEW.svalue='9'
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	regenerateAllSitesNginx = func() error { t.Fatal("nginx must not run after database failure"); return nil }
	applyFail2banSettings = func() error { t.Fatal("fail2ban must not run after database failure"); return nil }

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"wp_sqli_block_enabled":"false","wp_sqli_ban_threshold":"9"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for key, want := range map[string]string{
		"wp_sqli_block_enabled": "true",
		"wp_sqli_ban_threshold": "5",
	} {
		var got string
		if err := database.GetDB().QueryRow(`SELECT svalue FROM security_settings WHERE skey=?`, key).Scan(&got); err != nil || got != want {
			t.Fatalf("%s after failed transaction = %q, err=%v, want %q", key, got, err, want)
		}
	}
}

func TestUpdateSecuritySettingsDatabaseWriteIsAtomicAcrossSubsystems(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	if _, err := database.GetDB().Exec(`CREATE TRIGGER reject_rate_limit_burst
		BEFORE UPDATE ON security_settings
		WHEN NEW.skey='rate_limit_burst' AND NEW.svalue='20'
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	regenerateAllSitesNginx = func() error { t.Fatal("nginx must not run after database failure"); return nil }
	applyFail2banSettings = func() error { t.Fatal("fail2ban must not run after database failure"); return nil }
	applyRateLimitSettings = func() error { t.Fatal("rate limit must not run after database failure"); return nil }

	beforeSQLi := securitySettingValue(t, "wp_sqli_block_enabled")
	beforeBurst := securitySettingValue(t, "rate_limit_burst")
	rec := performSecurityRequest(http.MethodPut, "/settings", `{"wp_sqli_block_enabled":"false","rate_limit_burst":"20"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "wp_sqli_block_enabled"); got != beforeSQLi {
		t.Fatalf("SQLi setting after failed transaction = %q, want %q", got, beforeSQLi)
	}
	if got := securitySettingValue(t, "rate_limit_burst"); got != beforeBurst {
		t.Fatalf("rate limit setting after failed transaction = %q, want %q", got, beforeBurst)
	}
}

func TestUpdateGenericSecuritySettingRollsBackDatabaseAndRuntime(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	before := securitySettingValue(t, "fail2ban_maxretry")
	calls := 0
	applyFail2banSettings = func() error {
		calls++
		if calls == 1 {
			return errors.New("injected fail2ban failure")
		}
		return nil
	}

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"fail2ban_maxretry":"12"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("fail2ban calls = %d, want 2", calls)
	}
	if got := securitySettingValue(t, "fail2ban_maxretry"); got != before {
		t.Fatalf("maxretry after rollback = %q, want %q", got, before)
	}
}

func TestUpdateSecuritySettingsRollbackAttemptsEveryRuntimeSubsystem(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	nginxCalls, fail2banCalls, rateLimitCalls := 0, 0, 0
	regenerateAllSitesNginx = func() error {
		nginxCalls++
		if nginxCalls == 2 {
			return errors.New("injected rollback nginx failure")
		}
		return nil
	}
	applyFail2banSettings = func() error { fail2banCalls++; return nil }
	applyRateLimitSettings = func() error {
		rateLimitCalls++
		if rateLimitCalls == 1 {
			return errors.New("injected rate limit failure")
		}
		return nil
	}

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"wp_sqli_block_enabled":"false","rate_limit_burst":"20"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if nginxCalls != 2 || fail2banCalls != 2 || rateLimitCalls != 2 {
		t.Fatalf("nginx/fail2ban/rate limit calls = %d/%d/%d, want 2/2/2", nginxCalls, fail2banCalls, rateLimitCalls)
	}
	if !strings.Contains(decodeAPIResponse(t, rec).Message, "服务器配置恢复不完整") {
		t.Fatalf("unexpected message: %s", rec.Body.String())
	}
	if got := securitySettingValue(t, "wp_sqli_block_enabled"); got != "true" {
		t.Fatalf("SQLi setting after rollback = %q, want true", got)
	}
}

func TestUpdateSecuritySettingsSerializesSnapshotThroughRuntimeApply(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	firstApplyStarted := make(chan struct{})
	releaseFirstApply := make(chan struct{})
	var calls atomic.Int32
	applyFail2banSettings = func() error {
		if calls.Add(1) == 1 {
			close(firstApplyStarted)
			<-releaseFirstApply
			return errors.New("injected first request failure")
		}
		return nil
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performSecurityRequest(http.MethodPut, "/settings", `{"fail2ban_maxretry":"12"}`, func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/settings", h.UpdateSettings)
		})
	}()
	select {
	case <-firstApplyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach runtime apply")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDone <- performSecurityRequest(http.MethodPut, "/settings", `{"fail2ban_maxretry":"13"}`, func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/settings", h.UpdateSettings)
		})
	}()
	select {
	case rec := <-secondDone:
		t.Fatalf("second request completed before first request released: %s", rec.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirstApply)

	if rec := <-firstDone; rec.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := <-secondDone; rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "fail2ban_maxretry"); got != "13" {
		t.Fatalf("final maxretry = %q, want 13", got)
	}
}

func TestUpdateTelemetrySettingPersistsEnableAndDisable(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled":"true"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_enabled"); got != "false" {
		t.Fatalf("telemetry_enabled changed after rejected enable: %q", got)
	}

	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled":"true","telemetry_url":"https://8.8.8.8/telemetry/"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable with endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_enabled"); got != "true" {
		t.Fatalf("telemetry_enabled = %q, want true", got)
	}
	if got := securitySettingValue(t, "telemetry_url"); got != "https://8.8.8.8/telemetry" {
		t.Fatalf("telemetry_url = %q, want canonical endpoint", got)
	}

	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_url":""}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clear while enabled status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_url"); got != "https://8.8.8.8/telemetry" {
		t.Fatalf("telemetry_url changed after rejected clear: %q", got)
	}

	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled":false,"telemetry_url":""}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable and clear status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_enabled"); got != "false" {
		t.Fatalf("telemetry_enabled = %q, want false", got)
	}
	if got := securitySettingValue(t, "telemetry_url"); got != "" {
		t.Fatalf("telemetry_url = %q, want empty", got)
	}
}

func TestUpdateTelemetrySettingValidatesStoredEndpointOnEnable(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_url":"https://8.8.8.8/telemetry"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled":true}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable with stored endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if _, err := database.GetDB().Exec(`UPDATE security_settings SET svalue='false' WHERE skey='telemetry_enabled'`); err != nil {
		t.Fatalf("disable telemetry fixture: %v", err)
	}
	if _, err := database.GetDB().Exec(`UPDATE security_settings SET svalue='https://192.0.2.1/legacy' WHERE skey='telemetry_url'`); err != nil {
		t.Fatalf("set legacy endpoint fixture: %v", err)
	}
	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled":true}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable with unsafe stored endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_enabled"); got != "false" {
		t.Fatalf("telemetry_enabled changed after unsafe stored endpoint: %q", got)
	}
}

func TestUpdateTelemetryURLPersistsOnlySafeHTTPSValue(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_url":"https://8.8.8.8/telemetry/"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("safe URL status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_url"); got != "https://8.8.8.8/telemetry" {
		t.Fatalf("telemetry_url = %q, want canonical safe URL", got)
	}

	for _, unsafe := range []string{
		"http://8.8.8.8/telemetry",
		"https://user:pass@8.8.8.8/telemetry",
		"https://8.8.8.8/telemetry#fragment",
		"https://8.8.8.8/telemetry?token=secret",
		"https://8.8.8.8:70000/telemetry",
		"https://localhost/telemetry",
		"https://127.0.0.1/telemetry",
		"https://10.0.0.1/telemetry",
		"https://169.254.169.254/latest/meta-data",
		"https://192.0.2.1/telemetry",
		"https://198.18.0.1/telemetry",
		"https://198.51.100.1/telemetry",
		"https://203.0.113.1/telemetry",
		"https://[::1]/telemetry",
		"https://[2001:db8::1]/telemetry",
	} {
		payload, err := json.Marshal(map[string]string{"telemetry_url": unsafe})
		if err != nil {
			t.Fatal(err)
		}
		rec = performSecurityRequest(http.MethodPut, "/settings", string(payload), func(router *gin.Engine, h *SecurityHandler) {
			router.PUT("/settings", h.UpdateSettings)
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unsafe URL %q status = %d, body = %s", unsafe, rec.Code, rec.Body.String())
		}
		if got := securitySettingValue(t, "telemetry_url"); got != "https://8.8.8.8/telemetry" {
			t.Fatalf("telemetry_url changed after rejecting %q: %q", unsafe, got)
		}
	}

	rec = performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_url":""}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK || securitySettingValue(t, "telemetry_url") != "" {
		t.Fatalf("clear telemetry URL failed: status=%d body=%s value=%q", rec.Code, rec.Body.String(), securitySettingValue(t, "telemetry_url"))
	}
}

func TestUpdateSecuritySettingsContinuesToIgnoreUnknownKeys(t *testing.T) {
	setupSecurityTestDB(t)
	restoreSecurityExecutorHooks(t)
	before := securitySettingValue(t, "telemetry_enabled")

	rec := performSecurityRequest(http.MethodPut, "/settings", `{"telemetry_enabled_typo":"true"}`, func(router *gin.Engine, h *SecurityHandler) {
		router.PUT("/settings", h.UpdateSettings)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := securitySettingValue(t, "telemetry_enabled"); got != before {
		t.Fatalf("known setting changed from %q to %q after unknown-only update", before, got)
	}
	var count int
	if err := database.GetDB().QueryRow(`SELECT COUNT(*) FROM security_settings WHERE skey='telemetry_enabled_typo'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unknown security setting was persisted, count=%d", count)
	}
}

func TestNormalizeSecuritySettingAcceptsBotLimitSettings(t *testing.T) {
	for _, tc := range []struct {
		key  string
		val  interface{}
		want string
	}{
		{"bot_limit_enabled", true, "true"},
		{"bot_limit_rpm", "30", "30"},
		{"bot_limit_burst", float64(20), "20"},
	} {
		got, ok, err := normalizeSecuritySetting(tc.key, tc.val)
		if err != nil {
			t.Fatalf("%s returned error: %v", tc.key, err)
		}
		if !ok || got != tc.want {
			t.Fatalf("%s = %q, %v; want %q, true", tc.key, got, ok, tc.want)
		}
	}
}

func TestNormalizeSecuritySettingRejectsBotLimitOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		key string
		val interface{}
	}{
		{"bot_limit_rpm", "4"},
		{"bot_limit_rpm", "301"},
		{"bot_limit_burst", "4"},
		{"bot_limit_burst", "301"},
	} {
		if _, _, err := normalizeSecuritySetting(tc.key, tc.val); err == nil {
			t.Fatalf("expected %s=%v to be rejected", tc.key, tc.val)
		}
	}
}

func setupSecurityTestDB(t *testing.T) {
	t.Helper()
	oldDB := database.DB
	if err := database.Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.RunMigrations(); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
		database.DB = oldDB
	})
}

func insertTestCDNRealIPGroup(t *testing.T) {
	t.Helper()
	if _, err := database.GetDB().Exec(`INSERT INTO cdn_realip_groups
		(id, name, provider, header_name, ip_ranges, builtin, enabled, description)
		VALUES (99, 'Old', 'custom', 'X-Forwarded-For', '203.0.113.0/24', 0, 1, 'old desc')`); err != nil {
		t.Fatalf("insert cdn group: %v", err)
	}
}

func assertTestCDNRealIPGroupRolledBack(t *testing.T) {
	t.Helper()
	var name, header, ranges string
	var enabled int
	if err := database.GetDB().QueryRow(`SELECT name, header_name, ip_ranges, enabled FROM cdn_realip_groups WHERE id = 99`).
		Scan(&name, &header, &ranges, &enabled); err != nil {
		t.Fatalf("query group: %v", err)
	}
	if name != "Old" || header != "X-Forwarded-For" || ranges != "203.0.113.0/24" || enabled != 1 {
		t.Fatalf("group was not rolled back: name=%q header=%q ranges=%q enabled=%d", name, header, ranges, enabled)
	}
}

func restoreSecurityExecutorHooks(t *testing.T) {
	t.Helper()
	oldApplyFail2ban := applyFail2banSettings
	oldWithFail2banLock := withFail2banSettingsLock
	oldApplyRateLimit := applyRateLimitSettings
	oldEnsureLogMap := ensureLogMap
	oldRegenerateAllSitesNginx := regenerateAllSitesNginx
	oldWebsiteIDsForCDNRealIPGroup := websiteIDsForCDNRealIPGroup
	oldRestoreCDNRealIPGroupWithBindings := restoreCDNRealIPGroupWithBindings
	withFail2banSettingsLock = func(fn func(apply func() error) error) error {
		return fn(func() error { return applyFail2banSettings() })
	}
	t.Cleanup(func() {
		applyFail2banSettings = oldApplyFail2ban
		withFail2banSettingsLock = oldWithFail2banLock
		applyRateLimitSettings = oldApplyRateLimit
		ensureLogMap = oldEnsureLogMap
		regenerateAllSitesNginx = oldRegenerateAllSitesNginx
		websiteIDsForCDNRealIPGroup = oldWebsiteIDsForCDNRealIPGroup
		restoreCDNRealIPGroupWithBindings = oldRestoreCDNRealIPGroupWithBindings
	})
}

func securitySettingValue(t *testing.T, key string) string {
	t.Helper()
	var value string
	if err := database.GetDB().QueryRow(`SELECT svalue FROM security_settings WHERE skey=?`, key).Scan(&value); err != nil {
		t.Fatalf("read security setting %s: %v", key, err)
	}
	return value
}

func performSecurityRequest(method, path, body string, register func(*gin.Engine, *SecurityHandler)) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	register(router, &SecurityHandler{})

	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeAPIResponse(t *testing.T, rec *httptest.ResponseRecorder) models.ApiResponse {
	t.Helper()
	var resp models.ApiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}
