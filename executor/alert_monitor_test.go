package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zangwp/yub-wpanel/database"
)

func TestAlertRuleSustainedFiring(t *testing.T) {
	start := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	r := &alertRule{thresholdDuration: 5 * time.Minute}

	if r.sustainedFiring(true, start) {
		t.Fatal("first high sample should not alert immediately")
	}
	if r.sustainedFiring(true, start.Add(4*time.Minute+59*time.Second)) {
		t.Fatal("high duration below threshold should not alert")
	}
	if !r.sustainedFiring(true, start.Add(5*time.Minute)) {
		t.Fatal("high duration at threshold should alert")
	}
}

func TestWebhookConfiguredByURL(t *testing.T) {
	if webhookConfigured(nil) {
		t.Fatal("nil Webhook config should be disabled")
	}
	if webhookConfigured(&WebhookConfig{Channel: "wecom"}) {
		t.Fatal("Webhook config without URL should be disabled")
	}
	if !webhookConfigured(&WebhookConfig{Channel: "wecom", URL: "https://example.com/hook"}) {
		t.Fatal("Webhook config with URL should be enabled")
	}
}

func TestAlertRuleSustainedFiringResets(t *testing.T) {
	start := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	r := &alertRule{thresholdDuration: 5 * time.Minute}

	r.sustainedFiring(true, start)
	if r.sustainedFiring(false, start.Add(2*time.Minute)) {
		t.Fatal("normal sample should not alert")
	}
	if !r.pendingSince.IsZero() {
		t.Fatal("normal sample should reset pending state")
	}
	if r.sustainedFiring(true, start.Add(6*time.Minute)) {
		t.Fatal("new high period should restart the timer")
	}
}

func TestUnknownAlertCheckPreservesFiringAndPendingState(t *testing.T) {
	previousReporter := reportAlertStateError
	reportAlertStateError = func(string, ...any) {}
	t.Cleanup(func() { reportAlertStateError = previousReporter })

	pending := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	fired := pending.Add(-time.Hour)
	rule := &alertRule{
		key:          "alert_backup",
		firing:       true,
		pendingSince: pending,
		lastFired:    fired,
		lastAlertMsg: "backup is stale",
	}
	processAlertCheckResult(rule, unknownAlertCheck(errors.New("database locked")), pending.Add(time.Minute), false, false)

	if !rule.firing || !rule.pendingSince.Equal(pending) || !rule.lastFired.Equal(fired) || rule.lastAlertMsg != "backup is stale" {
		t.Fatalf("unknown check changed alert state: %+v", rule)
	}
}

func TestDatabaseAlertChecksReportUnknownOnQueryFailure(t *testing.T) {
	openAlertTestDB(t) // Deliberately leave all checker tables absent.
	tests := []struct {
		name  string
		check func() alertCheckResult
	}{
		{name: "cpu", check: checkCPUState},
		{name: "memory", check: checkMemoryState},
		{name: "ssl", check: checkSSLState},
		{name: "backup", check: checkBackupState},
		{name: "remote backup", check: checkRemoteBackupState},
		{name: "cron", check: checkCronFailState},
		{name: "sites", check: checkSitesState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.check()
			if result.state != alertCheckUnknown || result.err == nil {
				t.Fatalf("result=%+v, want unknown with error", result)
			}
		})
	}
}

func TestMetricAlertChecksReportUnknownWithoutSamples(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE monitoring_metrics (
		id INTEGER PRIMARY KEY, cpu_percent TEXT, memory_percent TEXT, recorded_at TEXT)`)
	for _, check := range []struct {
		name string
		fn   func() alertCheckResult
	}{{name: "cpu", fn: checkCPUState}, {name: "memory", fn: checkMemoryState}} {
		t.Run(check.name, func(t *testing.T) {
			result := check.fn()
			if result.state != alertCheckUnknown || result.err == nil {
				t.Fatalf("result=%+v, want unknown with an explanatory error", result)
			}
		})
	}
}

func TestMetricAlertChecksReportUnknownForStaleSamples(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE monitoring_metrics (
		id INTEGER PRIMARY KEY, cpu_percent TEXT, memory_percent TEXT, recorded_at TEXT)`)
	staleAt := time.Now().UTC().Add(-monitoringMetricMaxAge - time.Minute).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`INSERT INTO monitoring_metrics (cpu_percent, memory_percent, recorded_at)
		VALUES ('10', '20', ?)`, staleAt); err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct {
		name string
		fn   func() alertCheckResult
	}{{name: "cpu", fn: checkCPUState}, {name: "memory", fn: checkMemoryState}} {
		t.Run(check.name, func(t *testing.T) {
			result := check.fn()
			if result.state != alertCheckUnknown || result.err == nil || !strings.Contains(result.err.Error(), "stale") {
				t.Fatalf("result=%+v, want unknown stale sample", result)
			}
		})
	}
}

