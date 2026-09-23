package executor

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

type alertRule struct {
	key                string
	checkFn            func() (firing bool, msg string)
	checkStateFn       func() alertCheckResult
	thresholdDuration  time.Duration
	eventOnly          bool
	sendRecovery       bool
	pendingSince       time.Time
	lastFired          time.Time
	firing             bool
	lastAlertMsg       string
	runtimeStateDirty  bool
	resolutionDirty    bool
	runtimeStateLoaded bool
}

type alertManager struct {
	mu     sync.Mutex
	rules  []*alertRule
	stopCh chan struct{}
}

type alertCheckResult struct {
	state   alertCheckState
	message string
	err     error
}

type alertCheckState uint8

const (
	alertCheckNormal alertCheckState = iota
	alertCheckFiring
	alertCheckUnknown
)

func normalAlertCheck() alertCheckResult {
	return alertCheckResult{state: alertCheckNormal}
}

func firingAlertCheck(message string) alertCheckResult {
	return alertCheckResult{state: alertCheckFiring, message: message}
}

func unknownAlertCheck(err error) alertCheckResult {
	return alertCheckResult{state: alertCheckUnknown, err: err}
}

func legacyAlertCheck(result alertCheckResult) (bool, string) {
	return result.state == alertCheckFiring, result.message
}

func (r *alertRule) evaluate() alertCheckResult {
	if r.checkStateFn != nil {
		return r.checkStateFn()
	}
	if r.checkFn == nil {
		return unknownAlertCheck(errors.New("alert rule has no checker"))
	}
	firing, message := r.checkFn()
	if firing {
		return firingAlertCheck(message)
	}
	return normalAlertCheck()
}

var (
	alertMgr                 = &alertManager{stopCh: make(chan struct{})}
	panelCurrentVersion      string
	cloudflareProxyDetector  = isLikelyCloudflareProxied
	cloudflareDNSLookup      = net.DefaultResolver.LookupIPAddr
	reportAlertStateError    = log.Printf
	cloudflareDetectionCache = struct {
		sync.Mutex
		entries map[string]cloudflareDetectionEntry
	}{entries: make(map[string]cloudflareDetectionEntry)}
)

type cloudflareDetectionEntry struct {
	proxied   bool
	checkedAt time.Time
}

func StartAlertMonitor(currentVersion string) {
	panelCurrentVersion = currentVersion
	alertMgr.rules = []*alertRule{
		{key: "alert_cpu", checkStateFn: checkCPUState, thresholdDuration: 5 * time.Minute, sendRecovery: true},
		{key: "alert_memory", checkStateFn: checkMemoryState, thresholdDuration: 5 * time.Minute, sendRecovery: true},
		{key: "alert_disk", checkStateFn: checkDiskState, sendRecovery: true},
		{key: "alert_service", checkStateFn: checkServiceState, sendRecovery: true},
		{key: "alert_ssl", checkStateFn: checkSSLState, sendRecovery: true},
		{key: "alert_backup", checkStateFn: checkBackupState, sendRecovery: true},
		{key: "alert_website_expiry", checkFn: checkWebsiteExpiry, eventOnly: true},
		{key: "alert_remote_backup", checkStateFn: checkRemoteBackupState, sendRecovery: true},
		{key: "alert_cron_fail", checkStateFn: checkCronFailState, sendRecovery: true},
		{key: "alert_site", checkStateFn: checkSitesState, sendRecovery: true},
		{key: "alert_system_update", checkStateFn: checkSystemUpdateState},
		{key: "alert_panel_update", checkStateFn: checkPanelUpdateState},
		{key: "alert_wp_fake_search_bot", checkStateFn: checkWPFakeSearchBotThresholdState},
	}
	loadAlertRuntimeState(alertMgr.rules)
	go alertMgr.loop()
}

func (m *alertManager) loop() {
	// Initial check without sending (warm up)
	time.Sleep(30 * time.Second)
	m.runChecks()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.runChecks()
		case <-m.stopCh:
			return
		}
	}
}

func (m *alertManager) runChecks() {
	go runWPAnomalyChecks()
	go runWPCodeIntegrityChecks()

	m.mu.Lock()
	defer m.mu.Unlock()

	// A rule must not be evaluated until its persisted state has either been
	// restored or confirmed absent. Otherwise a transient startup read failure
	// turns a historical firing rule into an in-memory normal rule, which can
	// duplicate the next alert or leave the old incident unresolved forever.
	runtimeReady := make(map[string]bool, len(m.rules))
	for _, r := range m.rules {
		runtimeReady[r.key] = ensureAlertRuntimeStateLoaded(r)
		if runtimeReady[r.key] {
			retryDirtyAlertRuntimeState(r)
		}
	}

	// 站点访问和 SSL/CDN 探测可能等待网络。先在后台启动，让资源与服务规则
	// 立即评估；轮到对应规则时再接收结果，避免网络等待串行叠加。
	var siteResultCh chan alertCheckResult
	if runtimeReady["alert_site"] && isRuleEnabled("alert_site") {
		siteResultCh = make(chan alertCheckResult, 1)
		go func() {
			siteResultCh <- checkSitesState()
		}()
	}
	var sslResultCh chan alertCheckResult
	if runtimeReady["alert_ssl"] && isRuleEnabled("alert_ssl") {
		sslResultCh = make(chan alertCheckResult, 1)
		go func() {
			sslResultCh <- checkSSLState()
		}()
	}

	cfg := GetSMTPConfig()
	hasSMTP := cfg != nil && cfg.Host != "" && cfg.AdminEmail != ""

	wCfg := GetWebhookConfig()
	hasWebhook := webhookConfigured(wCfg)

	for _, r := range m.rules {
		if !runtimeReady[r.key] {
			continue
		}
		if !isRuleEnabled(r.key) {
			disableAlertRule(r)
			continue
		}

		var result alertCheckResult
		if r.key == "alert_site" && siteResultCh != nil {
			result = <-siteResultCh
		} else if r.key == "alert_ssl" && sslResultCh != nil {
			result = <-sslResultCh
		} else {
			result = r.evaluate()
		}
		processAlertCheckResult(r, result, time.Now(), hasSMTP, hasWebhook)
	}
}

