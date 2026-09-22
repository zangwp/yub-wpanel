package executor

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/database"
)

const (
	panelBinaryName             = "yub-wpanel"
	panelInstallPath            = "/usr/local/bin/yub-wpanel"
	releasePubKeyHex            = config.ReleasePublicKeyHex
	updateTerminalStatusTTL     = 5 * time.Minute
	autoUpdateCheckInterval     = 10 * time.Minute
	autoUpdateFetchInterval     = 24 * time.Hour
	autoUpdateFailureCooldown   = 24 * time.Hour
	panelBinaryBackupKeep       = 5
	panelDBBackupKeep           = 7
	panelBinaryPreflightTimeout = 20 * time.Second
	watchdogReadyTimeout        = 20 * time.Second
	panelRollbackStopTimeout    = 15 * time.Second
	panelBinaryMaxBytes         = int64(256 << 20)
	panelChecksumMaxBytes       = int64(4 << 10)
	panelSignatureMaxBytes      = int64(ed25519.SignatureSize)
)

type PanelUpdateStatus struct {
	Running         bool      `json:"running"`
	Completed       bool      `json:"completed"`
	Stage           string    `json:"stage"`
	Message         string    `json:"message"`
	Percent         int       `json:"percent"`
	DownloadPercent int       `json:"download_percent"`
	DownloadedBytes int64     `json:"downloaded_bytes"`
	TotalBytes      int64     `json:"total_bytes"`
	HasTotal        bool      `json:"has_total"`
	Error           string    `json:"error"`
	UpdatedAt       time.Time `json:"-"`
}

type PanelUpdateOptions struct {
	Trigger        string
	CurrentVersion string
	Proxy          string
	ConfigPath     string
	Config         *config.Config
	UseWatchdog    bool
}

type rollbackPlan struct {
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	BackupBinary   string `json:"backup_binary"`
	BackupSHA256   string `json:"backup_sha256"`
	BackupDB       string `json:"backup_db"`
	PlanPath       string `json:"plan_path"`
	ReadyPath      string `json:"ready_path"`
	ReadyNonce     string `json:"ready_nonce"`
	ConfigPath     string `json:"config_path"`
	HealthURL      string `json:"health_url"`
	CreatedAt      string `json:"created_at"`
	Trigger        string `json:"trigger"`
}

type autoUpdateSettings struct {
	Valid                    bool
	Enabled                  bool
	Mode                     string
	Window                   string
	ReleaseDelay             time.Duration
	SignatureTimeout         time.Duration
	LastTargetVersion        string
	LastAttemptAt            time.Time
	LastCheckAt              time.Time
	LastStatus               string
	LastStage                string
	LastSignatureWaitVersion string
	LastSignatureWaitAt      time.Time
}

var (
	panelUpdateMu            sync.Mutex
	panelUpdateStatusMu      sync.Mutex
	currentPanelUpdateStatus = PanelUpdateStatus{
		Stage:     "idle",
		Message:   "等待更新",
		UpdatedAt: time.Now(),
	}
	panelUpdateAutoStarted sync.Once
)

func ExecutePanelUpdate(opts PanelUpdateOptions) (*GithubRelease, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("仅支持 Linux 服务器更新")
	}
	if !panelUpdateMu.TryLock() {
		return nil, fmt.Errorf("已有更新任务正在执行，请稍后再试")
	}
	defer panelUpdateMu.Unlock()
	if !panelLifecycleMu.TryLock() {
		return nil, fmt.Errorf("已有面板维护操作正在执行，请稍后再试")
	}
	defer panelLifecycleMu.Unlock()
	watchdogPlanPath := ""
	watchdogPlanTransferred := false
	watchdogPlanPreserve := false
	defer func() {
		cleanupUntransferredRollbackPlan(watchdogPlanPath, watchdogPlanTransferred, watchdogPlanPreserve)
	}()
	if opts.Config != nil && ReconcilePanelDBRestoreStatus(opts.Config).Status == "running" {
		return nil, fmt.Errorf("面板数据库正在恢复，请稍后再更新")
	}

	resetPanelUpdateStatus()
	trigger := opts.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	recordPanelUpdateStage(trigger, "", "fetch_release", "running", "正在获取版本信息")
	setPanelUpdateStep("fetch_release", "正在获取版本信息...", 5)

	latest, err := FetchLatestPanelRelease(opts.Proxy)
	if err != nil {
		return nil, panelUpdateFail(trigger, "", "fetch_release", "获取版本信息失败: "+err.Error())
	}
	if latest == nil || latest.TagName == "" {
		return nil, panelUpdateFail(trigger, "", "fetch_release", "获取版本信息失败: release 为空")
	}
	if !isCanonicalStableTag(latest.TagName) {
		return nil, panelUpdateFail(trigger, latest.TagName, "compare_version", "目标 Release 标签不是规范稳定版本，拒绝更新")
	}
	if !isCanonicalStableTag(opts.CurrentVersion) {
		return nil, panelUpdateFail(trigger, latest.TagName, "compare_version", "当前版本无法验证为规范稳定版本，拒绝在线更新")
	}
	if CompareVersions(latest.TagName, opts.CurrentVersion) <= 0 {
		return nil, panelUpdateFail(trigger, latest.TagName, "compare_version", "已经是最新版本")
	}

	setPanelUpdateStep("resolve_assets", "正在准备更新文件...", 10)
	downloadURL, sha256URL, sigURL := resolvePanelAssets(latest)
	if downloadURL == "" {
		return nil, panelUpdateFail(trigger, latest.TagName, "resolve_assets", "未找到适用于当前系统的二进制文件")
	}
	if sha256URL == "" {
		return nil, panelUpdateFail(trigger, latest.TagName, "resolve_assets", "未找到 SHA256 校验文件，无法验证更新完整性")
	}
	if sigURL == "" {
		setPanelUpdateFailed("未找到 Ed25519 签名文件，等待签名发布")
		recordPanelUpdateStage(trigger, latest.TagName, "waiting_signature", "waiting", "未找到 yub-wpanel.sha256.sig，等待签名发布")
		return latest, errWaitingSignature
	}

	setPanelUpdateStep("prepare_download", "正在创建临时目录...", 12)
	tmpDir, err := os.MkdirTemp("", "yub-wpanel-update-*")
	if err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "prepare_download", "创建临时目录失败: "+err.Error())
	}
	defer os.RemoveAll(tmpDir)

	newBinary := filepath.Join(tmpDir, panelBinaryName)
	setPanelUpdateStep("download_binary", "正在下载更新包...", 15)
	if err := downloadFileWithProgress(proxyURL(opts.Proxy, downloadURL), newBinary, 10*time.Minute, panelBinaryMaxBytes, setPanelBinaryDownloadProgress); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "download_binary", "更新包下载失败: "+err.Error())
	}
	if err := os.Chmod(newBinary, 0755); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "download_binary", "设置新版本权限失败: "+err.Error())
	}

	setPanelUpdateStep("download_sha256", "正在下载校验文件...", 62)
	shaFile := filepath.Join(tmpDir, panelBinaryName+".sha256")
	if err := downloadFile(proxyURL(opts.Proxy, sha256URL), shaFile, panelChecksumMaxBytes); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "download_sha256", "SHA256 校验文件下载失败: "+err.Error())
	}
	setPanelUpdateStep("download_signature", "正在下载签名文件...", 66)
	sigFile := filepath.Join(tmpDir, panelBinaryName+".sha256.sig")
	if err := downloadFile(proxyURL(opts.Proxy, sigURL), sigFile, panelSignatureMaxBytes); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "download_signature", "签名文件下载失败: "+err.Error())
	}
	setPanelUpdateStep("verify_signature", "正在校验更新来源...", 72)
	if err := verifyEd25519(shaFile, sigFile); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "verify_signature", "签名校验失败: "+err.Error())
	}
	setPanelUpdateStep("verify_sha256", "正在校验更新包完整性...", 78)
	if err := verifySHA256(newBinary, shaFile); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "verify_sha256", "SHA256 校验失败: "+err.Error())
	}

	setPanelUpdateStep("preflight", "正在预检新版本...", 82)
	if err := preflightBinary(newBinary, opts.ConfigPath, latest.TagName); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "preflight", "新版本预检失败: "+err.Error())
	}
	if opts.UseWatchdog {
		if opts.Config == nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "preflight", "启用更新守护时配置不能为空")
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "preflight", "systemd-run 不可用，无法启动独立更新守护进程: "+err.Error())
		}
	}
	setPanelUpdateStep("disk_check", "正在检查磁盘空间...", 84)
	if err := checkUpdateDiskSpace(newBinary, opts.Config); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "disk_check", err.Error())
	}

	setPanelUpdateStep("backup", "正在备份当前版本...", 88)
	backupPath := versionedBackupPath(opts.CurrentVersion)
	if err := copyPanelFile(panelInstallPath, backupPath, 0755); err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "backup_binary", "备份旧版本失败: "+err.Error())
	}
	backupSHA256, err := fileSHA256Hex(backupPath)
	if err != nil {
		return nil, panelUpdateFail(trigger, latest.TagName, "backup_binary", "计算旧版本备份摘要失败: "+err.Error())
	}

	backupDB := ""
	if opts.Config != nil {
		backupDir := filepath.Join(opts.Config.Panel.BackupDir, "panel-db")
		path, err := database.BackupDatabase(backupDir)
		if err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "backup_database", "备份面板数据库失败: "+err.Error())
		}
		if err := database.VerifyDBBackup(path); err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "backup_database", "数据库备份校验失败: "+err.Error())
		}
		backupDB = path
	}

	plan := rollbackPlan{
		CurrentVersion: opts.CurrentVersion,
		TargetVersion:  latest.TagName,
		BackupBinary:   backupPath,
		BackupSHA256:   backupSHA256,
		BackupDB:       backupDB,
		ConfigPath:     opts.ConfigPath,
		HealthURL:      healthURL(opts.Config),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Trigger:        trigger,
	}
	if opts.UseWatchdog && opts.Config != nil {
		plan.PlanPath = rollbackPlanPath(opts.Config)
		plan.ReadyPath = plan.PlanPath + ".ready"
		plan.ReadyNonce, err = newWatchdogReadyNonce()
		if err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "write_rollback_plan", "生成更新守护握手随机数失败: "+err.Error())
		}
		watchdogPlanPath = plan.PlanPath
		if err := writeRollbackPlanFile(plan.PlanPath, plan); err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "write_rollback_plan", "写入回滚计划失败: "+err.Error())
		}
		validatedPlan, err := loadTrustedPendingRollbackPlan(opts.Config, opts.ConfigPath)
		if err != nil {
			return nil, panelUpdateFail(trigger, latest.TagName, "write_rollback_plan", "回滚计划身份复核失败: "+err.Error())
		}
		plan = validatedPlan
	}

	setPanelUpdateStep("replace_binary", "正在替换面板文件...", 92)
	if err := replaceInstalledPanelBinary(newBinary); err != nil {
		if panelFileReplacementCommitted(err) {
			preservePlan, recoveryErr := recoverBinaryAfterCommittedReplaceFailure(backupPath, err, replaceInstalledPanelBinary)
			watchdogPlanPreserve = preservePlan
			return nil, panelUpdateFail(trigger, latest.TagName, "replace_binary", recoveryErr.Error())
		}
		return nil, panelUpdateFail(trigger, latest.TagName, "replace_binary", "原子替换失败，旧版本仍保留: "+err.Error())
	}

	if opts.UseWatchdog && plan.PlanPath != "" {
		if err := startUpdateWatchdog(plan); err != nil {
			preservePlan, recoveryErr := recoverBinaryAfterWatchdogStartFailure(backupPath, err, replaceInstalledPanelBinary)
			watchdogPlanPreserve = preservePlan
			return nil, panelUpdateFail(trigger, latest.TagName, "start_watchdog", recoveryErr.Error())
		}
		watchdogPlanTransferred = true
	}

	setPanelUpdateStep("restart", "正在重启面板...", 98)
	recordPanelUpdateStage(trigger, latest.TagName, "restart", "running", "二进制替换完成，正在重启 yub-wpanel")
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("systemctl", "restart", "yub-wpanel").Run()
	}()

	setPanelUpdateCompleted("更新文件已替换，面板正在重启并等待健康检查...")
	return latest, nil
}