func TestServiceAlertUsesLiveStateAndReportsUnknownWhenUnreadable(t *testing.T) {
	previousGuard := guard
	previousCommand := guardCommand
	guard = &ProcessGuard{services: []*GuardService{{Name: "Nginx", ServiceName: "nginx"}}}
	t.Cleanup(func() {
		guard = previousGuard
		guardCommand = previousCommand
	})

	guardCommand = func(string, ...string) ([]byte, error) {
		return nil, errors.New("systemd unavailable")
	}
	if result := checkServiceState(); result.state != alertCheckUnknown || result.err == nil {
		t.Fatalf("unreadable service state=%+v, want unknown", result)
	}

	guardCommand = func(string, ...string) ([]byte, error) {
		return []byte("ActiveState=failed\nNRestarts=0\nResult=exit-code\n"), nil
	}
	result := checkServiceState()
	if result.state != alertCheckFiring || !strings.Contains(result.message, "服务未运行") {
		t.Fatalf("down service with no in-memory restart history=%+v, want firing", result)
	}
}

func TestAlertResendIntervalsByAlertClass(t *testing.T) {
	if got := alertResendInterval("alert_system_update"); got != 24*time.Hour {
		t.Fatalf("system update alert should resend daily, got %v", got)
	}
	if got := alertResendInterval("alert_panel_update"); got != 24*time.Hour {
		t.Fatalf("panel update alert should resend daily, got %v", got)
	}
	if got := alertResendInterval("alert_ssl"); got != 24*time.Hour {
		t.Fatalf("SSL alert should resend daily, got %v", got)
	}
	if got := alertResendInterval("alert_disk"); got != 2*time.Hour {
		t.Fatalf("resource alerts should resend every 2 hours, got %v", got)
	}
	if got := alertResendInterval("alert_site"); got != 6*time.Hour {
		t.Fatalf("availability alerts should resend every 6 hours, got %v", got)
	}
	if got := alertResendInterval("alert_cron_fail"); got != 24*time.Hour {
		t.Fatalf("operational alerts should resend daily, got %v", got)
	}
	if got := alertResendInterval("alert_wp_fake_search_bot"); got != 24*time.Hour {
		t.Fatalf("wp fake search bot alert should resend daily to avoid spamming during a sustained attack, got %v", got)
	}
}

func TestAlertRuntimeStateSurvivesReload(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	now := time.Date(2026, 8, 23, 3, 4, 5, 0, time.UTC)
	r := &alertRule{key: "alert_ssl", firing: true, lastFired: now, lastAlertMsg: "still expiring"}
	persistAlertRuntimeState(r)

	reloaded := &alertRule{key: "alert_ssl"}
	loadAlertRuntimeState([]*alertRule{reloaded})
	if !reloaded.firing || !reloaded.lastFired.Equal(now) || reloaded.lastAlertMsg != "still expiring" {
		t.Fatalf("runtime state not restored: %+v", reloaded)
	}
}

func TestAlertRuntimeStateRetriesAfterDatabaseRecovers(t *testing.T) {
	db := openAlertTestDB(t)
	r := &alertRule{key: "alert_ssl", firing: true, lastAlertMsg: "expiring"}
	if err := persistAlertRuntimeState(r); err == nil || !r.runtimeStateDirty {
		t.Fatalf("missing table should leave dirty state: err=%v rule=%+v", err, r)
	}
	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	retryDirtyAlertRuntimeState(r)
	if r.runtimeStateDirty {
		t.Fatalf("retry should converge after recovery: rule=%+v", r)
	}
	var status, message string
	if err := db.QueryRow(`SELECT status,last_message FROM alert_runtime_state WHERE alert_type='alert_ssl'`).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "firing" || message != "expiring" {
		t.Fatalf("status=%q message=%q", status, message)
	}
}