func processAlertCheckResult(r *alertRule, result alertCheckResult, now time.Time, hasSMTP, hasWebhook bool) {
	if r == nil {
		return
	}
	if result.state == alertCheckUnknown {
		// Unknown is deliberately not normal: retain firing/pending state and
		// open alert rows until a successful check observes recovery.
		reportAlertStateError("告警检查状态未知 [%s]，保留原状态: %v", r.key, result.err)
		return
	}
	instantFiring := result.state == alertCheckFiring
	msg := result.message
	if r.eventOnly {
		if instantFiring {
			sendAlertMilestone(r.key, msg)
		}
		return
	}
	wasPending := !r.pendingSince.IsZero()
	firing := r.sustainedFiring(instantFiring, now)
	if firing && !r.firing {
		// Transition: normal → alert
		r.firing = true
		r.lastFired = now
		r.lastAlertMsg = msg
		sendAlertNotificationWithChannels(r.key, msg, false, hasSMTP, hasWebhook)
		persistAlertRuntimeState(r)
	} else if !firing && r.firing {
		// Transition: alert → normal
		r.firing = false
		recoveryDetail := buildRecoveryDetail(r)
		if r.sendRecovery {
			logAlert(r.key, "info", recoveryDetail)
		}
		_ = resolveOpenAlertRows(r)
		// 即时告警（无阈值）直接发送恢复通知，有阈值的等 5 分钟防抖
		sendRecovery := now.Sub(r.lastFired) > 5*time.Minute || r.thresholdDuration <= 0
		if r.sendRecovery && hasSMTP && sendRecovery {
			go SendMail("", getPanelTitle()+" 恢复通知", formatEmailHTML(alertLabel(r.key)+" 已恢复正常", recoveryDetail, getEmailTip(r.key, true), false))
		}
		if r.sendRecovery && hasWebhook && sendRecovery {
			go SendWebhook(getPanelTitle()+" 恢复通知", recoveryDetail)
		}
		persistAlertRuntimeState(r)
	} else if firing && r.firing {
		r.lastAlertMsg = msg
		// Continuous alert — re-send on each rule's interval.
		if now.Sub(r.lastFired) > alertResendInterval(r.key) {
			r.lastFired = now
			sendAlertNotificationWithChannels(r.key, msg, true, hasSMTP, hasWebhook)
			persistAlertRuntimeState(r)
		}
	} else if wasPending != !r.pendingSince.IsZero() || (!firing && !r.pendingSince.IsZero()) {
		persistAlertRuntimeState(r)
	}
}

func retryDirtyAlertRuntimeState(r *alertRule) {
	if r == nil {
		return
	}
	if r.resolutionDirty && !r.firing {
		_ = resolveOpenAlertRows(r)
	}
	if r.runtimeStateDirty {
		_ = persistAlertRuntimeState(r)
	}
}

func resolveOpenAlertRows(r *alertRule) error {
	if r == nil || r.eventOnly {
		return nil
	}
	db := database.GetDB()
	if db == nil {
		r.resolutionDirty = true
		err := errors.New("database unavailable")
		reportAlertStateError("关闭未解决告警失败 [%s]，下一轮将重试: %v", r.key, err)
		return err
	}
	_, err := db.Exec("UPDATE alert_log SET resolved = 1 WHERE alert_type = ? AND resolved = 0", r.key)
	r.resolutionDirty = err != nil
	if err != nil {
		reportAlertStateError("关闭未解决告警失败 [%s]，下一轮将重试: %v", r.key, err)
	}
	return err
}

func disableAlertRule(r *alertRule) {
	if r == nil {
		return
	}
	wasActive := r.firing || !r.pendingSince.IsZero()
	r.firing = false
	r.pendingSince = time.Time{}
	if !wasActive {
		return
	}
	_ = resolveOpenAlertRows(r)
	_ = persistAlertRuntimeState(r)
}

func (r *alertRule) sustainedFiring(instantFiring bool, now time.Time) bool {
	if r.thresholdDuration <= 0 {
		if !instantFiring {
			r.pendingSince = time.Time{}
		}
		return instantFiring
	}
	if !instantFiring {
		r.pendingSince = time.Time{}
		return false
	}
	if r.pendingSince.IsZero() {
		r.pendingSince = now
		return false
	}
	return now.Sub(r.pendingSince) >= r.thresholdDuration
}

func alertResendInterval(key string) time.Duration {
	switch key {
	case "alert_cpu", "alert_memory", "alert_disk":
		return 2 * time.Hour
	case "alert_service", "alert_site":
		return 6 * time.Hour
	case "alert_ssl", "alert_backup", "alert_remote_backup", "alert_cron_fail",
		"alert_system_update", "alert_panel_update":
		return 24 * time.Hour
	case "alert_wp_fake_search_bot":
		// 判定条件本身就是"过去 24 小时内达到阈值"的滚动窗口，只要攻击没有停止，
		// 这个条件会持续成立一整天；用默认的 30 分钟重发会在攻击期间连续发出
		// 几十封"持续中"邮件，这里和系统/面板更新一样按 24 小时重发一次。
		return 24 * time.Hour
	}
	return 30 * time.Minute
}

func loadAlertRuntimeState(rules []*alertRule) {
	for _, r := range rules {
		_ = ensureAlertRuntimeStateLoaded(r)
	}
}

func ensureAlertRuntimeStateLoaded(r *alertRule) bool {
	if r == nil {
		return false
	}
	if r.runtimeStateLoaded {
		return true
	}
	if r.eventOnly {
		r.runtimeStateLoaded = true
		return true
	}

	db := database.GetDB()
	if db == nil {
		reportAlertStateError("读取告警运行状态失败 [%s]: database unavailable", r.key)
		return false
	}
	var status, pending, fired, message string
	err := db.QueryRow(`SELECT status, pending_since, last_fired_at, last_message
		FROM alert_runtime_state WHERE alert_type = ?`, r.key).Scan(&status, &pending, &fired, &message)
	if errors.Is(err, sql.ErrNoRows) {
		r.runtimeStateLoaded = true
		return true
	}
	if err != nil {
		reportAlertStateError("读取告警运行状态失败 [%s]: %v", r.key, err)
		return false
	}
	if status != "normal" && status != "pending" && status != "firing" {
		reportAlertStateError("读取告警运行状态失败 [%s]: invalid status %q", r.key, status)
		return false
	}
	pendingSince, err := parseOptionalAlertStateTime(pending)
	if err != nil {
		reportAlertStateError("读取告警运行状态失败 [%s]: invalid pending time: %v", r.key, err)
		return false
	}
	lastFired, err := parseOptionalAlertStateTime(fired)
	if err != nil {
		reportAlertStateError("读取告警运行状态失败 [%s]: invalid firing time: %v", r.key, err)
		return false
	}

	r.firing = status == "firing"
	r.pendingSince = pendingSince
	r.lastFired = lastFired
	r.lastAlertMsg = message
	r.runtimeStateLoaded = true
	return true
}