var errWaitingSignature = fmt.Errorf("waiting_signature")

func SnapshotPanelUpdateStatus() PanelUpdateStatus {
	panelUpdateStatusMu.Lock()
	defer panelUpdateStatusMu.Unlock()
	if panelUpdateStatusExpiredLocked(time.Now()) {
		resetPanelUpdateStatusLocked()
	}
	return currentPanelUpdateStatus
}

func StartPanelAutoUpdateScheduler(currentVersion, configPath string, cfg *config.Config) {
	panelUpdateAutoStarted.Do(func() {
		go func() {
			time.Sleep(2 * time.Minute)
			runPanelAutoUpdateCheck(currentVersion, configPath, cfg)
			ticker := time.NewTicker(autoUpdateCheckInterval)
			defer ticker.Stop()
			for range ticker.C {
				runPanelAutoUpdateCheck(currentVersion, configPath, cfg)
			}
		}()
	})
}

func FinalizePendingPanelUpdate(cfg *config.Config, configPath, currentVersion string) {
	if cfg == nil {
		return
	}
	plan, err := loadTrustedPendingRollbackPlan(cfg, configPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			recordPanelUpdateStage("unknown", "", "finalize", "failed", "读取回滚计划失败: "+err.Error())
		}
		return
	}
	if currentVersion == plan.TargetVersion {
		recordPanelUpdateStage(plan.Trigger, plan.TargetVersion, "new_process_started", "running", "新版本进程已启动，等待 watchdog 健康检查")
	}
}

func RunUpdateWatchdog(cfg *config.Config, configPath, planPath string) {
	plan, err := loadValidatedUpdateWatchdogPlan(cfg, configPath, planPath)
	if err != nil {
		log.Printf("[更新守护] 身份或回滚计划校验失败: %v", err)
		return
	}
	deadline := time.Now().Add(90 * time.Second)
	var healthErr error
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		healthErr = healthCheckManagedPanelVersion(plan.HealthURL, plan.TargetVersion, verifyPanelServiceMainProcess)
		if healthErr == nil {
			recordPanelUpdateStage(plan.Trigger, plan.TargetVersion, "health_check", "success", "更新后健康检查通过")
			if plan.Trigger == "auto" {
				sendPanelUpdateMail(true, plan.TargetVersion, "health_check", "自动更新成功，健康检查通过")
			}
			setSecuritySetting("panel_auto_update_last_status", "success")
			setSecuritySetting("panel_auto_update_last_success_at", time.Now().Format(time.RFC3339))
			setSecuritySetting("panel_auto_update_last_success_version", plan.TargetVersion)
			clearPanelUpdateCache()
			cleanupPanelUpdateBackups(plan)
			_ = removeWatchdogReadyFile(plan)
			_ = os.Remove(planPath)
			return
		}
	}
	msg := "健康检查超时"
	if healthErr != nil {
		msg += ": " + healthErr.Error()
	}
	ops := productionPanelRollbackOps()
	ops.prepare = func() (rollbackPlan, bool, error) {
		// The watchdog can run for 90 seconds. Revalidate the root-owned trust
		// chain only after the unhealthy service is confirmed inactive, then
		// persist diagnostics before a database restore closes this connection.
		validatedPlan, err := loadValidatedUpdateWatchdogPlan(cfg, configPath, planPath)
		if err != nil {
			return rollbackPlan{}, false, fmt.Errorf("revalidate rollback identity: %w", err)
		}
		plan = validatedPlan
		restoreDatabase := shouldRestoreDBAfterHealthFailure(plan.BackupDB)
		recordPanelUpdateStage(plan.Trigger, plan.TargetVersion, "rollback_stop", "success", "新版本服务已停止并确认完全退出")
		recordPanelUpdateStage(plan.Trigger, plan.TargetVersion, "health_check", "failed", msg)
		setSecuritySetting("panel_auto_update_last_status", "failed")
		setSecuritySetting("panel_auto_update_last_stage", "health_check")
		setSecuritySetting("panel_auto_update_last_error", msg)
		if plan.Trigger == "auto" {
			sendPanelUpdateMail(false, plan.TargetVersion, "health_check", msg+"；准备安全回滚旧版本")
		}
		return plan, restoreDatabase, nil
	}
	stage, err := executePanelRollback(cfg, ops)
	if err != nil {
		failMsg := "安全回滚失败（服务保持停止，回滚计划与备份已保留）: " + err.Error()
		if stage == "rollback_stop" {
			failMsg = "无法确认新版本服务已停止；未触碰数据库或二进制，回滚计划与备份已保留: " + err.Error()
		}
		log.Printf("[更新守护] %s", failMsg)
		if stage != "rollback_stop" {
			recordPanelUpdateStage(plan.Trigger, plan.TargetVersion, stage, "failed", failMsg)
			sendPanelUpdateMail(false, plan.TargetVersion, stage, failMsg)
		}
		return
	}
	log.Printf("[更新守护] 旧版本 %s 已恢复并通过健康检查", plan.CurrentVersion)
	recordPanelUpdateStage(plan.Trigger, plan.CurrentVersion, "rollback_health", "success", "旧版本进程已恢复并通过健康检查")
	_ = removeWatchdogReadyFile(plan)
	_ = os.Remove(planPath)
}

type panelRollbackOps struct {
	stopAndWait   func(time.Duration) error
	prepare       func() (rollbackPlan, bool, error)
	restoreDB     func(*config.Config, string) error
	restoreBinary func(string) error
	start         func() error
	waitVersion   func(string, string, time.Duration) error
}