func TestAlertResolutionRetriesAfterDatabaseRecovers(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	previousReporter := reportAlertStateError
	reportAlertStateError = func(string, ...any) {}
	t.Cleanup(func() { reportAlertStateError = previousReporter })

	rule := &alertRule{key: "alert_ssl"}
	if err := resolveOpenAlertRows(rule); err == nil || !rule.resolutionDirty {
		t.Fatalf("missing alert_log table should leave resolution dirty: err=%v rule=%+v", err, rule)
	}
	if err := persistAlertRuntimeState(rule); err == nil || !rule.runtimeStateDirty {
		t.Fatalf("normal runtime state must wait for alert resolution: err=%v rule=%+v", err, rule)
	}
	var runtimeRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM alert_runtime_state`).Scan(&runtimeRows); err != nil {
		t.Fatal(err)
	}
	if runtimeRows != 0 {
		t.Fatalf("persisted normal state before alert resolution: rows=%d", runtimeRows)
	}
	mustExec(t, db, `CREATE TABLE alert_log (
		id INTEGER PRIMARY KEY, alert_type TEXT, resolved INTEGER DEFAULT 0)`)
	mustExec(t, db, `INSERT INTO alert_log (alert_type, resolved) VALUES ('alert_ssl', 0)`)
	retryDirtyAlertRuntimeState(rule)
	if rule.resolutionDirty {
		t.Fatalf("resolution retry should converge: rule=%+v", rule)
	}
	var resolved int
	if err := db.QueryRow(`SELECT resolved FROM alert_log WHERE alert_type='alert_ssl'`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved=%d, want 1", resolved)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM alert_runtime_state WHERE alert_type='alert_ssl'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "normal" {
		t.Fatalf("runtime status=%q, want normal", status)
	}
}

func TestAlertResolutionRetryDoesNotResolveRefiredIncident(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE alert_log (
		id INTEGER PRIMARY KEY, alert_type TEXT, resolved INTEGER DEFAULT 0)`)
	mustExec(t, db, `INSERT INTO alert_log (alert_type, resolved) VALUES ('alert_ssl', 0)`)

	rule := &alertRule{key: "alert_ssl", firing: true, resolutionDirty: true}
	retryDirtyAlertRuntimeState(rule)

	var resolved int
	if err := db.QueryRow(`SELECT resolved FROM alert_log WHERE alert_type='alert_ssl'`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 0 {
		t.Fatalf("refired incident resolved=%d, want unresolved", resolved)
	}
	if !rule.resolutionDirty {
		t.Fatal("deferred resolution must remain dirty until the firing incident recovers")
	}
}

func TestPersistAlertRuntimeStateMarksNilDatabaseDirty(t *testing.T) {
	previousDB := database.DB
	database.DB = nil
	previousReporter := reportAlertStateError
	reportAlertStateError = func(string, ...any) {}
	t.Cleanup(func() {
		database.DB = previousDB
		reportAlertStateError = previousReporter
	})

	rule := &alertRule{key: "alert_ssl", firing: true}
	if err := persistAlertRuntimeState(rule); err == nil || !rule.runtimeStateDirty {
		t.Fatalf("nil database should leave runtime state dirty: err=%v rule=%+v", err, rule)
	}
}

func TestLoadAlertRuntimeStateDistinguishesMissingRowFromReadFailure(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	previousReporter := reportAlertStateError
	var reports []string
	reportAlertStateError = func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { reportAlertStateError = previousReporter })

	missingRule := &alertRule{key: "alert_ssl"}
	loadAlertRuntimeState([]*alertRule{missingRule})
	if len(reports) != 0 {
		t.Fatalf("missing row should be normal, reports=%v", reports)
	}
	if !missingRule.runtimeStateLoaded {
		t.Fatal("a confirmed missing runtime row should mark the rule ready")
	}
	mustExec(t, db, `DROP TABLE alert_runtime_state`)
	failedRule := &alertRule{key: "alert_ssl"}
	loadAlertRuntimeState([]*alertRule{failedRule})
	if len(reports) != 1 || !strings.Contains(reports[0], "读取告警运行状态失败") {
		t.Fatalf("read failure should be reported once, reports=%v", reports)
	}
	if failedRule.runtimeStateLoaded {
		t.Fatal("a failed runtime read must keep the rule blocked")
	}
}

func TestAlertRuntimeStateLoadRetriesAfterDatabaseRecovers(t *testing.T) {
	db := openAlertTestDB(t)
	previousReporter := reportAlertStateError
	reportAlertStateError = func(string, ...any) {}
	t.Cleanup(func() { reportAlertStateError = previousReporter })

	rule := &alertRule{key: "alert_ssl"}
	if ensureAlertRuntimeStateLoaded(rule) {
		t.Fatal("rule became ready while the runtime-state table was unavailable")
	}
	if rule.runtimeStateLoaded {
		t.Fatal("failed load must remain retryable")
	}

	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	firedAt := time.Date(2026, 9, 23, 1, 2, 3, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO alert_runtime_state
		(alert_type,status,pending_since,last_fired_at,last_message)
		VALUES ('alert_ssl','firing','',?,'certificate expiring')`, formatAlertStateTime(firedAt)); err != nil {
		t.Fatal(err)
	}

	if !ensureAlertRuntimeStateLoaded(rule) {
		t.Fatal("rule did not become ready after the database recovered")
	}
	if !rule.firing || !rule.lastFired.Equal(firedAt) || rule.lastAlertMsg != "certificate expiring" {
		t.Fatalf("persisted firing state was not restored: %+v", rule)
	}
}

func TestRemoteBackupMissingSingletonIsUnknown(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE remote_backup_settings (
		id INTEGER PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 0)`)

	result := checkRemoteBackupState()
	if result.state != alertCheckUnknown || result.err == nil {
		t.Fatalf("result=%+v, want unknown for missing singleton settings", result)
	}
}