func persistAlertRuntimeState(r *alertRule) error {
	if r == nil || r.eventOnly {
		return nil
	}
	if r.resolutionDirty && !r.firing {
		r.runtimeStateDirty = true
		err := errors.New("open alert rows are not resolved yet")
		reportAlertStateError("保存告警运行状态延后 [%s]，等待未解决告警关闭: %v", r.key, err)
		return err
	}
	db := database.GetDB()
	if db == nil {
		r.runtimeStateDirty = true
		err := errors.New("database unavailable")
		reportAlertStateError("保存告警运行状态失败 [%s]，下一轮将重试: %v", r.key, err)
		return err
	}
	status := "normal"
	if r.firing {
		status = "firing"
	} else if !r.pendingSince.IsZero() {
		status = "pending"
	}
	_, err := db.Exec(`INSERT INTO alert_runtime_state
		(alert_type, status, pending_since, last_fired_at, last_message, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(alert_type) DO UPDATE SET status=excluded.status,
		pending_since=excluded.pending_since, last_fired_at=excluded.last_fired_at,
		last_message=excluded.last_message, updated_at=CURRENT_TIMESTAMP`,
		r.key, status, formatAlertStateTime(r.pendingSince), formatAlertStateTime(r.lastFired), r.lastAlertMsg)
	r.runtimeStateDirty = err != nil
	if err != nil {
		reportAlertStateError("保存告警运行状态失败 [%s]，下一轮将重试: %v", r.key, err)
	}
	return err
}

func formatAlertStateTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseAlertStateTime(raw string) time.Time {
	t, _ := parseOptionalAlertStateTime(raw)
	return t
}

func parseOptionalAlertStateTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func sendAlertMilestone(key, message string) {
	smtp := GetSMTPConfig()
	hasSMTP := smtp != nil && smtp.Host != "" && smtp.AdminEmail != ""
	webhook := GetWebhookConfig()
	hasWebhook := webhookConfigured(webhook)
	logAlertEvent(key, "critical", message)
	deliverAlertNotification(key, message, false, hasSMTP, hasWebhook)
}

func sendAlertNotificationWithChannels(key, message string, ongoing, hasSMTP, hasWebhook bool) {
	logAlert(key, "critical", message)
	deliverAlertNotification(key, message, ongoing, hasSMTP, hasWebhook)
}

func deliverAlertNotification(key, message string, ongoing, hasSMTP, hasWebhook bool) {
	label := alertLabel(key)
	title := getPanelTitle() + " 告警 — " + label
	emailTitle := label
	if ongoing {
		title += "（持续中）"
		emailTitle += "（持续中）"
	}
	if hasSMTP {
		go SendMail("", title, formatEmailHTML(emailTitle, message, getEmailTip(key, false), true))
	}
	if hasWebhook {
		go SendWebhook(title, message)
	}
}

func sendResolvedAlertEvent(key, title, message, tip string) {
	logAlertEvent(key, "critical", message)
	subject := getPanelTitle() + " 告警 — " + title
	if cfg := GetSMTPConfig(); cfg != nil && cfg.Host != "" && cfg.AdminEmail != "" {
		go SendMail("", subject, formatEmailHTML(title, message, tip, true))
	}
	if cfg := GetWebhookConfig(); webhookConfigured(cfg) {
		go SendWebhook(subject, message)
	}
}

func isAlertRuntimeFiring(key string) bool {
	if database.GetDB() == nil {
		return false
	}
	var status string
	if err := database.GetDB().QueryRow("SELECT status FROM alert_runtime_state WHERE alert_type = ?", key).Scan(&status); err != nil {
		return false
	}
	return status == "firing"
}

func isRuleEnabled(key string) bool {
	var v string
	database.GetDB().QueryRow("SELECT svalue FROM security_settings WHERE skey = ?", key).Scan(&v)
	return v != "false"
}

func alertLabel(key string) string {
	switch key {
	case "alert_wp_admin_change":
		return "WordPress 管理员变化"
	case "alert_wp_post_volume":
		return "WordPress 文章发布量异常"
	case "alert_wp_content_change":
		return "WordPress 内容变化"
	case "alert_wp_content_volume":
		return "WordPress 内容修改量异常"
	case "alert_wp_setting_change":
		return "WordPress 关键设置变化"
	case "alert_wp_application_password":
		return "WordPress 应用程序密码异常"
	case "alert_wp_database_object":
		return "WordPress 数据库持久化异常"
	case "alert_cpu":
		return "CPU 高负载"
	case "alert_memory":
		return "可用内存不足"
	case "alert_disk":
		return "磁盘空间不足"
	case "alert_service":
		return "服务进程异常"
	case "alert_ssl":
		return "SSL 证书即将到期"
	case "alert_backup":
		return "数据库备份失败"
	case "alert_website_expiry":
		return "网站即将到期"
	case "alert_remote_backup":
		return "远程备份失败"
	case "alert_cron_fail":
		return "计划任务执行失败"
	case "alert_site":
		return "网站不可用"
	case "alert_system_update":
		return "系统有可用更新"
	case "alert_panel_update":
		return "面板有新版本"
	case "alert_wp_fake_search_bot":
		return "伪装搜索引擎爬虫"
	case "alert_wp_code_integrity":
		return "WordPress 代码完整性变化"
	}
	return key
}

func logAlert(alertType, level, message string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	db.Exec("INSERT INTO alert_log (alert_type, level, message) VALUES (?, ?, ?)", alertType, level, message)
	// 告警历史按时间保留，避免高频事件在数小时内挤掉所有上下文。
	db.Exec("DELETE FROM alert_log WHERE created_at < datetime('now', '-90 days')")
}

func logAlertEvent(alertType, level, message string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	db.Exec("INSERT INTO alert_log (alert_type, level, message, resolved) VALUES (?, ?, ?, 1)", alertType, level, message)
	db.Exec("DELETE FROM alert_log WHERE created_at < datetime('now', '-90 days')")
}

func getEmailTip(key string, isRecovery bool) string {
	if isRecovery {
		return ""
	}
	switch key {
	case "alert_cpu":
		return "请在面板查看资源趋势和异常流量。"
	case "alert_memory":
		return "请检查高占用进程和访问流量。"
	case "alert_disk":
		return "请清理无用备份或日志，并确认磁盘余量。"
	case "alert_service":
		return "请查看服务日志和自动恢复结果。"
	case "alert_ssl":
		return "请完成续签，或手动上传有效证书。"
	case "alert_backup":
		return "请检查自动备份记录并确认下一次备份成功。"
	case "alert_website_expiry":
		return "请及时续期或备份网站数据。"
	case "alert_remote_backup":
		return "请检查远程连接，并确认失败文件重新同步成功。"
	case "alert_cron_fail":
		return "请查看任务日志并确认下一次执行成功。"
	case "alert_site":
		return "请检查域名解析、服务器状态和网站程序。"
	case "alert_system_update":
		return "请在合适的维护窗口执行系统更新。"
	case "alert_panel_update":
		return "请在面板设置页查看并执行更新。"
	case "alert_wp_fake_search_bot":
		return "面板不会自动封禁；请在安全防御页面核对来源。"
	}
	return ""
}