func productionPanelRollbackOps() panelRollbackOps {
	return panelRollbackOps{
		stopAndWait:   stopPanelServiceAndWaitInactive,
		restoreDB:     restoreDBFile,
		restoreBinary: replaceInstalledPanelBinary,
		start: func() error {
			out, err := exec.Command("systemctl", "start", "yub-wpanel").CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl start yub-wpanel: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		waitVersion: waitForPanelVersion,
	}
}

func executePanelRollback(cfg *config.Config, ops panelRollbackOps) (string, error) {
	if ops.stopAndWait == nil || ops.prepare == nil || ops.restoreDB == nil || ops.restoreBinary == nil || ops.start == nil || ops.waitVersion == nil {
		return "rollback_stop", fmt.Errorf("rollback operations are incomplete")
	}
	if err := ops.stopAndWait(panelRollbackStopTimeout); err != nil {
		return "rollback_stop", fmt.Errorf("stop new panel service before rollback: %w", err)
	}
	failStopped := func(stage string, cause error) (string, error) {
		if stopErr := ops.stopAndWait(panelRollbackStopTimeout); stopErr != nil {
			return stage, fmt.Errorf("%w; additionally failed to confirm service remains stopped: %v", cause, stopErr)
		}
		return stage, cause
	}
	plan, restoreDatabase, err := ops.prepare()
	if err != nil {
		return failStopped("rollback_validate", err)
	}
	if restoreDatabase {
		if err := ops.restoreDB(cfg, plan.BackupDB); err != nil {
			return failStopped("rollback_database", fmt.Errorf("restore database backup: %w", err))
		}
	}
	if err := ops.restoreBinary(plan.BackupBinary); err != nil {
		return failStopped("rollback_binary", fmt.Errorf("restore old panel binary: %w", err))
	}
	if err := ops.start(); err != nil {
		return failStopped("rollback_start", fmt.Errorf("start restored panel service: %w", err))
	}
	if err := ops.waitVersion(plan.HealthURL, plan.CurrentVersion, 30*time.Second); err != nil {
		return failStopped("rollback_health", fmt.Errorf("verify restored panel version: %w", err))
	}
	return "rollback_health", nil
}

func stopPanelServiceAndWaitInactive(timeout time.Duration) error {
	out, err := exec.Command("systemctl", "stop", "yub-wpanel").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl stop yub-wpanel: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(timeout)
	var lastState, lastPID string
	for {
		stateOut, stateErr := exec.Command("systemctl", "show", "yub-wpanel.service", "--property=ActiveState", "--value").Output()
		pidOut, pidErr := exec.Command("systemctl", "show", "yub-wpanel.service", "--property=MainPID", "--value").Output()
		if stateErr == nil && pidErr == nil {
			lastState = strings.TrimSpace(string(stateOut))
			lastPID = strings.TrimSpace(string(pidOut))
			if lastState == "inactive" && lastPID == "0" {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			if stateErr != nil {
				return fmt.Errorf("query panel service state: %w", stateErr)
			}
			if pidErr != nil {
				return fmt.Errorf("query panel service main PID: %w", pidErr)
			}
			return fmt.Errorf("panel service did not become inactive (state=%s main_pid=%s)", lastState, lastPID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func recoverBinaryAfterWatchdogStartFailure(backupPath string, startErr error, restore func(string) error) (bool, error) {
	if restore == nil {
		return true, fmt.Errorf("启动更新健康检查进程失败，且旧二进制恢复函数不可用: %w", startErr)
	}
	if err := restore(backupPath); err != nil {
		return true, fmt.Errorf("启动更新健康检查进程失败 (%v)，且恢复旧二进制失败: %w", startErr, err)
	}
	return false, fmt.Errorf("启动更新健康检查进程失败，已恢复旧二进制: %w", startErr)
}

func recoverBinaryAfterCommittedReplaceFailure(backupPath string, replaceErr error, restore func(string) error) (bool, error) {
	if restore == nil {
		return true, fmt.Errorf("新二进制已替换，但目录持久化失败，且旧二进制恢复函数不可用: %w", replaceErr)
	}
	if err := restore(backupPath); err != nil {
		return true, fmt.Errorf("新二进制已替换但目录持久化失败 (%v)，且恢复旧二进制失败；回滚计划与备份已保留: %w", replaceErr, err)
	}
	return false, fmt.Errorf("新二进制已替换但目录持久化失败，已恢复旧二进制: %w", replaceErr)
}

func cleanupUntransferredRollbackPlan(planPath string, transferred, preserve bool) {
	if planPath == "" || transferred || preserve {
		return
	}
	_ = os.Remove(planPath)
}

func IsPatchBump(current, target string) bool {
	c, okC := parseStableSemver(current)
	t, okT := parseStableSemver(target)
	if !okC || !okT {
		return false
	}
	return c[0] == t[0] && c[1] == t[1] && t[2] > c[2]
}

func IsStableVersion(version string) bool {
	_, ok := parseStableSemver(version)
	return ok
}

func isCanonicalStableTag(version string) bool {
	parsed, ok := parseStableSemver(version)
	if !ok {
		return false
	}
	return version == fmt.Sprintf("v%d.%d.%d", parsed[0], parsed[1], parsed[2])
}

func proxyURL(proxy, original string) string {
	if proxy != "" {
		return strings.TrimRight(proxy, "/") + "/" + original
	}
	return original
}

func resolvePanelAssets(latest *GithubRelease) (binaryURL, sha256URL, sigURL string) {
	for _, a := range latest.Assets {
		switch a.Name {
		case panelBinaryName:
			binaryURL = a.BrowserDownloadURL
		case panelBinaryName + ".sha256":
			sha256URL = a.BrowserDownloadURL
		case panelBinaryName + ".sha256.sig":
			sigURL = a.BrowserDownloadURL
		}
	}
	return
}

func downloadFile(url, dest string, maxBytes int64) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * 2 * time.Second)
		}
		lastErr = downloadFileWithProgress(url, dest, 60*time.Second, maxBytes, nil)
		if lastErr == nil {
			return nil
		}
		_ = os.Remove(dest)
	}
	return lastErr
}

func downloadFileWithProgress(url, dest string, timeout time.Duration, maxBytes int64, progress func(downloaded, total int64)) error {
	if maxBytes <= 0 {
		return fmt.Errorf("下载大小上限无效")
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return fmt.Errorf("下载内容超过 %d 字节上限", maxBytes)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	copied := false
	defer func() {
		out.Close()
		if !copied {
			_ = os.Remove(dest)
		}
	}()
	if progress != nil {
		progress(0, resp.ContentLength)
	}
	var downloaded int64
	buf := make([]byte, 32*1024)
	limitedBody := io.LimitReader(resp.Body, maxBytes+1)
	for {
		n, readErr := limitedBody.Read(buf)
		if n > 0 {
			downloaded += int64(n)
			if downloaded > maxBytes {
				return fmt.Errorf("下载内容超过 %d 字节上限", maxBytes)
			}
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
			if progress != nil {
				progress(downloaded, resp.ContentLength)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	copied = true
	return nil
}

func verifySHA256(filePath, shaFile string) error {
	data, err := os.ReadFile(shaFile)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return fmt.Errorf("SHA256 文件为空")
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("SHA256 文件必须只有一条记录")
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 2 {
		return fmt.Errorf("SHA256 文件格式异常")
	}
	expected := fields[0]
	expectedName := filepath.Base(filePath)
	if fields[1] != expectedName && fields[1] != "*"+expectedName {
		return fmt.Errorf("SHA256 文件名不匹配")
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("SHA256 长度异常")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(expected, fmt.Sprintf("%x", h.Sum(nil))) {
		return fmt.Errorf("SHA256 不匹配")
	}
	return nil
}

func verifyEd25519(shaFile, sigFile string) error {
	pubKey, err := hex.DecodeString(releasePubKeyHex)
	if err != nil {
		return fmt.Errorf("解析内置公钥失败")
	}
	sig, err := os.ReadFile(sigFile)
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("签名长度异常: %d", len(sig))
	}
	message, err := os.ReadFile(shaFile)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pubKey, message, sig) {
		return fmt.Errorf("Ed25519 签名不匹配")
	}
	return nil
}

func preflightBinary(path, configPath, expectedVersion string) error {
	return preflightBinaryWithTimeout(path, configPath, expectedVersion, panelBinaryPreflightTimeout)
}

func preflightBinaryWithTimeout(path, configPath, expectedVersion string, timeout time.Duration) error {
	if strings.TrimSpace(configPath) == "" {
		return fmt.Errorf("配置文件路径为空")
	}
	if !isCanonicalStableTag(expectedVersion) {
		return fmt.Errorf("目标版本格式异常: %q", expectedVersion)
	}
	if timeout <= 0 {
		return fmt.Errorf("预检超时时间必须大于零")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--info", "--config", configPath)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("候选二进制预检超时")
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	actualVersion, err := parsePanelInfoVersion(out)
	if err != nil {
		return err
	}
	if actualVersion != expectedVersion {
		return fmt.Errorf("候选二进制版本 %q 与目标 Release %q 不一致", actualVersion, expectedVersion)
	}
	return nil
}

func parsePanelInfoVersion(out []byte) (string, error) {
	const prefix = "版本:"
	version := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if len(fields) == 0 || version != "" {
			return "", fmt.Errorf("候选二进制版本输出格式异常")
		}
		version = fields[0]
	}
	if !isCanonicalStableTag(version) {
		return "", fmt.Errorf("候选二进制未报告规范稳定版本")
	}
	return version, nil
}

func versionedBackupPath(currentVersion string) string {
	version := sanitizeBackupPart(currentVersion)
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("%s.bak.%s.%s", panelInstallPath, version, time.Now().UTC().Format("20060102-150405.000000000"))
}

func sanitizeBackupPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func copyPanelFile(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	copied := false
	dstClosed := false
	defer func() {
		if !dstClosed {
			_ = dst.Close()
		}
		if !copied {
			_ = os.Remove(dstPath)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Chmod(mode); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	dstClosed = true
	copied = true
	return nil
}

func replaceInstalledPanelBinary(srcPath string) error {
	return replacePanelFileAtomically(srcPath, panelInstallPath, 0o755)
}

type panelFileReplaceError struct {
	err       error
	committed bool
}

func (e *panelFileReplaceError) Error() string {
	return e.err.Error()
}

func (e *panelFileReplaceError) Unwrap() error {
	return e.err
}

func panelFileReplacementCommitted(err error) bool {
	var replaceErr *panelFileReplaceError
	return errors.As(err, &replaceErr) && replaceErr.committed
}

func replacePanelFileAtomically(srcPath, dstPath string, mode os.FileMode) error {
	return replacePanelFileAtomicallyWithSync(srcPath, dstPath, mode, syncDirectory)
}

func replacePanelFileAtomicallyWithSync(srcPath, dstPath string, mode os.FileMode, syncParent func(string) error) error {
	if syncParent == nil {
		return fmt.Errorf("panel replacement directory sync is unavailable")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), "."+filepath.Base(dstPath)+".replace-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return err
	}
	if err := syncParent(filepath.Dir(dstPath)); err != nil {
		return &panelFileReplaceError{
			err:       fmt.Errorf("sync panel binary directory after committed rename: %w", err),
			committed: true,
		}
	}
	return nil
}

func resetPanelUpdateStatus() {
	panelUpdateStatusMu.Lock()
	resetPanelUpdateStatusLocked()
	panelUpdateStatusMu.Unlock()
}

func resetPanelUpdateStatusLocked() {
	currentPanelUpdateStatus = PanelUpdateStatus{Stage: "idle", Message: "等待更新", UpdatedAt: time.Now()}
}

func panelUpdateStatusExpiredLocked(now time.Time) bool {
	if currentPanelUpdateStatus.Running {
		return false
	}
	if !currentPanelUpdateStatus.Completed && currentPanelUpdateStatus.Error == "" {
		return false
	}
	return now.Sub(currentPanelUpdateStatus.UpdatedAt) > updateTerminalStatusTTL
}

func setPanelUpdateStep(stage, message string, percent int) {
	panelUpdateStatusMu.Lock()
	currentPanelUpdateStatus.Running = true
	currentPanelUpdateStatus.Completed = false
	currentPanelUpdateStatus.Stage = stage
	currentPanelUpdateStatus.Message = message
	currentPanelUpdateStatus.Percent = clampPercent(percent)
	currentPanelUpdateStatus.DownloadPercent = 0
	currentPanelUpdateStatus.DownloadedBytes = 0
	currentPanelUpdateStatus.TotalBytes = 0
	currentPanelUpdateStatus.HasTotal = false
	currentPanelUpdateStatus.Error = ""
	currentPanelUpdateStatus.UpdatedAt = time.Now()
	panelUpdateStatusMu.Unlock()
}

func setPanelBinaryDownloadProgress(downloaded, total int64) {
	hasTotal := total > 0
	downloadPercent := 0
	overallPercent := 15
	if hasTotal {
		downloadPercent = clampPercent(int(downloaded * 100 / total))
		overallPercent = 15 + downloadPercent*45/100
	}
	panelUpdateStatusMu.Lock()
	currentPanelUpdateStatus.Running = true
	currentPanelUpdateStatus.Completed = false
	currentPanelUpdateStatus.Stage = "download_binary"
	currentPanelUpdateStatus.Message = "正在下载更新包..."
	currentPanelUpdateStatus.Percent = clampPercent(overallPercent)
	currentPanelUpdateStatus.DownloadPercent = downloadPercent
	currentPanelUpdateStatus.DownloadedBytes = downloaded
	currentPanelUpdateStatus.TotalBytes = total
	currentPanelUpdateStatus.HasTotal = hasTotal
	currentPanelUpdateStatus.Error = ""
	currentPanelUpdateStatus.UpdatedAt = time.Now()
	panelUpdateStatusMu.Unlock()
}

func setPanelUpdateFailed(message string) {
	panelUpdateStatusMu.Lock()
	currentPanelUpdateStatus.Running = false
	currentPanelUpdateStatus.Completed = false
	currentPanelUpdateStatus.Message = message
	currentPanelUpdateStatus.Error = message
	currentPanelUpdateStatus.UpdatedAt = time.Now()
	panelUpdateStatusMu.Unlock()
}

func setPanelUpdateCompleted(message string) {
	panelUpdateStatusMu.Lock()
	currentPanelUpdateStatus.Running = false
	currentPanelUpdateStatus.Completed = true
	currentPanelUpdateStatus.Stage = "completed"
	currentPanelUpdateStatus.Message = message
	currentPanelUpdateStatus.Percent = 100
	currentPanelUpdateStatus.DownloadPercent = 100
	currentPanelUpdateStatus.Error = ""
	currentPanelUpdateStatus.UpdatedAt = time.Now()
	panelUpdateStatusMu.Unlock()
}

func clampPercent(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func panelUpdateFail(trigger, version, stage, message string) error {
	setPanelUpdateFailed(message)
	recordPanelUpdateStage(trigger, version, stage, "failed", message)
	return fmt.Errorf("%s", message)
}

func recordPanelUpdateStage(trigger, targetVersion, stage, status, message string) {
	if trigger == "" {
		trigger = "manual"
	}
	target := targetVersion
	if target == "" {
		target = "unknown"
	}
	recordOperationLog("panel_"+trigger+"_update", target, status, stage+": "+message)
	if trigger == "auto" {
		setSecuritySetting("panel_auto_update_last_target_version", targetVersion)
		setSecuritySetting("panel_auto_update_last_status", status)
		setSecuritySetting("panel_auto_update_last_stage", stage)
		setSecuritySetting("panel_auto_update_last_error", message)
		setSecuritySetting("panel_auto_update_last_attempt_at", time.Now().Format(time.RFC3339))
	}
}

func runPanelAutoUpdateCheck(currentVersion, configPath string, cfg *config.Config) {
	if currentVersion == "" || currentVersion == "dev" {
		return
	}
	settings := readAutoUpdateSettings()
	if !settings.Valid || !settings.Enabled || !withinAutoUpdateWindow(settings.Window, time.Now()) {
		return
	}
	if settings.LastStatus == "failed" && settings.LastAttemptAt.After(time.Now().Add(-autoUpdateFailureCooldown)) {
		return
	}
	if !shouldFetchForAutoUpdate(settings, time.Now()) {
		return
	}
	setSecuritySetting("panel_auto_update_last_check_at", time.Now().Format(time.RFC3339))
	latest, err := FetchLatestPanelRelease(readSecuritySetting("github_proxy"))
	if err != nil || latest == nil || latest.TagName == "" || CompareVersions(latest.TagName, currentVersion) <= 0 {
		return
	}
	if !IsStableVersion(latest.TagName) {
		return
	}
	if settings.Mode != "all_stable" && !IsPatchBump(currentVersion, latest.TagName) {
		recordPanelUpdateStage("auto", latest.TagName, "version_policy", "skipped", "当前策略仅允许 patch 自动更新")
		return
	}
	if wait, ok := shouldWaitForReleaseDelay(settings, latest.TagName); ok {
		recordPanelUpdateStage("auto", latest.TagName, "waiting_release_delay", "waiting", "等待发布成熟期: "+wait.String())
		return
	}
	_, _, sigURL := resolvePanelAssets(latest)
	if sigURL == "" {
		handleWaitingSignature(settings, latest.TagName)
		return
	}
	_, err = ExecutePanelUpdate(PanelUpdateOptions{
		Trigger:        "auto",
		CurrentVersion: currentVersion,
		Proxy:          readSecuritySetting("github_proxy"),
		ConfigPath:     configPath,
		Config:         cfg,
		UseWatchdog:    true,
	})
	if err != nil && err != errWaitingSignature {
		sendPanelUpdateMail(false, latest.TagName, readSecuritySetting("panel_auto_update_last_stage"), err.Error())
	}
}

func readAutoUpdateSettings() autoUpdateSettings {
	enabledValue, err := readRequiredSecuritySetting("panel_auto_update_enabled")
	valid := err == nil && (enabledValue == "true" || enabledValue == "false")
	mode, err := readRequiredSecuritySetting("panel_auto_update_mode")
	valid = valid && err == nil && (mode == "patch_only" || mode == "all_stable")
	window, err := readRequiredSecuritySetting("panel_auto_update_window")
	valid = valid && err == nil
	delay, err := readRequiredSettingMinutes("panel_auto_update_release_delay_minutes", 1, 1440)
	valid = valid && err == nil
	signatureTimeout, err := readRequiredSettingMinutes("panel_auto_update_signature_timeout_minutes", 5, 1440)
	valid = valid && err == nil
	return autoUpdateSettings{
		Valid:                    valid && validAutoUpdateWindow(window),
		Enabled:                  enabledValue == "true",
		Mode:                     mode,
		Window:                   window,
		ReleaseDelay:             delay,
		SignatureTimeout:         signatureTimeout,
		LastTargetVersion:        readSecuritySetting("panel_auto_update_last_target_version"),
		LastCheckAt:              parseSettingTime("panel_auto_update_last_check_at"),
		LastAttemptAt:            parseSettingTime("panel_auto_update_last_attempt_at"),
		LastStatus:               readSecuritySetting("panel_auto_update_last_status"),
		LastStage:                readSecuritySetting("panel_auto_update_last_stage"),
		LastSignatureWaitVersion: readSecuritySetting("panel_auto_update_signature_wait_version"),
		LastSignatureWaitAt:      parseSettingTime("panel_auto_update_signature_wait_at"),
	}
}

func readRequiredSettingMinutes(key string, min, max int) (time.Duration, error) {
	raw, err := readRequiredSecuritySetting(key)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("invalid minute setting %s", key)
	}
	return time.Duration(value) * time.Minute, nil
}

func readRequiredSecuritySetting(key string) (string, error) {
	db := database.GetDB()
	if db == nil {
		return "", errors.New("database unavailable")
	}
	var value string
	if err := db.QueryRow("SELECT svalue FROM security_settings WHERE skey = ?", key).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

func validAutoUpdateWindow(window string) bool {
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return false
	}
	_, err1 := parseClock(parts[0])
	_, err2 := parseClock(parts[1])
	return err1 == nil && err2 == nil
}

func shouldFetchForAutoUpdate(settings autoUpdateSettings, now time.Time) bool {
	if isWaitingForSignature(settings, now) {
		return true
	}
	if isReleaseDelayReady(settings, now) {
		return true
	}
	return settings.LastCheckAt.IsZero() || now.Sub(settings.LastCheckAt) >= autoUpdateFetchInterval
}

func isWaitingForSignature(settings autoUpdateSettings, now time.Time) bool {
	return settings.LastStatus == "waiting" &&
		settings.LastSignatureWaitVersion != "" &&
		!settings.LastSignatureWaitAt.IsZero() &&
		now.Sub(settings.LastSignatureWaitAt) <= settings.SignatureTimeout
}

func isReleaseDelayReady(settings autoUpdateSettings, now time.Time) bool {
	return settings.LastStatus == "waiting" &&
		settings.LastStage == "waiting_release_delay" &&
		settings.LastTargetVersion != "" &&
		!settings.LastAttemptAt.IsZero() &&
		now.Sub(settings.LastAttemptAt) >= settings.ReleaseDelay
}

func parseSettingMinutes(key string, fallback int) time.Duration {
	v, err := strconv.Atoi(readSecuritySetting(key))
	if err != nil || v <= 0 {
		v = fallback
	}
	return time.Duration(v) * time.Minute
}

func parseSettingTime(key string) time.Time {
	v := readSecuritySetting(key)
	if v == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

func shouldWaitForReleaseDelay(settings autoUpdateSettings, version string) (time.Duration, bool) {
	if settings.LastTargetVersion != version || settings.LastAttemptAt.IsZero() {
		setSecuritySetting("panel_auto_update_last_target_version", version)
		setSecuritySetting("panel_auto_update_last_attempt_at", time.Now().Format(time.RFC3339))
		return settings.ReleaseDelay, true
	}
	remaining := settings.ReleaseDelay - time.Since(settings.LastAttemptAt)
	return remaining, remaining > 0
}

func handleWaitingSignature(settings autoUpdateSettings, version string) {
	waitAt := settings.LastSignatureWaitAt
	if settings.LastSignatureWaitVersion != version || waitAt.IsZero() {
		waitAt = time.Now()
		setSecuritySetting("panel_auto_update_signature_wait_version", version)
		setSecuritySetting("panel_auto_update_signature_wait_at", waitAt.Format(time.RFC3339))
	}
	if time.Since(waitAt) > settings.SignatureTimeout {
		msg := "等待签名文件超时，未找到 yub-wpanel.sha256.sig"
		recordPanelUpdateStage("auto", version, "waiting_signature", "failed", msg)
		sendPanelUpdateMail(false, version, "waiting_signature", msg)
		return
	}
	recordPanelUpdateStage("auto", version, "waiting_signature", "waiting", "未找到 yub-wpanel.sha256.sig，等待签名发布")
}

func withinAutoUpdateWindow(window string, now time.Time) bool {
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return false
	}
	start, err1 := parseClock(parts[0])
	end, err2 := parseClock(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if start <= end {
		return cur >= start && cur <= end
	}
	return cur >= start || cur <= end
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func parseStableSemver(version string) ([3]int, bool) {
	var out [3]int
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" || strings.Contains(v, "-") {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func checkUpdateDiskSpace(binaryPath string, cfg *config.Config) error {
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("读取更新包大小失败: %w", err)
	}
	targetDirs := []string{filepath.Dir(panelInstallPath)}
	if cfg != nil && cfg.Panel.BackupDir != "" {
		targetDirs = append(targetDirs, cfg.Panel.BackupDir)
	}
	required := info.Size()*4 + 64*1024*1024
	for _, targetDir := range targetDirs {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(targetDir, &stat); err != nil {
			return fmt.Errorf("检查磁盘空间失败 (%s): %w", targetDir, err)
		}
		free := int64(stat.Bavail) * int64(stat.Bsize)
		if free < required {
			return fmt.Errorf("磁盘剩余空间不足 (%s): 可用 %d MB，需要至少 %d MB", targetDir, free/1024/1024, required/1024/1024)
		}
	}
	return nil
}

func rollbackPlanPath(cfg *config.Config) string {
	return filepath.Join(cfg.Panel.DataDir, "update_rollback.json")
}

func newWatchdogReadyNonce() (string, error) {
	nonce := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

func writeRollbackPlanFile(path string, plan rollbackPlan) (retErr error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("rollback plan path is not an exact absolute path")
	}
	expectedUID := uint32(os.Geteuid())
	if err := validateOwnedDirectory(filepath.Dir(path), expectedUID); err != nil {
		return fmt.Errorf("rollback plan parent is unsafe: %w", err)
	}
	if err := validateRollbackPlanWriteTarget(path, expectedUID); err != nil {
		return err
	}
	plan.PlanPath = path
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".update-rollback.tmp-*")
	if err != nil {
		return fmt.Errorf("create rollback plan temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set rollback plan temporary permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write rollback plan temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync rollback plan temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close rollback plan temporary file: %w", err)
	}
	if err := validateRollbackPlanWriteTarget(path, expectedUID); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace rollback plan: %w", err)
	}
	if err := validateOwnedRegularFile(path, expectedUID, 0o077); err != nil {
		return fmt.Errorf("verify rollback plan after replace: %w", err)
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open rollback plan parent for sync: %w", err)
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("sync rollback plan parent: %w", err)
	}
	return nil
}

func validateRollbackPlanWriteTarget(path string, expectedUID uint32) error {
	if err := validateOwnedRegularFile(path, expectedUID, 0o077); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("rollback plan target is unsafe: %w", err)
	}
	return nil
}

// SignalUpdateWatchdogReady is called only after the watchdog executable has
// passed its special distribution-identity gate and opened the panel database.
// The updater will not restart the service unless it observes this plan-bound,
// root-only nonce and re-confirms the transient watchdog unit is still active.
func SignalUpdateWatchdogReady(cfg *config.Config, configPath, planPath string) error {
	plan, err := loadValidatedUpdateWatchdogPlan(cfg, configPath, planPath)
	if err != nil {
		return err
	}
	return writeWatchdogReadyFile(plan, uint32(os.Geteuid()))
}

func writeWatchdogReadyFile(plan rollbackPlan, expectedUID uint32) error {
	if err := validateWatchdogReadyFields(plan); err != nil {
		return err
	}
	if err := validateOwnedDirectory(filepath.Dir(plan.ReadyPath), expectedUID); err != nil {
		return fmt.Errorf("watchdog ready parent is unsafe: %w", err)
	}
	if err := validateWatchdogReadyWriteTarget(plan.ReadyPath, expectedUID); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(plan.ReadyPath), ".update-watchdog-ready.tmp-*")
	if err != nil {
		return fmt.Errorf("create watchdog ready temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set watchdog ready temporary permissions: %w", err)
	}
	if _, err := io.WriteString(tmp, plan.ReadyNonce+"\n"); err != nil {
		return fmt.Errorf("write watchdog ready temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync watchdog ready temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close watchdog ready temporary file: %w", err)
	}
	if err := validateWatchdogReadyWriteTarget(plan.ReadyPath, expectedUID); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, plan.ReadyPath); err != nil {
		return fmt.Errorf("publish watchdog ready file: %w", err)
	}
	// The ready file is an ephemeral live handshake, not recovery state. Once
	// the atomic rename publishes it, this function must not report a later
	// error that would make the watchdog exit while the updater can observe a
	// seemingly successful handshake. The reader independently revalidates
	// ownership, mode, link count, inode stability, and nonce.
	_ = syncDirectory(filepath.Dir(plan.ReadyPath))
	return nil
}

func validateWatchdogReadyWriteTarget(path string, expectedUID uint32) error {
	if err := validateOwnedRegularFile(path, expectedUID, 0o077); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("watchdog ready target is unsafe: %w", err)
	}
	return nil
}

func readWatchdogReadyFile(plan rollbackPlan, expectedUID uint32) error {
	if err := validateWatchdogReadyFields(plan); err != nil {
		return err
	}
	if err := validateOwnedDirectory(filepath.Dir(plan.ReadyPath), expectedUID); err != nil {
		return fmt.Errorf("watchdog ready parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(plan.ReadyPath, expectedUID, 0o077); err != nil {
		return fmt.Errorf("watchdog ready identity is unsafe: %w", err)
	}
	before, err := os.Lstat(plan.ReadyPath)
	if err != nil {
		return err
	}
	f, err := os.Open(plan.ReadyPath)
	if err != nil {
		return err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("watchdog ready file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(len(plan.ReadyNonce)+2)))
	if err != nil {
		return err
	}
	if string(data) != plan.ReadyNonce+"\n" {
		return fmt.Errorf("watchdog ready nonce mismatch")
	}
	return nil
}

func removeWatchdogReadyFile(plan rollbackPlan) error {
	if err := validateWatchdogReadyFields(plan); err != nil {
		return err
	}
	expectedUID := uint32(os.Geteuid())
	if err := validateOwnedDirectory(filepath.Dir(plan.ReadyPath), expectedUID); err != nil {
		return fmt.Errorf("watchdog ready parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(plan.ReadyPath, expectedUID, 0o077); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("watchdog ready target is unsafe: %w", err)
	}
	if err := os.Remove(plan.ReadyPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(plan.ReadyPath))
}

func validateWatchdogReadyFields(plan rollbackPlan) error {
	if !exactAbsolutePath(plan.PlanPath) || plan.ReadyPath != plan.PlanPath+".ready" || !exactAbsolutePath(plan.ReadyPath) {
		return fmt.Errorf("watchdog ready path is not bound to rollback plan")
	}
	if !validSHA256Hex(plan.ReadyNonce) {
		return fmt.Errorf("watchdog ready nonce is invalid")
	}
	return nil
}

func healthURL(cfg *config.Config) string {
	if cfg == nil {
		return "http://127.0.0.1:8080/healthz"
	}
	port := cfg.Panel.Port
	scheme := "http"
	if cfg.Panel.TLSPort > 0 && cfg.Panel.TLSCertPath != "" && cfg.Panel.TLSKeyPath != "" {
		port = cfg.Panel.TLSPort
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d/healthz", scheme, port)
}

func startUpdateWatchdog(plan rollbackPlan) error {
	if err := validateWatchdogReadyFields(plan); err != nil {
		return err
	}
	backupInfo, err := os.Lstat(plan.BackupBinary)
	if err != nil {
		return fmt.Errorf("inspect rollback backup binary: %w", err)
	}
	if err := removeWatchdogReadyFile(plan); err != nil {
		return fmt.Errorf("clear stale watchdog ready file: %w", err)
	}
	unit := fmt.Sprintf("yub-wpanel-update-watchdog-%d", time.Now().UnixNano())
	cmd := exec.Command(
		"systemd-run",
		"--unit", unit,
		"--collect",
		"--property", "Type=simple",
		"--property", "KillMode=process",
		plan.BackupBinary,
		"--update-watchdog", plan.PlanPath,
		"--config", plan.ConfigPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	unitName := unit + ".service"
	readyAccepted := false
	defer func() {
		if !readyAccepted {
			_ = exec.Command("systemctl", "stop", unitName).Run()
		}
		_ = removeWatchdogReadyFile(plan)
	}()
	deadline := time.Now().Add(watchdogReadyTimeout)
	lastState := "unknown"
	lastReadyErr := error(nil)
	for time.Now().Before(deadline) {
		if err := readWatchdogReadyFile(plan, uint32(os.Geteuid())); err == nil {
			if _, state, err := verifyWatchdogUnitProcess(unitName, backupInfo); err == nil {
				lastState = state
				time.Sleep(100 * time.Millisecond)
				if _, state, err := verifyWatchdogUnitProcess(unitName, backupInfo); err == nil {
					lastState = state
					if err := removeWatchdogReadyFile(plan); err != nil {
						return fmt.Errorf("consume watchdog ready handshake: %w", err)
					}
					readyAccepted = true
					return nil
				} else {
					lastReadyErr = err
				}
			} else {
				lastState = state
				lastReadyErr = err
			}
		} else {
			lastReadyErr = err
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("watchdog ready handshake is unsafe: %w", err)
			}
		}
		stateOut, stateErr := exec.Command("systemctl", "show", unitName, "--property=ActiveState", "--value").Output()
		if stateErr == nil {
			lastState = strings.TrimSpace(string(stateOut))
			if lastState == "failed" || lastState == "inactive" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("watchdog did not complete the protected ready handshake (state=%s ready=%v)", lastState, lastReadyErr)
}

func verifyWatchdogUnitProcess(unitName string, backupInfo os.FileInfo) (int, string, error) {
	stateOut, err := exec.Command("systemctl", "show", unitName, "--property=ActiveState", "--value").Output()
	if err != nil {
		return 0, "unknown", fmt.Errorf("query watchdog unit state: %w", err)
	}
	state := strings.TrimSpace(string(stateOut))
	if state != "active" {
		return 0, state, fmt.Errorf("watchdog unit is not active")
	}
	pidOut, err := exec.Command("systemctl", "show", unitName, "--property=MainPID", "--value").Output()
	if err != nil {
		return 0, state, fmt.Errorf("query watchdog main PID: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil || pid <= 0 {
		return 0, state, fmt.Errorf("watchdog unit has invalid main PID")
	}
	runningInfo, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return 0, state, fmt.Errorf("inspect watchdog executable: %w", err)
	}
	if !os.SameFile(backupInfo, runningInfo) {
		return 0, state, fmt.Errorf("watchdog executable does not match protected backup")
	}
	confirmPIDOut, err := exec.Command("systemctl", "show", unitName, "--property=MainPID", "--value").Output()
	if err != nil || strings.TrimSpace(string(confirmPIDOut)) != strconv.Itoa(pid) {
		return 0, state, fmt.Errorf("watchdog main PID changed during verification")
	}
	confirmStateOut, err := exec.Command("systemctl", "show", unitName, "--property=ActiveState", "--value").Output()
	if err != nil || strings.TrimSpace(string(confirmStateOut)) != "active" {
		return 0, state, fmt.Errorf("watchdog unit left active state during verification")
	}
	return pid, state, nil
}

func healthCheckVersion(rawURL, expectedVersion string) error {
	return healthCheckVersionWithResponder(rawURL, expectedVersion, nil)
}

type panelHealthConnection struct {
	ClientIP   string
	ClientPort int
	ServerIP   string
	ServerPort int
}

type panelHealthResponderVerifier func(panelHealthConnection) error

func healthCheckVersionWithResponder(rawURL, expectedVersion string, verifyResponder panelHealthResponderVerifier) error {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var connectionMu sync.Mutex
	var connection panelHealthConnection
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			identity, err := panelHealthConnectionFromConn(conn)
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			connectionMu.Lock()
			connection = identity
			connectionMu.Unlock()
			return conn, nil
		},
	}
	if strings.HasPrefix(rawURL, "https://") {
		transport.TLSClientConfig = insecureLocalTLSConfig()
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("healthz redirect is not allowed")
		},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	var payload struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload); err != nil {
		return fmt.Errorf("解析 healthz 失败: %w", err)
	}
	if !payload.OK || normalizePanelVersion(payload.Version) != normalizePanelVersion(expectedVersion) {
		return fmt.Errorf("healthz 版本不匹配: got=%s want=%s", payload.Version, expectedVersion)
	}
	if verifyResponder != nil {
		connectionMu.Lock()
		connected := connection
		connectionMu.Unlock()
		if err := verifyResponder(connected); err != nil {
			return fmt.Errorf("panel service responder identity check failed: %w", err)
		}
	}
	return nil
}

func panelHealthConnectionFromConn(conn net.Conn) (panelHealthConnection, error) {
	client, clientOK := conn.LocalAddr().(*net.TCPAddr)
	server, serverOK := conn.RemoteAddr().(*net.TCPAddr)
	if !clientOK || !serverOK || client.IP == nil || server.IP == nil || client.Port <= 0 || server.Port <= 0 {
		return panelHealthConnection{}, fmt.Errorf("healthz connection is not a valid TCP endpoint")
	}
	if !client.IP.IsLoopback() || !server.IP.IsLoopback() {
		return panelHealthConnection{}, fmt.Errorf("healthz connection did not stay on loopback")
	}
	return panelHealthConnection{
		ClientIP:   client.IP.String(),
		ClientPort: client.Port,
		ServerIP:   server.IP.String(),
		ServerPort: server.Port,
	}, nil
}

// healthCheck is intentionally version-agnostic for panel database restore.
// Restoring panel data must also work when the running binary predates the
// version field added to /healthz.
func healthCheck(rawURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	if strings.HasPrefix(rawURL, "https://") {
		client.Transport = &http.Transport{TLSClientConfig: insecureLocalTLSConfig()}
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	return nil
}

func waitForPanelVersion(rawURL, expectedVersion string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = healthCheckManagedPanelVersion(rawURL, expectedVersion, verifyPanelServiceMainProcess)
		if lastErr == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return lastErr
}

func healthCheckManagedPanelVersion(rawURL, expectedVersion string, verifyProcess panelHealthResponderVerifier) error {
	if verifyProcess == nil {
		return fmt.Errorf("panel service process verifier is unavailable")
	}
	return healthCheckVersionWithResponder(rawURL, expectedVersion, verifyProcess)
}

type systemdPropertyReader func(string) (string, error)

func verifyPanelServiceMainProcess(connection panelHealthConnection) error {
	return verifyPanelServiceMainProcessAt(panelInstallPath, 0, connection, func(property string) (string, error) {
		out, err := exec.Command("systemctl", "show", "yub-wpanel.service", "--property="+property, "--value").Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	})
}

func verifyPanelServiceMainProcessAt(expectedBinaryPath string, expectedUID uint32, connection panelHealthConnection, property systemdPropertyReader) error {
	if !exactAbsolutePath(expectedBinaryPath) || property == nil {
		return fmt.Errorf("panel service identity probe is invalid")
	}
	if err := validateOwnedRegularFile(expectedBinaryPath, expectedUID, 0o022); err != nil {
		return fmt.Errorf("panel service canonical binary is unsafe: %w", err)
	}
	expectedInfo, err := os.Lstat(expectedBinaryPath)
	if err != nil {
		return fmt.Errorf("inspect panel service canonical binary: %w", err)
	}
	state, err := property("ActiveState")
	if err != nil {
		return fmt.Errorf("query panel service active state: %w", err)
	}
	if state != "active" {
		return fmt.Errorf("panel service is not active (state=%s)", state)
	}
	pidText, err := property("MainPID")
	if err != nil {
		return fmt.Errorf("query panel service main PID: %w", err)
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return fmt.Errorf("panel service has invalid main PID %q", pidText)
	}
	procExe := fmt.Sprintf("/proc/%d/exe", pid)
	runningPath, err := os.Readlink(procExe)
	if err != nil {
		return fmt.Errorf("read panel service executable path: %w", err)
	}
	if runningPath != expectedBinaryPath {
		return fmt.Errorf("panel service executable path mismatch: got=%s want=%s", runningPath, expectedBinaryPath)
	}
	runningInfo, err := os.Stat(procExe)
	if err != nil {
		return fmt.Errorf("inspect panel service executable inode: %w", err)
	}
	if !os.SameFile(expectedInfo, runningInfo) {
		return fmt.Errorf("panel service executable inode differs from canonical binary")
	}
	if err := verifyProcessOwnsHealthConnection("/proc", pid, connection); err != nil {
		return err
	}
	confirmPID, err := property("MainPID")
	if err != nil || confirmPID != pidText {
		return fmt.Errorf("panel service main PID changed during verification")
	}
	confirmState, err := property("ActiveState")
	if err != nil || confirmState != "active" {
		return fmt.Errorf("panel service left active state during verification")
	}
	confirmRunningInfo, err := os.Stat(procExe)
	if err != nil || !os.SameFile(expectedInfo, confirmRunningInfo) {
		return fmt.Errorf("panel service executable changed during verification")
	}
	if err := verifyProcessOwnsHealthConnection("/proc", pid, connection); err != nil {
		return fmt.Errorf("panel service health connection changed during verification: %w", err)
	}
	return nil
}

func verifyProcessOwnsHealthConnection(procRoot string, pid int, connection panelHealthConnection) error {
	clientIP := net.ParseIP(connection.ClientIP)
	serverIP := net.ParseIP(connection.ServerIP)
	if procRoot == "" || pid <= 0 || clientIP == nil || serverIP == nil ||
		!clientIP.IsLoopback() || !serverIP.IsLoopback() ||
		connection.ClientPort <= 0 || connection.ClientPort > 65535 ||
		connection.ServerPort <= 0 || connection.ServerPort > 65535 {
		return fmt.Errorf("healthz TCP connection identity is invalid")
	}

	fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return fmt.Errorf("inspect panel service file descriptors: %w", err)
	}
	ownedSockets := make(map[string]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if inode != "" {
				ownedSockets[inode] = struct{}{}
			}
		}
	}
	if len(ownedSockets) == 0 {
		return fmt.Errorf("panel service MainPID owns no sockets")
	}

	readTable := false
	for _, tableName := range []string{"tcp", "tcp6"} {
		matched, exists, err := processOwnsHealthConnectionInTable(
			filepath.Join(procRoot, strconv.Itoa(pid), "net", tableName),
			connection,
			ownedSockets,
		)
		if err != nil {
			return err
		}
		readTable = readTable || exists
		if matched {
			return nil
		}
	}
	if !readTable {
		return fmt.Errorf("panel service TCP socket tables are unavailable")
	}
	return fmt.Errorf("healthz TCP connection is not owned by the stable systemd MainPID")
}

func processOwnsHealthConnectionInTable(path string, connection panelHealthConnection, ownedSockets map[string]struct{}) (bool, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("open panel service TCP socket table: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "01" {
			continue
		}
		localIP, localPort, err := parseProcNetTCPEndpoint(fields[1])
		if err != nil {
			continue
		}
		remoteIP, remotePort, err := parseProcNetTCPEndpoint(fields[2])
		if err != nil {
			continue
		}
		if !localIP.Equal(net.ParseIP(connection.ServerIP)) || localPort != connection.ServerPort ||
			!remoteIP.Equal(net.ParseIP(connection.ClientIP)) || remotePort != connection.ClientPort {
			continue
		}
		if _, ok := ownedSockets[fields[9]]; ok {
			return true, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, true, fmt.Errorf("read panel service TCP socket table: %w", err)
	}
	return false, true, nil
}

func parseProcNetTCPEndpoint(raw string) (net.IP, int, error) {
	separator := strings.LastIndexByte(raw, ':')
	if separator < 0 || separator == len(raw)-1 {
		return nil, 0, fmt.Errorf("invalid proc TCP endpoint")
	}
	addressBytes, err := hex.DecodeString(raw[:separator])
	if err != nil || (len(addressBytes) != net.IPv4len && len(addressBytes) != net.IPv6len) {
		return nil, 0, fmt.Errorf("invalid proc TCP address")
	}
	if len(addressBytes) == net.IPv4len {
		for left, right := 0, len(addressBytes)-1; left < right; left, right = left+1, right-1 {
			addressBytes[left], addressBytes[right] = addressBytes[right], addressBytes[left]
		}
	} else {
		for wordStart := 0; wordStart < len(addressBytes); wordStart += 4 {
			addressBytes[wordStart], addressBytes[wordStart+3] = addressBytes[wordStart+3], addressBytes[wordStart]
			addressBytes[wordStart+1], addressBytes[wordStart+2] = addressBytes[wordStart+2], addressBytes[wordStart+1]
		}
	}
	port, err := strconv.ParseUint(raw[separator+1:], 16, 16)
	if err != nil || port == 0 {
		return nil, 0, fmt.Errorf("invalid proc TCP port")
	}
	return net.IP(addressBytes), int(port), nil
}

func normalizePanelVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func insecureLocalTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

func shouldRestoreDBAfterHealthFailure(backupPath string) bool {
	if backupPath == "" {
		return false
	}
	db := database.GetDB()
	if db == nil {
		return true
	}
	var version string
	if err := db.QueryRow("SELECT version FROM schema_version ORDER BY updated_at DESC, rowid DESC LIMIT 1").Scan(&version); err != nil {
		return true
	}
	for _, table := range []string{"admin_users", "websites", "security_settings"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table + " LIMIT 1").Scan(&n); err != nil {
			return true
		}
	}
	return false
}

func restoreDBFile(cfg *config.Config, backupPath string) (retErr error) {
	if cfg == nil || backupPath == "" {
		return nil
	}
	targetPath := cfg.SQLite.Path
	if !exactAbsolutePath(backupPath) || !exactAbsolutePath(targetPath) || backupPath == targetPath {
		return fmt.Errorf("database rollback paths must be distinct exact absolute paths")
	}
	expectedUID := uint32(os.Geteuid())
	if err := validateOwnedDirectory(filepath.Dir(backupPath), expectedUID); err != nil {
		return fmt.Errorf("database backup parent is unsafe: %w", err)
	}
	if err := validateOwnedRegularFile(backupPath, expectedUID, 0o022); err != nil {
		return fmt.Errorf("database backup identity is unsafe: %w", err)
	}
	if err := validateOwnedDirectory(filepath.Dir(targetPath), expectedUID); err != nil {
		return fmt.Errorf("database target parent is unsafe: %w", err)
	}
	if err := validateDatabaseRestoreTarget(targetPath, expectedUID); err != nil {
		return err
	}

	backupBefore, err := os.Lstat(backupPath)
	if err != nil {
		return fmt.Errorf("inspect database backup: %w", err)
	}
	src, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("open database backup: %w", err)
	}
	defer src.Close()
	backupOpened, err := src.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened database backup: %w", err)
	}
	if !os.SameFile(backupBefore, backupOpened) {
		return fmt.Errorf("database backup changed while opening")
	}

	tmp, err := os.CreateTemp(filepath.Dir(targetPath), "."+filepath.Base(targetPath)+".rollback-*")
	if err != nil {
		return fmt.Errorf("create database rollback temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
		_ = os.Remove(tmpPath + "-journal")
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set database rollback temporary permissions: %w", err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("copy database backup to rollback temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync database rollback temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close database rollback temporary file: %w", err)
	}
	if err := database.VerifyDBBackup(tmpPath); err != nil {
		return fmt.Errorf("verify database rollback temporary file: %w", err)
	}
	if err := database.Close(); err != nil {
		return fmt.Errorf("close watchdog database before rollback: %w", err)
	}

	quarantined, err := quarantineSQLiteSidecars(targetPath, expectedUID)
	if err != nil {
		return err
	}
	restoreSidecars := true
	defer func() {
		if restoreSidecars {
			retErr = errors.Join(retErr, restoreQuarantinedSQLiteSidecars(quarantined))
			if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("sync database directory after restoring sidecars: %w", err))
			}
		}
	}()
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return fmt.Errorf("sync database directory after sidecar quarantine: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("atomically replace panel database: %w", err)
	}
	// Once the main database rename succeeds, old WAL/SHM/journal files must
	// never be restored beside it. A later error therefore leaves the service
	// stopped and the verified backup/plan intact for an operator retry.
	restoreSidecars = false
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return fmt.Errorf("sync database directory after replacement: %w", err)
	}
	if err := removeQuarantinedSQLiteSidecars(quarantined); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(targetPath)); err != nil {
		return fmt.Errorf("sync database directory after sidecar cleanup: %w", err)
	}
	return nil
}

func exactAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateDatabaseRestoreTarget(path string, expectedUID uint32) error {
	if err := validateOwnedRegularFile(path, expectedUID, 0o022); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("database restore target is unsafe: %w", err)
	}
	return nil
}

type quarantinedSQLiteSidecar struct {
	originalPath   string
	quarantinePath string
}

func quarantineSQLiteSidecars(databasePath string, expectedUID uint32) ([]quarantinedSQLiteSidecar, error) {
	var quarantined []quarantinedSQLiteSidecar
	abort := func(cause error) ([]quarantinedSQLiteSidecar, error) {
		if restoreErr := restoreQuarantinedSQLiteSidecars(quarantined); restoreErr != nil {
			cause = errors.Join(cause, restoreErr)
		}
		if syncErr := syncDirectory(filepath.Dir(databasePath)); syncErr != nil {
			cause = errors.Join(cause, fmt.Errorf("sync database directory after aborting sidecar quarantine: %w", syncErr))
		}
		return nil, cause
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := databasePath + suffix
		if err := validateOwnedRegularFile(path, expectedUID, 0o022); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return abort(fmt.Errorf("database sidecar %s is unsafe: %w", suffix, err))
		}
		placeholder, err := os.CreateTemp(filepath.Dir(databasePath), "."+filepath.Base(databasePath)+suffix+".quarantine-*")
		if err != nil {
			return abort(fmt.Errorf("reserve database sidecar quarantine path: %w", err))
		}
		quarantinePath := placeholder.Name()
		if err := placeholder.Close(); err != nil {
			_ = os.Remove(quarantinePath)
			return abort(fmt.Errorf("close database sidecar quarantine placeholder: %w", err))
		}
		if err := os.Remove(quarantinePath); err != nil {
			return abort(fmt.Errorf("prepare database sidecar quarantine path: %w", err))
		}
		if err := os.Rename(path, quarantinePath); err != nil {
			return abort(fmt.Errorf("quarantine database sidecar %s: %w", suffix, err))
		}
		quarantined = append(quarantined, quarantinedSQLiteSidecar{originalPath: path, quarantinePath: quarantinePath})
	}
	return quarantined, nil
}