func TestCheckSSLKeepsLongExpiredCertificateFiring(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		domain TEXT, ssl_enabled INTEGER, ssl_expires_at DATETIME, ssl_last_error TEXT)`)
	mustExec(t, db, `INSERT INTO websites VALUES ('expired.example', 1, datetime('now', '-30 days'), '')`)
	previousDetector := cloudflareProxyDetector
	cloudflareProxyDetector = func(string) bool { return false }
	t.Cleanup(func() { cloudflareProxyDetector = previousDetector })

	firing, msg := checkSSL()
	if !firing || !strings.Contains(msg, "已过期") {
		t.Fatalf("long-expired certificate should remain firing, got firing=%v msg=%q", firing, msg)
	}
}

func TestCheckSSLAddsConciseCloudflareGuidance(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		domain TEXT, ssl_enabled INTEGER, ssl_expires_at DATETIME, ssl_last_error TEXT)`)
	mustExec(t, db, `INSERT INTO websites VALUES ('cdn.example', 1, datetime('now', '+5 days'), 'renew failed')`)
	previousDetector := cloudflareProxyDetector
	cloudflareProxyDetector = func(string) bool { return true }
	t.Cleanup(func() { cloudflareProxyDetector = previousDetector })

	firing, msg := checkSSL()
	if !firing || !strings.Contains(msg, "自动续签未成功") || !strings.Contains(msg, "Full (strict)") || !strings.Contains(msg, "手动上传证书") {
		t.Fatalf("unexpected Cloudflare SSL guidance: firing=%v msg=%q", firing, msg)
	}
}

func TestCheckSSLRunsCloudflareDetectionsConcurrently(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		domain TEXT, ssl_enabled INTEGER, ssl_expires_at DATETIME, ssl_last_error TEXT)`)
	mustExec(t, db, `INSERT INTO websites VALUES
		('one.example', 1, datetime('now', '+5 days'), 'failed'),
		('two.example', 1, datetime('now', '+5 days'), 'failed'),
		('three.example', 1, datetime('now', '+5 days'), 'failed')`)
	previousDetector := cloudflareProxyDetector
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	cloudflareProxyDetector = func(string) bool {
		started <- struct{}{}
		<-release
		return false
	}
	t.Cleanup(func() { cloudflareProxyDetector = previousDetector })

	done := make(chan struct{})
	go func() {
		checkSSL()
		close(done)
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("Cloudflare detections ran serially")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("checkSSL did not finish after concurrent detections were released")
	}
}

func TestCheckSSLLimitsCloudflareDetectionWorkers(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		domain TEXT, ssl_enabled INTEGER, ssl_expires_at DATETIME, ssl_last_error TEXT)`)
	total := maxSSLCloudflareDetectionWorkers + 5
	for i := 0; i < total; i++ {
		if _, err := db.Exec(`INSERT INTO websites VALUES (?, 1, datetime('now', '+5 days'), 'failed')`, fmt.Sprintf("site-%d.example", i)); err != nil {
			t.Fatal(err)
		}
	}

	previousDetector := cloudflareProxyDetector
	started := make(chan struct{}, total)
	release := make(chan struct{})
	cloudflareProxyDetector = func(string) bool {
		started <- struct{}{}
		<-release
		return false
	}
	t.Cleanup(func() { cloudflareProxyDetector = previousDetector })

	done := make(chan alertCheckResult, 1)
	go func() { done <- checkSSLState() }()
	for i := 0; i < maxSSLCloudflareDetectionWorkers; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("SSL detection worker pool did not fill")
		}
	}

	overLimit := false
	select {
	case <-started:
		overLimit = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-done:
		if result.state != alertCheckFiring {
			t.Fatalf("result=%+v, want firing SSL alert", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkSSLState did not finish after workers were released")
	}
	if overLimit {
		t.Fatalf("more than %d Cloudflare detections ran concurrently", maxSSLCloudflareDetectionWorkers)
	}
	if got := maxSSLCloudflareDetectionWorkers + len(started); got != total {
		t.Fatalf("Cloudflare detection calls=%d, want %d", got, total)
	}
}

func TestCloudflareDetectionRequiresEveryResolvedAddressInOfficialRanges(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE security_settings (skey TEXT PRIMARY KEY, svalue TEXT)`)
	mustExec(t, db, `INSERT INTO security_settings VALUES ('cloudflare_realip_ips', '104.16.0.0/12 2606:4700::/32')`)
	previousLookup := cloudflareDNSLookup
	t.Cleanup(func() { cloudflareDNSLookup = previousLookup })
	resetCloudflareDetectionCache := func() {
		cloudflareDetectionCache.Lock()
		cloudflareDetectionCache.entries = make(map[string]cloudflareDetectionEntry)
		cloudflareDetectionCache.Unlock()
	}
	resetCloudflareDetectionCache()
	cloudflareDNSLookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("104.21.2.219")}, {IP: net.ParseIP("203.0.113.10")}}, nil
	}
	if isLikelyCloudflareProxied("mixed.example") {
		t.Fatal("mixed Cloudflare/origin answers must not be classified as proxied")
	}

	resetCloudflareDetectionCache()
	cloudflareDNSLookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("104.21.2.219")}, {IP: net.ParseIP("2606:4700::6815:2db")}}, nil
	}
	if !isLikelyCloudflareProxied("proxied.example") {
		t.Fatal("all-official Cloudflare answers should be classified as proxied")
	}
}

func TestCheckCronFailurePersistsUntilSuccessfulRun(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE cron_jobs (
		name TEXT, enabled INTEGER, notify_fail INTEGER, running INTEGER,
		last_status TEXT, last_run_at DATETIME)`)
	mustExec(t, db, `INSERT INTO cron_jobs VALUES ('nightly', 1, 1, 0, 'failed', datetime('now', '-2 days'))`)

	firing, _ := checkCronFail()
	if !firing {
		t.Fatal("failed cron should remain firing until a successful run")
	}
	mustExec(t, db, `UPDATE cron_jobs SET last_status='success', last_run_at=datetime('now')`)
	firing, _ = checkCronFail()
	if firing {
		t.Fatal("successful cron run should clear the alert")
	}
}