func extractDomains(msg string) string {
	parts := strings.Split(msg, "；")
	var domains []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if idx := strings.Index(p, " "); idx > 0 {
			domains = append(domains, p[:idx])
		}
	}
	return strings.Join(domains, "、")
}

func buildRecoveryDetail(r *alertRule) string {
	if r.key == "alert_site" && r.lastAlertMsg != "" {
		domains := extractDomains(r.lastAlertMsg)
		if domains != "" {
			return domains + " 已恢复正常"
		}
	}
	if r.key == "alert_system_update" {
		return "系统所有软件包已更新完毕，当前为最新版本"
	}
	if r.key == "alert_panel_update" {
		return "面板已更新到最新版本"
	}
	return alertLabel(r.key) + " 已恢复正常"
}

func formatEmailHTML(title, detail, tip string, isAlert bool) string {
	icon := "ℹ️"
	titleColor := "#1976d2"
	if isAlert {
		icon = "⚠️"
		titleColor = "#d32f2f"
	}
	panelTitle := html.EscapeString(getPanelTitle())
	detail = html.EscapeString(detail)
	tip = html.EscapeString(tip)

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html>
<head><meta charset="UTF-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Helvetica Neue', sans-serif; max-width: 560px; margin: 0 auto; padding: 24px; color: #333;">
`)
	fmt.Fprintf(&b, `<h2 style="color: %s; margin: 0 0 16px 0; font-size: 18px;">%s %s</h2>`+"\n", titleColor, icon, title)
	fmt.Fprintf(&b, `<p style="font-size: 15px; line-height: 1.7; margin: 0 0 24px 0; color: #444;">%s</p>`+"\n", detail)
	if tip != "" {
		b.WriteString(`<hr style="border: none; border-top: 1px solid #e0e0e0; margin: 24px 0;">` + "\n")
		fmt.Fprintf(&b, `<p style="font-size: 13px; line-height: 1.6; color: #888; margin: 0;">%s</p>`+"\n", tip)
	}
	fmt.Fprintf(&b, `<p style="font-size: 12px; color: #aaa; margin: 20px 0 0 0;">— 来自 %s 面板</p>`+"\n", panelTitle)
	b.WriteString(`</body>
</html>`)
	return b.String()
}

// --- Checkers ---

const (
	monitoringMetricMaxAge     = 3 * time.Minute
	monitoringMetricFutureSkew = time.Minute
)

func checkCPU() (bool, string) {
	return legacyAlertCheck(checkCPUState())
}

func checkCPUState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	var cpu, ts string
	if err := db.QueryRow("SELECT cpu_percent, recorded_at FROM monitoring_metrics ORDER BY id DESC LIMIT 1").Scan(&cpu, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return unknownAlertCheck(errors.New("CPU metrics unavailable: no samples"))
		}
		return unknownAlertCheck(fmt.Errorf("query CPU metrics: %w", err))
	}
	if err := validateMonitoringMetricTime(ts, time.Now()); err != nil {
		return unknownAlertCheck(fmt.Errorf("CPU metrics unavailable: %w", err))
	}
	v, err := strconv.ParseFloat(cpu, 64)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("parse CPU metric: %w", err))
	}
	if v > 80 {
		return firingAlertCheck(fmt.Sprintf("CPU 使用率 %.1f%%（阈值 80%%），于 %s", v, toLocalTime(ts)))
	}
	return normalAlertCheck()
}

func checkMemory() (bool, string) {
	return legacyAlertCheck(checkMemoryState())
}

func checkMemoryState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	var mem, ts string
	if err := db.QueryRow("SELECT memory_percent, recorded_at FROM monitoring_metrics ORDER BY id DESC LIMIT 1").Scan(&mem, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return unknownAlertCheck(errors.New("memory metrics unavailable: no samples"))
		}
		return unknownAlertCheck(fmt.Errorf("query memory metrics: %w", err))
	}
	if err := validateMonitoringMetricTime(ts, time.Now()); err != nil {
		return unknownAlertCheck(fmt.Errorf("memory metrics unavailable: %w", err))
	}
	v, err := strconv.ParseFloat(mem, 64)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("parse memory metric: %w", err))
	}
	if v > 90 {
		return firingAlertCheck(fmt.Sprintf("可用内存低于 10%%（当前使用率 %.1f%%），于 %s", v, toLocalTime(ts)))
	}
	return normalAlertCheck()
}

func validateMonitoringMetricTime(raw string, now time.Time) error {
	recordedAt, err := parseMonitoringMetricTime(raw)
	if err != nil {
		return err
	}
	age := now.Sub(recordedAt)
	if age > monitoringMetricMaxAge {
		return fmt.Errorf("latest sample is stale (%s old)", age.Round(time.Second))
	}
	if age < -monitoringMetricFutureSkew {
		return fmt.Errorf("latest sample is in the future by %s", (-age).Round(time.Second))
	}
	return nil
}

func parseMonitoringMetricTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		time.RFC3339Nano,
	} {
		if recordedAt, err := time.Parse(layout, raw); err == nil {
			return recordedAt, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid sample timestamp %q", raw)
}

func toLocalTime(dbTime string) string {
	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
	}
	for _, layout := range layouts {
		t, err := time.Parse(layout, dbTime)
		if err == nil {
			return t.Local().Format("2006-01-02 15:04:05")
		}
	}
	return dbTime
}

func checkDisk() (bool, string) {
	return legacyAlertCheck(checkDiskState())
}

func checkDiskState() alertCheckResult {
	out, err := exec.Command("df", "-h", "/").Output()
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("read disk usage: %w", err))
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return unknownAlertCheck(errors.New("unexpected df output"))
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return unknownAlertCheck(errors.New("unexpected df fields"))
	}
	useStr := strings.TrimSuffix(fields[4], "%")
	use, err := strconv.Atoi(useStr)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("parse disk usage: %w", err))
	}
	if use > 90 {
		return firingAlertCheck(fmt.Sprintf("磁盘使用率 %d%%（阈值 90%%），剩余 %s", use, fields[3]))
	}
	return normalAlertCheck()
}

func checkService() (bool, string) {
	return legacyAlertCheck(checkServiceState())
}

func checkServiceState() alertCheckResult {
	svcs := GetGuardStatus()
	var msgs []string
	var unreadable []string
	for _, s := range svcs {
		if s.Paused {
			continue
		}
		state := readGuardServiceState(s.ServiceName)
		if !state.valid || state.activeState == "" {
			unreadable = append(unreadable, s.ServiceName)
			continue
		}
		if !state.active {
			detail := fmt.Sprintf("已自动重启 %d 次", s.Restarts)
			if s.LastIncident != "" {
				detail += "，最近: " + s.LastIncident
			}
			msgs = append(msgs, fmt.Sprintf("%s 异常（服务未运行；%s）", s.Name, detail))
		}
	}
	if len(msgs) > 0 {
		return firingAlertCheck(strings.Join(msgs, "；"))
	}
	if len(unreadable) > 0 {
		return unknownAlertCheck(fmt.Errorf("read service state for %s", strings.Join(unreadable, ", ")))
	}
	return normalAlertCheck()
}

func checkSSL() (bool, string) {
	return legacyAlertCheck(checkSSLState())
}

const maxSSLCloudflareDetectionWorkers = 8

func checkSSLState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	rows, err := db.Query(`SELECT domain, ssl_expires_at, COALESCE(ssl_last_error, '')
		FROM websites WHERE ssl_enabled = 1 AND ssl_expires_at IS NOT NULL`)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("query SSL certificates: %w", err))
	}
	defer rows.Close()
	type sslAlertCandidate struct {
		domain           string
		message          string
		detectCloudflare bool
		cloudflare       bool
	}
	var candidates []sslAlertCandidate
	now := time.Now()
	for rows.Next() {
		var domain, lastError string
		var expiresAt time.Time
		if err := rows.Scan(&domain, &expiresAt, &lastError); err != nil {
			return unknownAlertCheck(fmt.Errorf("scan SSL certificate: %w", err))
		}
		days := int(expiresAt.Sub(now).Hours() / 24)
		message := ""
		if days < 0 {
			message = fmt.Sprintf("%s 证书已过期 %d 天", domain, -days)
		} else if days <= 14 {
			message = fmt.Sprintf("%s 证书 %d 天后到期", domain, days)
		}
		if message == "" {
			continue
		}
		detectCloudflare := false
		if strings.TrimSpace(lastError) != "" {
			message += "，自动续签未成功"
			detectCloudflare = true
		}
		candidates = append(candidates, sslAlertCandidate{domain: domain, message: message, detectCloudflare: detectCloudflare})
	}
	if err := rows.Err(); err != nil {
		return unknownAlertCheck(fmt.Errorf("iterate SSL certificates: %w", err))
	}

	detectionCount := 0
	for i := range candidates {
		if candidates[i].detectCloudflare {
			detectionCount++
		}
	}
	workerCount := detectionCount
	if workerCount > maxSSLCloudflareDetectionWorkers {
		workerCount = maxSSLCloudflareDetectionWorkers
	}
	if workerCount > 0 {
		indices := make(chan int)
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go func() {
				defer wg.Done()
				for index := range indices {
					candidates[index].cloudflare = cloudflareProxyDetector(candidates[index].domain)
				}
			}()
		}
		for i := range candidates {
			if candidates[i].detectCloudflare {
				indices <- i
			}
		}
		close(indices)
		wg.Wait()
	}

	msgs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		message := candidate.message
		if candidate.cloudflare {
			message += "。可能使用 Cloudflare；Full (strict) 仍需有效源站证书，可放行 ACME 路径或手动上传证书"
		}
		msgs = append(msgs, message)
	}
	if len(msgs) > 0 {
		return firingAlertCheck(strings.Join(msgs, "；"))
	}
	return normalAlertCheck()
}

func isLikelyCloudflareProxied(domain string) bool {
	cloudflareDetectionCache.Lock()
	if entry, ok := cloudflareDetectionCache.entries[domain]; ok && time.Since(entry.checkedAt) < 6*time.Hour {
		cloudflareDetectionCache.Unlock()
		return entry.proxied
	}
	cloudflareDetectionCache.Unlock()

	raw := cachedCloudflareRealIPRanges()
	var ranges []*net.IPNet
	for _, line := range strings.Fields(raw) {
		_, network, err := net.ParseCIDR(strings.TrimSpace(line))
		if err == nil {
			ranges = append(ranges, network)
		}
	}
	if len(ranges) == 0 {
		return cacheCloudflareDetection(domain, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := cloudflareDNSLookup(ctx, domain)
	if err != nil || len(addrs) == 0 {
		return cacheCloudflareDetection(domain, false)
	}
	for _, addr := range addrs {
		matched := false
		for _, network := range ranges {
			if network.Contains(addr.IP) {
				matched = true
				break
			}
		}
		if !matched {
			return cacheCloudflareDetection(domain, false)
		}
	}
	return cacheCloudflareDetection(domain, true)
}

func cacheCloudflareDetection(domain string, proxied bool) bool {
	cloudflareDetectionCache.Lock()
	cloudflareDetectionCache.entries[domain] = cloudflareDetectionEntry{proxied: proxied, checkedAt: time.Now()}
	cloudflareDetectionCache.Unlock()
	return proxied
}

func checkBackup() (bool, string) {
	return legacyAlertCheck(checkBackupState())
}

func checkBackupState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	rows, err := db.Query(`SELECT w.domain FROM backup_settings bs
		JOIN websites w ON w.id = bs.site_id
		WHERE bs.enabled = 1
		AND w.status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM site_migration_locks ml
			WHERE ml.site_id = bs.site_id AND ml.status = 'active'
		)
		AND EXISTS (
			SELECT 1 FROM db_backups b
			WHERE b.site_id = bs.site_id AND b.auto = 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM db_backups b
			WHERE b.site_id = bs.site_id AND b.auto = 1
			AND b.created_at > datetime('now', '-1 day', '-5 minutes')
		)
		ORDER BY w.domain`)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("query backup freshness: %w", err))
	}
	defer rows.Close()

	var domains []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return unknownAlertCheck(fmt.Errorf("scan backup freshness: %w", err))
		}
		domains = append(domains, d)
	}
	if err := rows.Err(); err != nil {
		return unknownAlertCheck(fmt.Errorf("iterate backup freshness: %w", err))
	}
	if len(domains) > 0 {
		return firingAlertCheck(strings.Join(domains, "、") + " 最近 24 小时内没有成功的自动备份")
	}
	return normalAlertCheck()
}

func checkWebsiteExpiry() (bool, string) {
	db := database.GetDB()
	rows, err := db.Query(`SELECT domain, expires_at FROM websites WHERE expires_at IS NOT NULL AND expires_at > datetime('now')`)
	if err != nil {
		return false, ""
	}
	type expiryMilestone struct {
		domain    string
		expiresAt time.Time
		days      int
	}
	var candidates []expiryMilestone
	now := time.Now()
	milestones := map[int]bool{14: true, 7: true, 3: true, 1: true}

	for rows.Next() {
		var domain string
		var expiresAt time.Time
		if rows.Scan(&domain, &expiresAt) != nil {
			continue
		}
		days := int(expiresAt.Sub(now).Hours() / 24)
		if !milestones[days] {
			continue
		}
		candidates = append(candidates, expiryMilestone{domain: domain, expiresAt: expiresAt, days: days})
	}
	rows.Close()

	var msgs []string
	for _, candidate := range candidates {
		eventKey := candidate.domain + "|" + candidate.expiresAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.Itoa(candidate.days)
		result, insertErr := db.Exec(`INSERT OR IGNORE INTO alert_event_markers (alert_type, event_key)
			VALUES ('alert_website_expiry', ?)`, eventKey)
		if insertErr != nil {
			continue
		}
		inserted, rowsErr := result.RowsAffected()
		if rowsErr != nil || inserted == 0 {
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s %d 天后到期", candidate.domain, candidate.days))
	}
	db.Exec("DELETE FROM alert_event_markers WHERE created_at < datetime('now', '-2 years')")
	if len(msgs) > 0 {
		return true, strings.Join(msgs, "；")
	}
	return false, ""
}

func checkRemoteBackup() (bool, string) {
	return legacyAlertCheck(checkRemoteBackupState())
}

func checkRemoteBackupState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	var enabled int
	if err := db.QueryRow("SELECT enabled FROM remote_backup_settings WHERE id = 1").Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return unknownAlertCheck(errors.New("remote backup settings are missing"))
		}
		return unknownAlertCheck(fmt.Errorf("query remote backup settings: %w", err))
	}
	if enabled == 0 {
		return normalAlertCheck()
	}

	// 失败记录只有在对应备份后续同步成功、transport_status 被更新后才算恢复。
	var failCount int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM db_backups WHERE transport_status = 'failed') +
		(SELECT COUNT(*) FROM file_backups WHERE transport_status = 'failed')`).Scan(&failCount); err != nil {
		return unknownAlertCheck(fmt.Errorf("query remote backup failures: %w", err))
	}
	if failCount > 0 {
		return firingAlertCheck(fmt.Sprintf("有 %d 个远程备份文件同步失败", failCount))
	}
	return normalAlertCheck()
}