func restoreQuarantinedSQLiteSidecars(quarantined []quarantinedSQLiteSidecar) error {
	var restoreErr error
	for i := len(quarantined) - 1; i >= 0; i-- {
		item := quarantined[i]
		if err := os.Rename(item.quarantinePath, item.originalPath); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore quarantined database sidecar: %w", err))
		}
	}
	return restoreErr
}

func removeQuarantinedSQLiteSidecars(quarantined []quarantinedSQLiteSidecar) error {
	var removeErr error
	for _, item := range quarantined {
		if err := os.Remove(item.quarantinePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove quarantined database sidecar: %w", err))
		}
	}
	return removeErr
}

func stageLabel(stage string) string {
	switch stage {
	case "fetch_release":
		return "获取版本信息"
	case "compare_version":
		return "版本比较"
	case "resolve_assets":
		return "解析发布文件"
	case "waiting_release_delay":
		return "等待发布延迟"
	case "waiting_signature":
		return "等待签名文件"
	case "version_policy":
		return "版本策略检查"
	case "download_binary":
		return "下载二进制"
	case "download_sha256":
		return "下载校验文件"
	case "download_signature":
		return "下载签名文件"
	case "verify_signature":
		return "校验签名"
	case "verify_sha256":
		return "校验完整性"
	case "preflight":
		return "预检新版本"
	case "disk_check":
		return "磁盘空间检查"
	case "backup_binary":
		return "备份旧版本"
	case "backup_database":
		return "备份数据库"
	case "write_rollback_plan":
		return "写入回滚计划"
	case "replace_binary":
		return "替换二进制"
	case "start_watchdog":
		return "启动健康检查"
	case "restart":
		return "重启面板"
	case "health_check":
		return "健康检查"
	case "rollback_binary":
		return "回滚二进制"
	case "rollback_database":
		return "回滚数据库"
	case "rollback_stop":
		return "停止新版本服务"
	case "rollback_validate":
		return "复核回滚身份"
	case "rollback_start":
		return "启动旧版本服务"
	case "rollback_health":
		return "旧版本健康检查"
	default:
		return stage
	}
}