func TestWebsiteExpiryMilestoneDeduplicatesEveryDomainInMergedMessage(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (domain TEXT, expires_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE alert_event_markers (
		alert_type TEXT, event_key TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (alert_type, event_key))`)
	expiresAt := time.Now().Add(7*24*time.Hour + 12*time.Hour)
	if _, err := db.Exec(`INSERT INTO websites VALUES ('a.example', ?), ('b.example', ?)`, expiresAt, expiresAt); err != nil {
		t.Fatal(err)
	}
	var scannedExpiry time.Time
	if err := db.QueryRow("SELECT expires_at FROM websites LIMIT 1").Scan(&scannedExpiry); err != nil {
		t.Fatalf("scan expiry: %v", err)
	}
	if days := int(scannedExpiry.Sub(time.Now()).Hours() / 24); days != 7 {
		t.Fatalf("fixture days=%d expiry=%v", days, scannedExpiry)
	}

	firing, msg := checkWebsiteExpiry()
	if !firing || !strings.Contains(msg, "a.example") || !strings.Contains(msg, "b.example") {
		t.Fatalf("first milestone should merge both domains, firing=%v msg=%q", firing, msg)
	}
	firing, msg = checkWebsiteExpiry()
	if firing || msg != "" {
		t.Fatalf("second check should deduplicate every merged domain, firing=%v msg=%q", firing, msg)
	}
	var markers int
	if err := db.QueryRow("SELECT COUNT(*) FROM alert_event_markers").Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 2 {
		t.Fatalf("markers=%d, want 2", markers)
	}
}

func TestDisableAlertRuleResolvesOpenAlerts(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE alert_log (
		id INTEGER PRIMARY KEY, alert_type TEXT, level TEXT, message TEXT,
		resolved INTEGER DEFAULT 0, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)
	mustExec(t, db, `CREATE TABLE alert_runtime_state (
		alert_type TEXT PRIMARY KEY, status TEXT, pending_since TEXT,
		last_fired_at TEXT, last_message TEXT, updated_at DATETIME)`)
	mustExec(t, db, `INSERT INTO alert_log (alert_type, level, message) VALUES ('alert_cpu', 'critical', 'high')`)
	r := &alertRule{key: "alert_cpu", firing: true, lastFired: time.Now(), lastAlertMsg: "high"}
	disableAlertRule(r)

	var resolved int
	if err := db.QueryRow("SELECT resolved FROM alert_log").Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 || r.firing || !r.pendingSince.IsZero() {
		t.Fatalf("disabled rule not closed cleanly: resolved=%d rule=%+v", resolved, r)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM alert_runtime_state WHERE alert_type='alert_cpu'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "normal" {
		t.Fatalf("runtime status=%q, want normal", status)
	}
}

func TestClearSystemUpdateAlertCache(t *testing.T) {
	sysUpdateCache.mu.Lock()
	prevLastAt := sysUpdateCache.lastAt
	prevNames := sysUpdateCache.names
	sysUpdateCache.lastAt = time.Now()
	sysUpdateCache.names = []string{"openssl"}
	sysUpdateCache.mu.Unlock()
	t.Cleanup(func() {
		sysUpdateCache.mu.Lock()
		sysUpdateCache.lastAt = prevLastAt
		sysUpdateCache.names = prevNames
		sysUpdateCache.mu.Unlock()
	})

	ClearSystemUpdateAlertCache()

	sysUpdateCache.mu.Lock()
	defer sysUpdateCache.mu.Unlock()
	if !sysUpdateCache.lastAt.IsZero() {
		t.Fatalf("lastAt should be reset, got %v", sysUpdateCache.lastAt)
	}
	if sysUpdateCache.names != nil {
		t.Fatalf("names should be cleared, got %v", sysUpdateCache.names)
	}
}

func TestClearPanelUpdateAlertCache(t *testing.T) {
	panelUpdateCache.mu.Lock()
	prevLastAt := panelUpdateCache.lastAt
	prevLatest := panelUpdateCache.latest
	prevMessage := panelUpdateCache.message
	panelUpdateCache.lastAt = time.Now()
	panelUpdateCache.latest = "v1.2.3"
	panelUpdateCache.message = "panel update available"
	panelUpdateCache.mu.Unlock()
	t.Cleanup(func() {
		panelUpdateCache.mu.Lock()
		panelUpdateCache.lastAt = prevLastAt
		panelUpdateCache.latest = prevLatest
		panelUpdateCache.message = prevMessage
		panelUpdateCache.mu.Unlock()
	})

	ClearPanelUpdateAlertCache()

	panelUpdateCache.mu.Lock()
	defer panelUpdateCache.mu.Unlock()
	if !panelUpdateCache.lastAt.IsZero() {
		t.Fatalf("lastAt should be reset, got %v", panelUpdateCache.lastAt)
	}
	if panelUpdateCache.latest != "" {
		t.Fatalf("latest should be cleared, got %q", panelUpdateCache.latest)
	}
	if panelUpdateCache.message != "" {
		t.Fatalf("message should be cleared, got %q", panelUpdateCache.message)
	}
}

func TestCheckPanelUpdateUsesCachedMessage(t *testing.T) {
	panelUpdateCache.mu.Lock()
	prevLastAt := panelUpdateCache.lastAt
	prevLatest := panelUpdateCache.latest
	prevMessage := panelUpdateCache.message
	panelUpdateCache.lastAt = time.Now()
	panelUpdateCache.latest = "v1.2.3"
	panelUpdateCache.message = "面板有新版本 v1.2.3 可用，当前版本 v1.2.2。"
	panelUpdateCache.mu.Unlock()
	prevCurrent := panelCurrentVersion
	panelCurrentVersion = "v1.2.2"
	t.Cleanup(func() {
		panelUpdateCache.mu.Lock()
		panelUpdateCache.lastAt = prevLastAt
		panelUpdateCache.latest = prevLatest
		panelUpdateCache.message = prevMessage
		panelUpdateCache.mu.Unlock()
		panelCurrentVersion = prevCurrent
	})

	firing, msg := checkPanelUpdate()
	if !firing {
		t.Fatal("cached panel update message should keep alert firing")
	}
	if !strings.Contains(msg, "v1.2.3") {
		t.Fatalf("message should include cached latest version, got %q", msg)
	}
}

func TestSystemUpdateCommandRunsAptDirectly(t *testing.T) {
	cmd := newSystemUpdateCommand(context.Background())
	want := "apt list --upgradable"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("system update command=%q, want %q", got, want)
	}
}

func TestSystemUpdateCommandFailureIsUnknown(t *testing.T) {
	previousCommand := runSystemUpdateCommand
	sysUpdateCache.mu.Lock()
	previousLastAt := sysUpdateCache.lastAt
	previousNames := sysUpdateCache.names
	sysUpdateCache.lastAt = time.Time{}
	sysUpdateCache.names = nil
	sysUpdateCache.mu.Unlock()
	runSystemUpdateCommand = func() ([]byte, error) {
		return nil, errors.New("apt unavailable")
	}
	t.Cleanup(func() {
		runSystemUpdateCommand = previousCommand
		sysUpdateCache.mu.Lock()
		sysUpdateCache.lastAt = previousLastAt
		sysUpdateCache.names = previousNames
		sysUpdateCache.mu.Unlock()
	})

	result := checkSystemUpdateState()
	if result.state != alertCheckUnknown || result.err == nil {
		t.Fatalf("result=%+v, want unknown when update command fails", result)
	}
}

func TestPanelUpdateFetchFailuresAreUnknown(t *testing.T) {
	previousFetch := fetchLatestPanelReleaseForAlert
	previousCurrent := panelCurrentVersion
	panelUpdateCache.mu.Lock()
	previousLastAt := panelUpdateCache.lastAt
	previousLatest := panelUpdateCache.latest
	previousMessage := panelUpdateCache.message
	panelUpdateCache.lastAt = time.Time{}
	panelUpdateCache.latest = ""
	panelUpdateCache.message = ""
	panelUpdateCache.mu.Unlock()
	panelCurrentVersion = "v2.0.1"
	t.Cleanup(func() {
		fetchLatestPanelReleaseForAlert = previousFetch
		panelCurrentVersion = previousCurrent
		panelUpdateCache.mu.Lock()
		panelUpdateCache.lastAt = previousLastAt
		panelUpdateCache.latest = previousLatest
		panelUpdateCache.message = previousMessage
		panelUpdateCache.mu.Unlock()
	})

	t.Run("request failure", func(t *testing.T) {
		fetchLatestPanelReleaseForAlert = func(string) (*GithubRelease, error) {
			return nil, errors.New("GitHub unavailable")
		}
		result := checkPanelUpdateState()
		if result.state != alertCheckUnknown || result.err == nil {
			t.Fatalf("result=%+v, want unknown", result)
		}
	})
	t.Run("empty release", func(t *testing.T) {
		fetchLatestPanelReleaseForAlert = func(string) (*GithubRelease, error) {
			return &GithubRelease{}, nil
		}
		result := checkPanelUpdateState()
		if result.state != alertCheckUnknown || result.err == nil {
			t.Fatalf("result=%+v, want unknown", result)
		}
	})
}

func TestWPFakeSearchBotFailuresAreUnknown(t *testing.T) {
	t.Run("configuration query", func(t *testing.T) {
		openAlertTestDB(t)
		result := checkWPFakeSearchBotThresholdState()
		if result.state != alertCheckUnknown || result.err == nil {
			t.Fatalf("result=%+v, want unknown", result)
		}
	})

	t.Run("event query", func(t *testing.T) {
		db := openAlertTestDB(t)
		seedWPSecurityAlertSettings(t, db, 1, 24)
		result := checkWPFakeSearchBotThresholdState()
		if result.state != alertCheckUnknown || result.err == nil {
			t.Fatalf("result=%+v, want unknown", result)
		}
	})

	t.Run("event row scan", func(t *testing.T) {
		db := openAlertTestDB(t)
		seedWPSecurityAlertSettings(t, db, 1, 24)
		mustExec(t, db, `CREATE TABLE wp_security_events (
			ip_address TEXT, event_type TEXT, occurred_at TEXT, path TEXT)`)
		mustExec(t, db, `INSERT INTO wp_security_events
			(ip_address, event_type, occurred_at, path)
			VALUES (NULL, 'fake_search_bot', datetime('now'), '/wp-login.php')`)
		result := checkWPFakeSearchBotThresholdState()
		if result.state != alertCheckUnknown || result.err == nil || !strings.Contains(result.err.Error(), "scan") {
			t.Fatalf("result=%+v, want unknown scan failure", result)
		}
	})
}

func TestWPSecurityAlertQueryBoundsOffendersBeforeLoadingPaths(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE wp_security_events (
		ip_address TEXT NOT NULL, event_type TEXT NOT NULL,
		occurred_at TEXT NOT NULL, path TEXT NOT NULL)`)
	recent := time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05")
	for index, count := range []int{2, 4, 3} {
		ip := fmt.Sprintf("192.0.2.%d", index+1)
		for hit := 0; hit < count; hit++ {
			if _, err := db.Exec(`INSERT INTO wp_security_events
				(ip_address, event_type, occurred_at, path) VALUES (?, ?, ?, ?)`,
				ip, SecurityEventFakeSearchBot, recent, fmt.Sprintf("/path-%d", hit)); err != nil {
				t.Fatal(err)
			}
		}
	}

	offenders, omitted, err := queryWPSecurityOffendersForAlert(
		SecurityEventFakeSearchBot, time.Now().UTC().Add(-24*time.Hour), 1, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 2 || omitted != 1 {
		t.Fatalf("offenders=%+v omitted=%d, want two retained and one omitted", offenders, omitted)
	}
	if offenders[0].IP != "192.0.2.2" || offenders[0].Count != 4 ||
		offenders[1].IP != "192.0.2.3" || offenders[1].Count != 3 {
		t.Fatalf("unexpected bounded offender order: %+v", offenders)
	}
	for _, offender := range offenders {
		if len(offender.Paths) == 0 || len(offender.Paths) > 3 {
			t.Fatalf("unexpected bounded paths for %+v", offender)
		}
	}
}

func TestCheckBackupReportsOnlyStaleEnabledSites(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (id INTEGER PRIMARY KEY, domain TEXT, status TEXT)`)
	mustExec(t, db, `CREATE TABLE backup_settings (site_id INTEGER, enabled INTEGER)`)
	mustExec(t, db, `CREATE TABLE db_backups (site_id INTEGER, auto INTEGER, created_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE site_migration_locks (site_id INTEGER, status TEXT)`)
	mustExec(t, db, `INSERT INTO websites (id, domain, status) VALUES
		(1, 'stale.example', 'active'),
		(2, 'recent.example', 'active'),
		(3, 'never.example', 'active'),
		(4, 'disabled.example', 'active'),
		(5, 'paused.example', 'paused'),
		(6, 'migrating.example', 'active')`)
	mustExec(t, db, `INSERT INTO backup_settings (site_id, enabled) VALUES
		(1, 1), (2, 1), (3, 1), (4, 0), (5, 1), (6, 1)`)
	mustExec(t, db, `INSERT INTO db_backups (site_id, auto, created_at) VALUES
		(1, 1, datetime('now', '-2 days')),
		(2, 1, datetime('now', '-1 hour')),
		(4, 1, datetime('now', '-2 days')),
		(5, 1, datetime('now', '-2 days')),
		(6, 1, datetime('now', '-2 days'))`)
	mustExec(t, db, `INSERT INTO site_migration_locks (site_id, status) VALUES (6, 'active')`)

	firing, msg := checkBackup()
	if !firing {
		t.Fatal("stale enabled site should alert")
	}
	if !strings.Contains(msg, "stale.example") {
		t.Fatalf("message should include stale site, got %q", msg)
	}
	for _, domain := range []string{"recent.example", "never.example", "disabled.example", "paused.example", "migrating.example"} {
		if strings.Contains(msg, domain) {
			t.Fatalf("message should not include %s, got %q", domain, msg)
		}
	}
}

func TestCheckSitesKeepsCachedFailureWhenCheckIsSkipped(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		id INTEGER PRIMARY KEY,
		domain TEXT,
		status TEXT,
		ssl_enabled INTEGER,
		monitoring_enabled INTEGER,
		monitoring_interval INTEGER
	)`)
	mustExec(t, db, `INSERT INTO websites
		(id, domain, status, ssl_enabled, monitoring_enabled, monitoring_interval)
		VALUES (1, 'down.example', 'active', 1, 1, 5)`)

	siteLastCheck["1"] = time.Now()
	siteFailureCounts["1"] = siteFailureAlertThreshold
	siteFailureMessages["1"] = "down.example 返回 500"

	firing, msg := checkSites()
	if !firing {
		t.Fatal("cached site failure should keep alert firing while interval skips the check")
	}
	if msg != "down.example 返回 500" {
		t.Fatalf("unexpected cached failure message: %q", msg)
	}
}

func TestCheckSitesReportsUnknownOnUnconfirmedCachedFailure(t *testing.T) {
	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		id INTEGER PRIMARY KEY,
		domain TEXT,
		status TEXT,
		ssl_enabled INTEGER,
		monitoring_enabled INTEGER,
		monitoring_interval INTEGER
	)`)
	mustExec(t, db, `INSERT INTO websites
		(id, domain, status, ssl_enabled, monitoring_enabled, monitoring_interval)
		VALUES (1, 'slow.example', 'active', 1, 1, 5)`)

	siteLastCheck["1"] = time.Now()
	siteFailureMessages["1"] = "slow.example timeout"
	siteFailureCounts["1"] = siteFailureAlertThreshold - 1

	result := checkSitesState()
	if result.state != alertCheckUnknown || result.err == nil {
		t.Fatalf("unconfirmed cached failure=%+v, want unknown", result)
	}
}

func TestCheckSitesReportsUnknownOnFirstFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	db := openAlertTestDB(t)
	mustExec(t, db, `CREATE TABLE websites (
		id INTEGER PRIMARY KEY,
		domain TEXT,
		status TEXT,
		ssl_enabled INTEGER,
		monitoring_enabled INTEGER,
		monitoring_interval INTEGER
	)`)
	domain := strings.TrimPrefix(server.URL, "http://")
	if _, err := db.Exec(`INSERT INTO websites
		(id, domain, status, ssl_enabled, monitoring_enabled, monitoring_interval)
		VALUES (1, ?, 'active', 0, 1, 5)`, domain); err != nil {
		t.Fatal(err)
	}

	result := checkSitesState()
	if result.state != alertCheckUnknown || result.err == nil {
		t.Fatalf("first failed check=%+v, want unknown until threshold is reached", result)
	}
	if siteFailureCounts["1"] != 1 {
		t.Fatalf("failure count=%d, want 1", siteFailureCounts["1"])
	}
}

func openAlertTestDB(t *testing.T) *sql.DB {
	t.Helper()

	prevDB := database.DB
	prevSiteLastCheck := siteLastCheck
	prevSiteFailureMessages := siteFailureMessages
	prevSiteFailureCounts := siteFailureCounts

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.DB = db
	siteLastCheck = make(map[string]time.Time)
	siteFailureMessages = make(map[string]string)
	siteFailureCounts = make(map[string]int)

	t.Cleanup(func() {
		db.Close()
		database.DB = prevDB
		siteLastCheck = prevSiteLastCheck
		siteFailureMessages = prevSiteFailureMessages
		siteFailureCounts = prevSiteFailureCounts
	})

	return db
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec failed: %v\nSQL: %s", err, query)
	}
}

func seedWPSecurityAlertSettings(t *testing.T, db *sql.DB, threshold, windowHours int) {
	t.Helper()
	mustExec(t, db, `CREATE TABLE security_settings (skey TEXT PRIMARY KEY, svalue TEXT)`)
	if _, err := db.Exec(`INSERT INTO security_settings (skey, svalue) VALUES
		('alert_wp_security_threshold', ?),
		('alert_wp_security_window_hours', ?)`, threshold, windowHours); err != nil {
		t.Fatalf("seed WordPress security alert settings: %v", err)
	}
}