func checkCronFail() (bool, string) {
	return legacyAlertCheck(checkCronFailState())
}

func checkCronFailState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	rows, err := db.Query(`SELECT name FROM cron_jobs
		WHERE enabled = 1 AND notify_fail = 1 AND running = 0
		AND last_status = 'failed'`)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("query failed cron jobs: %w", err))
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return unknownAlertCheck(fmt.Errorf("scan failed cron job: %w", err))
		}
		names = append(names, "「"+name+"」")
	}
	if err := rows.Err(); err != nil {
		return unknownAlertCheck(fmt.Errorf("iterate failed cron jobs: %w", err))
	}
	if len(names) > 0 {
		return firingAlertCheck("计划任务 " + strings.Join(names, "、") + " 执行失败")
	}
	return normalAlertCheck()
}

const siteFailureAlertThreshold = 2

var siteLastCheck = make(map[string]time.Time)
var siteFailureMessages = make(map[string]string)
var siteFailureCounts = make(map[string]int)

func checkSites() (bool, string) {
	return legacyAlertCheck(checkSitesState())
}

func checkSitesState() alertCheckResult {
	db := database.GetDB()
	if db == nil {
		return unknownAlertCheck(errors.New("database unavailable"))
	}
	rows, err := db.Query(`SELECT id, domain, ssl_enabled, monitoring_interval FROM websites WHERE status = 'active' AND monitoring_enabled = 1`)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("query monitored sites: %w", err))
	}
	defer rows.Close()

	type siteInfo struct {
		id       string
		domain   string
		ssl      int
		interval int
	}
	var sites []siteInfo
	seen := make(map[string]bool)
	for rows.Next() {
		var s siteInfo
		if err := rows.Scan(&s.id, &s.domain, &s.ssl, &s.interval); err != nil {
			return unknownAlertCheck(fmt.Errorf("scan monitored site: %w", err))
		}
		seen[s.id] = true
		if s.interval <= 0 {
			s.interval = 5
		}
		sites = append(sites, s)
	}
	if err := rows.Err(); err != nil {
		return unknownAlertCheck(fmt.Errorf("iterate monitored sites: %w", err))
	}

	for id := range siteFailureMessages {
		if !seen[id] {
			delete(siteFailureMessages, id)
			delete(siteFailureCounts, id)
		}
	}
	if len(siteLastCheck) > 100 {
		for id := range siteLastCheck {
			if !seen[id] {
				delete(siteLastCheck, id)
			}
		}
	}

	type checkTarget struct {
		id     string
		domain string
		url    string
	}
	var toCheck []checkTarget
	var msgs []string
	var unconfirmedFailures []string
	for _, s := range sites {
		if last, ok := siteLastCheck[s.id]; ok && time.Since(last) < time.Duration(s.interval)*time.Minute {
			if msg, ok := siteFailureMessages[s.id]; ok {
				if siteFailureCounts[s.id] >= siteFailureAlertThreshold {
					msgs = append(msgs, msg)
				} else if siteFailureCounts[s.id] > 0 {
					unconfirmedFailures = append(unconfirmedFailures, msg)
				}
			}
			continue
		}
		siteLastCheck[s.id] = time.Now()
		proto := "http"
		if s.ssl == 1 {
			proto = "https"
		}
		url := proto + "://" + s.domain + "/?wp_hc=" + strconv.FormatInt(time.Now().Unix(), 10)
		toCheck = append(toCheck, checkTarget{id: s.id, domain: s.domain, url: url})
	}

	if len(toCheck) == 0 {
		if len(msgs) > 0 {
			return firingAlertCheck(strings.Join(msgs, "；"))
		}
		if len(unconfirmedFailures) > 0 {
			return unknownAlertCheck(fmt.Errorf("site availability failure awaiting confirmation: %s", strings.Join(unconfirmedFailures, "；")))
		}
		return normalAlertCheck()
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	type result struct {
		id     string
		domain string
		code   int
		err    error
	}
	resultCh := make(chan result, len(toCheck))
	for _, t := range toCheck {
		go func(t checkTarget) {
			resp, err := httpClient.Get(t.url)
			if err != nil {
				resultCh <- result{id: t.id, domain: t.domain, err: err}
				return
			}
			resp.Body.Close()
			resultCh <- result{id: t.id, domain: t.domain, code: resp.StatusCode}
		}(t)
	}

	for range toCheck {
		r := <-resultCh
		if r.err != nil {
			msg := fmt.Sprintf("%s 无法访问 (%v)", r.domain, r.err)
			siteFailureMessages[r.id] = msg
			siteFailureCounts[r.id]++
			if siteFailureCounts[r.id] >= siteFailureAlertThreshold {
				msgs = append(msgs, msg)
			} else {
				unconfirmedFailures = append(unconfirmedFailures, msg)
			}
		} else if r.code < 200 || r.code >= 400 {
			msg := fmt.Sprintf("%s 返回 %d", r.domain, r.code)
			siteFailureMessages[r.id] = msg
			siteFailureCounts[r.id]++
			if siteFailureCounts[r.id] >= siteFailureAlertThreshold {
				msgs = append(msgs, msg)
			} else {
				unconfirmedFailures = append(unconfirmedFailures, msg)
			}
		} else {
			delete(siteFailureMessages, r.id)
			delete(siteFailureCounts, r.id)
		}
	}

	if len(msgs) > 0 {
		return firingAlertCheck(strings.Join(msgs, "；"))
	}
	if len(unconfirmedFailures) > 0 {
		return unknownAlertCheck(fmt.Errorf("site availability failure awaiting confirmation: %s", strings.Join(unconfirmedFailures, "；")))
	}
	return normalAlertCheck()
}