func sendPanelUpdateMail(success bool, targetVersion, stage, message string) {
	cfg := GetSMTPConfig()
	if cfg == nil || cfg.Host == "" || cfg.AdminEmail == "" {
		return
	}
	status := "失败"
	if success {
		status = "成功"
	}
	body := fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; padding: 20px; color: #333;">
<h2>YUB WPanel 自动更新%s</h2>
<p>目标版本：%s</p>
<p>阶段：%s</p>
<p>详情：%s</p>
<p style="font-size: 12px; color: #aaa; margin-top: 20px;">来自 %s 面板</p>
</body></html>`, status, html.EscapeString(targetVersion), html.EscapeString(stageLabel(stage)), html.EscapeString(message), html.EscapeString(getPanelTitle()))
	if err := SendMail("", getPanelTitle()+" 自动更新"+status, body); err != nil {
		log.Printf("自动更新邮件发送失败: %v", err)
	}
}

func readSecuritySetting(key string) string {
	db := database.GetDB()
	if db == nil {
		return ""
	}
	var v string
	_ = db.QueryRow("SELECT svalue FROM security_settings WHERE skey = ?", key).Scan(&v)
	return v
}

func setSecuritySetting(key, value string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	_, _ = db.Exec(`INSERT INTO security_settings (skey, svalue, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, updated_at = excluded.updated_at`, key, value)
}

func clearPanelUpdateCache() {
	panelUpdateCache.mu.Lock()
	panelUpdateCache.lastAt = time.Time{}
	panelUpdateCache.latest = ""
	panelUpdateCache.message = ""
	panelUpdateCache.mu.Unlock()
}

func cleanupPanelUpdateBackups(plan rollbackPlan) {
	if plan.BackupDB != "" {
		backupDir := filepath.Dir(plan.BackupDB)
		if removed := database.CleanupOldDBBackups(backupDir, panelDBBackupKeep); removed > 0 {
			recordOperationLog("panel_"+plan.Trigger+"_update", plan.TargetVersion, "success", fmt.Sprintf("cleanup_database_backups: 已清理 %d 份旧数据库备份", removed))
		}
	}
	if removed := cleanupPanelBinaryBackups(panelBinaryBackupKeep); removed > 0 {
		recordOperationLog("panel_"+plan.Trigger+"_update", plan.TargetVersion, "success", fmt.Sprintf("cleanup_binary_backups: 已清理 %d 份旧二进制备份", removed))
	}
}

func cleanupPanelBinaryBackups(keep int) int {
	if keep <= 0 {
		keep = panelBinaryBackupKeep
	}
	matches, err := filepath.Glob(panelInstallPath + ".bak.*")
	if err != nil || len(matches) <= keep {
		return 0
	}
	sort.Slice(matches, func(i, j int) bool {
		ii, ierr := os.Stat(matches[i])
		ji, jerr := os.Stat(matches[j])
		if ierr == nil && jerr == nil && !ii.ModTime().Equal(ji.ModTime()) {
			return ii.ModTime().After(ji.ModTime())
		}
		return matches[i] > matches[j]
	})
	removed := 0
	for _, path := range matches[keep:] {
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}

func LocalOnly(c net.Addr) bool {
	host, _, err := net.SplitHostPort(c.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