var sysUpdateCache struct {
	mu     sync.Mutex
	lastAt time.Time
	names  []string
}

var panelUpdateCache struct {
	mu      sync.Mutex
	lastAt  time.Time
	latest  string
	message string
}

const systemUpdateCommandTimeout = 2 * time.Minute

func newSystemUpdateCommand(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "apt", "list", "--upgradable")
}

var runSystemUpdateCommand = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), systemUpdateCommandTimeout)
	defer cancel()
	return newSystemUpdateCommand(ctx).Output()
}

var fetchLatestPanelReleaseForAlert = FetchLatestPanelRelease

func ClearSystemUpdateAlertCache() {
	sysUpdateCache.mu.Lock()
	sysUpdateCache.lastAt = time.Time{}
	sysUpdateCache.names = nil
	sysUpdateCache.mu.Unlock()
}

func ClearPanelUpdateAlertCache() {
	panelUpdateCache.mu.Lock()
	panelUpdateCache.lastAt = time.Time{}
	panelUpdateCache.latest = ""
	panelUpdateCache.message = ""
	panelUpdateCache.mu.Unlock()
}

func checkSystemUpdate() (bool, string) {
	return legacyAlertCheck(checkSystemUpdateState())
}

func checkSystemUpdateState() alertCheckResult {
	sysUpdateCache.mu.Lock()
	if time.Since(sysUpdateCache.lastAt) < 24*time.Hour {
		names := sysUpdateCache.names
		sysUpdateCache.mu.Unlock()
		if len(names) > 0 {
			return firingAlertCheck(fmt.Sprintf("系统有 %d 个可用更新：%s", len(names), strings.Join(names, "、")))
		}
		return normalAlertCheck()
	}
	sysUpdateCache.mu.Unlock()

	out, err := runSystemUpdateCommand()
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("list system updates: %w", err))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Listing...") {
			continue
		}
		parts := strings.SplitN(line, "/", 2)
		if len(parts) > 0 {
			names = append(names, parts[0])
		}
	}

	sysUpdateCache.mu.Lock()
	sysUpdateCache.lastAt = time.Now()
	sysUpdateCache.names = names
	sysUpdateCache.mu.Unlock()

	if len(names) > 0 {
		return firingAlertCheck(fmt.Sprintf("系统有 %d 个可用更新：%s", len(names), strings.Join(names, "、")))
	}
	return normalAlertCheck()
}

func checkPanelUpdate() (bool, string) {
	return legacyAlertCheck(checkPanelUpdateState())
}

func checkPanelUpdateState() alertCheckResult {
	if panelCurrentVersion == "" || panelCurrentVersion == "dev" {
		return normalAlertCheck()
	}

	panelUpdateCache.mu.Lock()
	if time.Since(panelUpdateCache.lastAt) < 24*time.Hour {
		msg := panelUpdateCache.message
		panelUpdateCache.mu.Unlock()
		if msg != "" {
			return firingAlertCheck(msg)
		}
		return normalAlertCheck()
	}
	panelUpdateCache.mu.Unlock()

	latest, err := fetchLatestPanelReleaseForAlert("")
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("fetch panel release metadata: %w", err))
	}
	if latest == nil || latest.TagName == "" {
		return unknownAlertCheck(errors.New("fetch panel release metadata: empty release"))
	}

	msg := ""
	if CompareVersions(latest.TagName, panelCurrentVersion) > 0 {
		msg = fmt.Sprintf("面板有新版本 %s 可用，当前版本 %s。建议尽快到面板设置页更新，避免跨多个版本升级。", latest.TagName, panelCurrentVersion)
	}

	panelUpdateCache.mu.Lock()
	panelUpdateCache.lastAt = time.Now()
	panelUpdateCache.latest = latest.TagName
	panelUpdateCache.message = msg
	panelUpdateCache.mu.Unlock()

	if msg != "" {
		return firingAlertCheck(msg)
	}
	return normalAlertCheck()
}

// 方案 D 阶段四：SQL 注入探测 / 伪装搜索引擎爬虫告警，默认关闭。
// 只做"记录 → 超过阈值提醒管理员"，不自动封禁，最终是否封禁仍由管理员在
// 「安全防御」页面手动决定。
//
// threshold / window 通过 security_settings 表中的 alert_wp_security_threshold
// 与 alert_wp_security_window_hours 配置（审核优化项 3.1）；这里的常量仅作为
// DB 读取失败或值非法时的 fallback。pathLimit / maxOffenders 是展示细节，
// 不对用户开放，保持硬编码。
const (
	defaultWPSecurityAlertThreshold = 10
	defaultWPSecurityAlertWindow    = 24 * time.Hour
	wpSecurityAlertPathLimit        = 3
	wpSecurityAlertMaxOffenders     = 10
)

// wpSecurityAlertConfig 汇总一次告警判定所需的全部参数。
type wpSecurityAlertConfig struct {
	threshold    int
	window       time.Duration
	pathLimit    int
	maxOffenders int
}

func defaultWPSecurityAlertConfigValue() wpSecurityAlertConfig {
	return wpSecurityAlertConfig{
		threshold:    defaultWPSecurityAlertThreshold,
		window:       defaultWPSecurityAlertWindow,
		pathLimit:    wpSecurityAlertPathLimit,
		maxOffenders: wpSecurityAlertMaxOffenders,
	}
}

// loadWPSecurityAlertConfig is strict so a broken or unreadable configuration
// cannot be interpreted as a healthy security signal by the alert state machine.
func loadWPSecurityAlertConfig() (wpSecurityAlertConfig, error) {
	cfg := defaultWPSecurityAlertConfigValue()
	db := database.GetDB()
	if db == nil {
		return cfg, errors.New("database unavailable")
	}
	var thresholdValue string
	if err := db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'alert_wp_security_threshold'").Scan(&thresholdValue); err != nil {
		return cfg, fmt.Errorf("read WordPress security alert threshold: %w", err)
	}
	threshold, err := strconv.Atoi(thresholdValue)
	if err != nil || threshold < 1 || threshold > 10000 {
		return cfg, fmt.Errorf("invalid WordPress security alert threshold %q", thresholdValue)
	}
	cfg.threshold = threshold

	var windowValue string
	if err := db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'alert_wp_security_window_hours'").Scan(&windowValue); err != nil {
		return cfg, fmt.Errorf("read WordPress security alert window: %w", err)
	}
	windowHours, err := strconv.Atoi(windowValue)
	if err != nil || windowHours < 1 || windowHours > 168 {
		return cfg, fmt.Errorf("invalid WordPress security alert window %q", windowValue)
	}
	cfg.window = time.Duration(windowHours) * time.Hour
	return cfg, nil
}

// getWPSecurityAlertConfig keeps the legacy fallback contract used outside the
// stateful alert path. The state checker itself uses the strict loader above.
func getWPSecurityAlertConfig() wpSecurityAlertConfig {
	cfg, err := loadWPSecurityAlertConfig()
	if err != nil {
		return defaultWPSecurityAlertConfigValue()
	}
	return cfg
}

func checkWPFakeSearchBotThreshold() (bool, string) {
	return legacyAlertCheck(checkWPFakeSearchBotThresholdState())
}

func checkWPFakeSearchBotThresholdState() alertCheckResult {
	return checkWPSecurityEventThresholdState(SecurityEventFakeSearchBot, "伪装搜索引擎爬虫")
}

func checkWPSecurityEventThreshold(eventType, label string) (bool, string) {
	return legacyAlertCheck(checkWPSecurityEventThresholdState(eventType, label))
}

func checkWPSecurityEventThresholdState(eventType, label string) alertCheckResult {
	cfg, err := loadWPSecurityAlertConfig()
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("load WordPress security alert configuration: %w", err))
	}
	since := time.Now().UTC().Add(-cfg.window)
	offenders, omitted, err := queryWPSecurityOffendersForAlert(eventType, since, cfg.threshold, cfg.pathLimit, cfg.maxOffenders)
	if err != nil {
		return unknownAlertCheck(fmt.Errorf("query WordPress security offenders: %w", err))
	}
	if len(offenders) == 0 {
		return normalAlertCheck()
	}

	// 分布式扫描可能同时有几十上百个 IP 超过阈值，不截断的话邮件正文会膨胀到
	// 几百 KB，Webhook 走 URL 路径段的渠道（如 Bark）还会直接投递失败。
	// 告警专用查询已在 SQL 层按次数排序并限制为影响最大的前 N 个，并且只为
	// 这些最终展示的 IP 查询热门路径，避免对所有攻击来源执行逐个路径查询。
	// 邮件正文按单个 <p> 段落渲染，换行符不会被转成 <br>，因此和其余告警规则一样
	// 使用「；」分隔多条信息，避免所有 IP 挤成一整行无法阅读。
	entries := make([]string, 0, len(offenders))
	for _, o := range offenders {
		paths := "（无样本）"
		if len(o.Paths) > 0 {
			paths = strings.Join(o.Paths, "、")
		}
		entries = append(entries, fmt.Sprintf("%s（%d 次，热门路径：%s）", o.IP, o.Count, paths))
	}
	suffix := "。"
	if omitted > 0 {
		suffix = fmt.Sprintf("（还有 %d 个 IP 未列出，请到「安全防御」页面查看完整列表）。", omitted)
	}
	msg := fmt.Sprintf("过去 %d 小时内以下 IP 触发「%s」次数达到阈值（%d 次）：%s%s面板仅记录、不自动封禁，请结合 IP 来源在「安全防御」页面手动决定是否封禁。",
		int(cfg.window.Hours()), label, cfg.threshold, strings.Join(entries, "；"), suffix)
	return firingAlertCheck(msg)
}

func queryWPSecurityOffendersForAlert(eventType string, since time.Time, threshold, pathLimit, maxOffenders int) ([]WPSecurityAlertOffender, int, error) {
	db := database.GetDB()
	if db == nil {
		return nil, 0, errors.New("database unavailable")
	}
	if maxOffenders <= 0 {
		return nil, 0, errors.New("maximum offender count must be positive")
	}
	formattedSince := since.UTC().Format("2006-01-02 15:04:05")

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT ip_address FROM wp_security_events
		WHERE event_type = ? AND occurred_at >= ?
		GROUP BY ip_address HAVING COUNT(*) >= ?
	)`, eventType, formattedSince, threshold).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count offenders: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := db.Query(`SELECT ip_address, COUNT(*) AS hit_count FROM wp_security_events
		WHERE event_type = ? AND occurred_at >= ?
		GROUP BY ip_address HAVING COUNT(*) >= ?
		ORDER BY hit_count DESC, ip_address ASC LIMIT ?`,
		eventType, formattedSince, threshold, maxOffenders)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var offenders []WPSecurityAlertOffender
	for rows.Next() {
		var offender WPSecurityAlertOffender
		if err := rows.Scan(&offender.IP, &offender.Count); err != nil {
			return nil, 0, fmt.Errorf("scan offender count: %w", err)
		}
		offenders = append(offenders, offender)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate offender counts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close offender counts: %w", err)
	}

	for i := range offenders {
		paths, err := queryWPSecurityEventPathsForAlert(db, eventType, since, offenders[i].IP, pathLimit)
		if err != nil {
			return nil, 0, fmt.Errorf("query paths for %s: %w", offenders[i].IP, err)
		}
		offenders[i].Paths = paths
	}
	omitted := total - len(offenders)
	if omitted < 0 {
		omitted = 0
	}
	return offenders, omitted, nil
}

func queryWPSecurityEventPathsForAlert(db *sql.DB, eventType string, since time.Time, ip string, limit int) ([]string, error) {
	rows, err := db.Query(`SELECT path, COUNT(*) c FROM wp_security_events
		WHERE event_type = ? AND ip_address = ? AND occurred_at >= ?
		GROUP BY path ORDER BY c DESC, path ASC LIMIT ?`,
		eventType, ip, since.UTC().Format("2006-01-02 15:04:05"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		var count int
		if err := rows.Scan(&path, &count); err != nil {
			return nil, fmt.Errorf("scan offender path: %w", err)
		}
		paths = append(paths, fmt.Sprintf("%s × %d", path, count))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate offender paths: %w", err)
	}
	return paths, nil
}

func getPanelTitle() string {
	db := database.GetDB()
	if db == nil {
		return "YUB WPanel"
	}
	var title string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'panel_title'").Scan(&title)
	if title == "" {
		return "YUB WPanel"
	}
	return title
}
