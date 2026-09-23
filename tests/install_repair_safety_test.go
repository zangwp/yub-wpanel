package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInstallRepairPreflightRunsBeforeAPT(t *testing.T) {
	script := readInstallScript(t, installScriptPath)

	preflight := requiredIndex(t, script, `log_info "repair预检与备份完成"`)
	apt := requiredIndex(t, script, `log_info "配置 APT 源..."`)
	if preflight >= apt {
		t.Fatalf("repair preflight offset=%d must be before APT offset=%d", preflight, apt)
	}
	for _, required := range []string{
		`--repair-config-check --config "$CONFIG_FILE"`,
		`validate_existing_panel_binary`,
		`binary_owner=$(stat -c '%u' "$BIN_PATH"`,
		`binary_mode=$(stat -c '%a' "$BIN_PATH"`,
		`binary_links=$(stat -c '%h' "$BIN_PATH"`,
		`[[ "$binary_owner" == "0" ]]`,
		`[[ "$binary_mode" == "755" ]]`,
		`[[ "$binary_links" == "1" ]]`,
		`[[ "$binary_path" == "$BIN_PATH" ]]`,
		`validate_repair_service_unit`,
		`cmp -s -- "$expected_unit" "$SERVICE_PATH"`,
		`(8#$unit_mode & 0022) == 0`,
		`systemd-analyze unit-paths`,
		`yub-wpanel.service.d yub-.service.d service.d`,
		`--property=FragmentPath --value`,
		`--property=DropInPaths --value`,
		`--property=ExecStart --value`,
		`--property=MainPID --value`,
		`[[ "$confirmed_pid" == "$main_pid" ]]`,
		`readlink -- "/proc/${main_pid}/exe"`,
		`validate_existing_panel_cron_file`,
		`local cron_parent="${CRON_PATH%/*}"`,
		`[[ -d "$cron_parent" ]] && [[ ! -L "$cron_parent" ]]`,
		`parent_owner=$(stat -c '%u' "$cron_parent"`,
		`(8#$parent_mode & 0022) == 0`,
		`现有systemd unit不是YUB WPanel生成的精确安全版本，或存在drop-in`,
		`现有cron文件不是root安全持有的YUB WPanel受管文件`,
		`fresh安装前cron父目录或现有cron文件身份不安全`,
		`config.json，无法安全repair`,
		`repair模式保持config.json字节不变`,
		`repair模式保留现有登录凭据和安全入口`,
		`repair模式保留现有TLS证书与私钥`,
		`repair模式保留现有MariaDB身份与配置`,
		`sha256sum -c "$REPAIR_BACKUP_DIR/SHA256SUMS"`,
		`sha256sum sort stat`,
		`wc xargs`,
		`REPAIR_LICENSE_DIR_EXISTED=true`,
		`cp -a -- "$LICENSE_DOC_DIR" "$REPAIR_BACKUP_DIR/license-docs"`,
		`atomic_install_license_document "$LICENSE_RELEASE_VERSION_FILE" "$LICENSE_DOC_DIR/RELEASE_VERSION"`,
		`atomic_install_managed_file "$PANEL_CANDIDATE" "$BIN_PATH" 0755`,
		`mktemp "${target_dir}/.${target_base}.yub-install.XXXXXXXX"`,
		`sync -f "$ATOMIC_STAGE_PATH"`,
		`sync -f "$target_dir"`,
	} {
		requiredIndex(t, script, required)
	}
}

func TestRepairReleaseDocumentationInventoryIncludesReleaseVersion(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	backup := extractShellFunction(t, script, "create_repair_backup", "prepare_repair_snapshot")
	installDocuments := extractShellFunction(t, script, "install_release_license_documentation", "repair_rollback")
	rollback := extractShellFunction(t, script, "repair_rollback", "apt_package_available")

	for _, name := range []string{
		"LICENSE",
		"NOTICE.md",
		"THIRD_PARTY_NOTICES.md",
		"RELEASE_VERSION",
		"yub-wpanel-third-party-licenses.tar.gz",
		"yub-wpanel-third-party-licenses.tar.gz.sha256",
		"yub-wpanel-third-party-licenses.tar.gz.sha256.sig",
		"release-public-key.pem",
	} {
		if !strings.Contains(backup, name) {
			t.Errorf("repair snapshot omits release document %q", name)
		}
		if !strings.Contains(installDocuments, `"$LICENSE_DOC_DIR/`+name+`"`) {
			t.Errorf("release documentation install omits %q", name)
		}
		if !strings.Contains(rollback, name) {
			t.Errorf("repair rollback omits release document %q", name)
		}
	}
}

func TestInstallFreshValidatesCronParentBeforeAPT(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	freshCronCheck := requiredIndex(t, script, `fresh安装前cron父目录或现有cron文件身份不安全`)
	apt := requiredIndex(t, script, `log_info "配置 APT 源..."`)
	if freshCronCheck >= apt {
		t.Fatalf("fresh cron parent check offset=%d must precede APT offset=%d", freshCronCheck, apt)
	}
	for _, required := range []string{
		`validate_fresh_panel_cron_location`,
		`while [[ ! -e "$nearest_parent" ]]`,
		`[[ -d "$cron_parent" ]] && [[ ! -L "$cron_parent" ]]`,
		`[[ "$parent_owner" == "0" ]]`,
		`(8#$parent_mode & 0022) == 0`,
		`可选运行统计 / Optional Runtime Telemetry`,
	} {
		requiredIndex(t, script, required)
	}
	if strings.Contains(script, "Anonymous Install Telemetry") || strings.Contains(script, "匿名安装统计") {
		t.Fatal("installer still describes stable runtime telemetry as anonymous install telemetry")
	}
	componentComplete := requiredIndex(t, script, `log_info "基础组件安装完成"`)
	strictAfterInstall := requiredIndex(t, script, `cron安装后 /etc/cron.d 身份不安全`)
	if strictAfterInstall <= componentComplete {
		t.Fatalf("strict cron parent recheck offset=%d must follow cron package installation offset=%d", strictAfterInstall, componentComplete)
	}
}

func TestFreshCronPreflightAllowsMissingCronPackageDirectoryButRejectsBrokenLink(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	validator := extractShellFunction(t, script, "validate_fresh_panel_cron_location", "copy_local_release_bundle")
	root := t.TempDir()
	cronPath := filepath.Join(root, "missing", "cron.d", "yub_wpanel_cron")
	shell := fmt.Sprintf(`
CRON_PATH=%s
stat() {
    case "$2" in
        %%u) printf '0\n' ;;
        %%a) printf '755\n' ;;
        *) return 1 ;;
    esac
}
%s
validate_fresh_panel_cron_location
`, strconv.Quote(cronPath), validator)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("missing cron package directory was rejected: %v\n%s", err, output)
	}

	brokenParent := filepath.Join(root, "broken")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), brokenParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	brokenCronPath := filepath.Join(brokenParent, "yub_wpanel_cron")
	shell = fmt.Sprintf("CRON_PATH=%s\n%s\nvalidate_fresh_panel_cron_location", strconv.Quote(brokenCronPath), validator)
	cmd = exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("broken cron parent symlink was accepted: %s", output)
	}
}

func TestInstallRepairInstallsSQLiteOnlyAfterSafeConfigCheck(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	configCheck := requiredIndex(t, script, `--repair-config-check --config "$CONFIG_FILE"`)
	binaryCheck := requiredIndex(t, script, `validate_existing_panel_binary ||`)
	unitCheck := requiredIndex(t, script, `validate_repair_service_unit ||`)
	cronCheck := requiredIndex(t, script, `validate_existing_panel_cron_file ||`)
	sqliteInstall := requiredIndex(t, script, `DEBIAN_FRONTEND=noninteractive apt-get install -y sqlite3`)
	backupAfterInstall := strings.Index(script[sqliteInstall:], "\n    prepare_repair_snapshot")
	if backupAfterInstall < 0 {
		t.Fatal("repair sqlite dependency install must be followed by prepare_repair_snapshot call")
	}
	backup := sqliteInstall + backupAfterInstall
	if !(configCheck < binaryCheck && binaryCheck < unitCheck && unitCheck < cronCheck && cronCheck < sqliteInstall && sqliteInstall < backup) {
		t.Fatalf("repair safety order is invalid: config_check=%d binary_check=%d unit_check=%d cron_check=%d sqlite_install=%d backup=%d", configCheck, binaryCheck, unitCheck, cronCheck, sqliteInstall, backup)
	}

	componentInstall := requiredIndex(t, script, "apt-get install -y \\\n    iproute2 \\")
	componentComplete := requiredIndex(t, script, `log_info "基础组件安装完成"`)
	if strings.Contains(script[componentInstall:componentComplete], "sqlite3") {
		t.Fatal("fresh install must not install repair-only sqlite3 dependency")
	}
}

func TestRepairSnapshotStopsActiveServiceAndRestoresItWhenSnapshotFails(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	snapshot := extractShellFunction(t, script, "prepare_repair_snapshot", "atomic_install_managed_file")
	rollback := extractShellFunction(t, script, "repair_rollback", "apt_package_available")

	stopAt := requiredIndex(t, snapshot, `systemctl stop yub-wpanel`)
	backupAt := requiredIndex(t, snapshot, `create_repair_backup`)
	if stopAt >= backupAt {
		t.Fatalf("active service stop offset=%d must precede snapshot offset=%d", stopAt, backupAt)
	}
	if strings.Contains(snapshot[stopAt:backupAt], "systemctl start yub-wpanel") {
		t.Fatal("repair service restarted in the stop-to-snapshot window")
	}
	snapshotCall := requiredIndex(t, script, "\n    prepare_repair_snapshot\n")
	activeRestoreRelative := strings.Index(script[snapshotCall:], `systemctl start yub-wpanel || log_error "repair后yub-wpanel启动失败"`)
	if activeRestoreRelative < 0 {
		t.Fatal("active repair does not restore the service after deployment")
	}
	activeRestore := snapshotCall + activeRestoreRelative
	if strings.Contains(script[snapshotCall:activeRestore], "systemctl start yub-wpanel") {
		t.Fatal("active repair starts the panel between its snapshot and final deployment")
	}

	logPath := filepath.Join(t.TempDir(), "events.log")
	shell := fmt.Sprintf(`set -u
REPAIR_MODE=true
REPAIR_COMMITTED=false
REPAIR_MUTATED=false
REPAIR_BACKUP_DIR=""
REPAIR_SERVICE_WAS_ACTIVE=false
REPAIR_SERVICE_STOPPED_FOR_SNAPSHOT=false
SERVICE_STATE=active
EVENT_LOG=%s
log_error() { return 1; }
log_warn() { :; }
systemctl() {
    case "$1" in
        is-active) [[ "$SERVICE_STATE" == active ]] ;;
        stop) printf 'stop\n' >> "$EVENT_LOG"; SERVICE_STATE=inactive ;;
        start) printf 'start\n' >> "$EVENT_LOG"; SERVICE_STATE=active ;;
        *) return 1 ;;
    esac
}
create_repair_backup() {
    printf 'snapshot:%%s\n' "$SERVICE_STATE" >> "$EVENT_LOG"
    return 1
}
%s
%s
if prepare_repair_snapshot; then
    exit 91
fi
test "$REPAIR_SERVICE_WAS_ACTIVE" = true
test "$REPAIR_SERVICE_STOPPED_FOR_SNAPSHOT" = true
test "$SERVICE_STATE" = inactive
repair_rollback
test "$SERVICE_STATE" = active
`, strconv.Quote(logPath), snapshot, rollback)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repair snapshot failure fixture failed: %v\n%s", err, output)
	}
	assertTestFile(t, logPath, "stop\nsnapshot:inactive\nstart\n")

	t.Run("stop reports failure after service became inactive", func(t *testing.T) {
		failedStopLog := filepath.Join(t.TempDir(), "events.log")
		failedStopShell := fmt.Sprintf(`set -u
REPAIR_MODE=true
REPAIR_COMMITTED=false
REPAIR_MUTATED=false
REPAIR_BACKUP_DIR=""
REPAIR_SERVICE_WAS_ACTIVE=false
REPAIR_SERVICE_STOPPED_FOR_SNAPSHOT=false
SERVICE_STATE=active
EVENT_LOG=%s
log_error() { return 1; }
log_warn() { printf 'warn:%%s\n' "$1" >> "$EVENT_LOG"; }
systemctl() {
    case "$1" in
        is-active) [[ "$SERVICE_STATE" == active ]] ;;
        stop) printf 'stop-error\n' >> "$EVENT_LOG"; SERVICE_STATE=inactive; return 1 ;;
        start) printf 'start\n' >> "$EVENT_LOG"; SERVICE_STATE=active ;;
        *) return 1 ;;
    esac
}
create_repair_backup() { printf 'snapshot:%%s\n' "$SERVICE_STATE" >> "$EVENT_LOG"; }
%s
%s
prepare_repair_snapshot
test "$SERVICE_STATE" = inactive
repair_rollback
test "$SERVICE_STATE" = active
`, strconv.Quote(failedStopLog), snapshot, rollback)
		cmd := exec.Command("bash", "-c", failedStopShell)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("reported stop failure fixture failed: %v\n%s", err, output)
		}
		assertTestFile(t, failedStopLog, "stop-error\nwarn:systemctl stop返回失败，但yub-wpanel已停止；继续创建repair快照\nsnapshot:inactive\nstart\n")
	})
}

func TestRepairRollbackFaultInjection(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	rollback := extractShellFunction(t, script, "repair_rollback", "apt_package_available")

	t.Run("restores existing files and active service", func(t *testing.T) {
		root := t.TempDir()
		backup := filepath.Join(root, "backup")
		certDir := filepath.Join(root, "certs")
		mustWriteTestFile(t, filepath.Join(backup, "yub-wpanel"), "old-binary", 0755)
		mustWriteTestFile(t, filepath.Join(backup, "yub-wpanel.service"), "old-unit", 0644)
		mustWriteTestFile(t, filepath.Join(backup, "panel.crt"), "old-cert", 0644)
		mustWriteTestFile(t, filepath.Join(backup, "panel.key"), "old-key", 0600)
		mustWriteTestFile(t, filepath.Join(backup, "panel.db"), "old-database", 0600)
		binary := filepath.Join(root, "yub-wpanel")
		unit := filepath.Join(root, "yub-wpanel.service")
		database := filepath.Join(root, "panel.db")
		mustWriteTestFile(t, binary, "new-binary", 0755)
		mustWriteTestFile(t, unit, "new-unit", 0644)
		mustWriteTestFile(t, database, "new-database", 0600)
		mustWriteTestFile(t, database+"-wal", "new-wal", 0600)
		mustWriteTestFile(t, database+"-shm", "new-shm", 0600)
		mustWriteTestFile(t, filepath.Join(certDir, "panel.crt"), "new-cert", 0644)
		mustWriteTestFile(t, filepath.Join(certDir, "panel.key"), "new-key", 0600)
		logPath := filepath.Join(root, "systemctl.log")

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, true, true, true, true, true, false, "preserve", false)
		assertTestFile(t, binary, "old-binary")
		assertTestFile(t, unit, "old-unit")
		assertTestFile(t, filepath.Join(certDir, "panel.crt"), "old-cert")
		assertTestFile(t, filepath.Join(certDir, "panel.key"), "old-key")
		assertTestFile(t, database, "old-database")
		for _, sidecar := range []string{database + "-wal", database + "-shm"} {
			if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
				t.Fatalf("SQLite sidecar still exists after rollback: %s", sidecar)
			}
		}
		assertTestFile(t, logPath, "stop yub-wpanel\ndaemon-reload\nstart yub-wpanel\n")
	})

	t.Run("removes newly created files and preserves inactive service", func(t *testing.T) {
		root := t.TempDir()
		backup := filepath.Join(root, "backup")
		licenseDir := filepath.Join(root, "license-root", "yub-wpanel")
		if err := os.MkdirAll(backup, 0700); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(root, "yub-wpanel")
		unit := filepath.Join(root, "yub-wpanel.service")
		database := filepath.Join(root, "panel.db")
		mustWriteTestFile(t, binary, "new-binary", 0755)
		mustWriteTestFile(t, unit, "new-unit", 0644)
		mustWriteTestFile(t, filepath.Join(root, "certs", "panel.crt"), "new-cert", 0644)
		mustWriteTestFile(t, filepath.Join(root, "certs", "panel.key"), "new-key", 0600)
		mustWriteTestFile(t, database, "new-database", 0600)
		mustWriteTestFile(t, database+"-wal", "new-wal", 0600)
		mustWriteTestFile(t, filepath.Join(licenseDir, "LICENSE"), "new-license", 0644)
		logPath := filepath.Join(root, "systemctl.log")

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, false, false, false, false, false, false, "generate", false)
		for _, path := range []string{binary, unit, database, database + "-wal", filepath.Join(root, "certs", "panel.crt"), filepath.Join(root, "certs", "panel.key"), licenseDir} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("new repair file still exists after rollback: %s", path)
			}
		}
		assertTestFile(t, logPath, "stop yub-wpanel\ndaemon-reload\nstop yub-wpanel\n")
	})

	t.Run("keeps old service stopped when database restore fails", func(t *testing.T) {
		root := t.TempDir()
		backup := filepath.Join(root, "backup")
		mustWriteTestFile(t, filepath.Join(backup, "yub-wpanel"), "old-binary", 0755)
		mustWriteTestFile(t, filepath.Join(backup, "panel.db"), "old-database", 0600)
		binary := filepath.Join(root, "yub-wpanel")
		unit := filepath.Join(root, "yub-wpanel.service")
		database := filepath.Join(root, "panel.db")
		mustWriteTestFile(t, binary, "new-binary", 0755)
		mustWriteTestFile(t, database, "new-database", 0600)
		logPath := filepath.Join(root, "systemctl.log")

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, true, false, false, true, true, false, "preserve", true)
		assertTestFile(t, binary, "old-binary")
		assertTestFile(t, database, "new-database")
		assertTestFile(t, logPath, "stop yub-wpanel\ndaemon-reload\nstop yub-wpanel\n")
	})

	t.Run("restores release documents and preserves existing inactive service", func(t *testing.T) {
		root := t.TempDir()
		backup := filepath.Join(root, "backup")
		licenseBackup := filepath.Join(backup, "license-docs")
		licenseDir := filepath.Join(root, "license-root", "yub-wpanel")
		mustWriteTestFile(t, filepath.Join(backup, "yub-wpanel"), "old-binary", 0755)
		mustWriteTestFile(t, filepath.Join(backup, "panel.db"), "old-database", 0600)
		oldReleaseDocuments := map[string]string{
			"LICENSE":                                "old-license",
			"NOTICE.md":                              "old-notice",
			"THIRD_PARTY_NOTICES.md":                 "old-third-party-notice",
			"RELEASE_VERSION":                        "v2.0.1\n",
			"yub-wpanel-third-party-licenses.tar.gz": "old-license-archive",
			"yub-wpanel-third-party-licenses.tar.gz.sha256":     "old-license-checksum",
			"yub-wpanel-third-party-licenses.tar.gz.sha256.sig": "old-license-signature",
			"release-public-key.pem":                            "old-release-key",
		}
		for name, contents := range oldReleaseDocuments {
			mustWriteTestFile(t, filepath.Join(licenseBackup, name), contents, 0644)
		}
		binary := filepath.Join(root, "yub-wpanel-bin")
		unit := filepath.Join(root, "yub-wpanel.service")
		database := filepath.Join(root, "panel.db")
		mustWriteTestFile(t, binary, "new-binary", 0755)
		mustWriteTestFile(t, database, "new-database", 0600)
		for name := range oldReleaseDocuments {
			mustWriteTestFile(t, filepath.Join(licenseDir, name), "new-"+name, 0644)
		}
		logPath := filepath.Join(root, "systemctl.log")

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, true, false, false, true, false, true, "preserve", false)
		assertTestFile(t, binary, "old-binary")
		assertTestFile(t, database, "old-database")
		for name, contents := range oldReleaseDocuments {
			assertTestFile(t, filepath.Join(licenseDir, name), contents)
		}
		assertTestFile(t, logPath, "stop yub-wpanel\ndaemon-reload\nstop yub-wpanel\n")
	})
}

func TestRepairRollbackRuntimeRestoreFailuresPreventRestart(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	rollback := extractShellFunction(t, script, "repair_rollback", "apt_package_available")

	for _, test := range []struct {
		name        string
		failure     string
		wantWarning string
	}{
		{name: "backup unavailable", failure: "missing-backup", wantWarning: "repair备份目录不可用"},
		{name: "service remains active", failure: "stop-still-active", wantWarning: "回滚时yub-wpanel仍在运行"},
		{name: "database restore", failure: "database", wantWarning: "面板数据库恢复失败"},
		{name: "binary restore", failure: "binary", wantWarning: "面板二进制原子恢复失败"},
		{name: "unit restore", failure: "unit", wantWarning: "systemd unit恢复失败"},
		{name: "TLS restore", failure: "tls", wantWarning: "TLS身份恢复失败"},
		{name: "license restore", failure: "license", wantWarning: "许可文档目录未能完整恢复"},
		{name: "daemon reload", failure: "daemon-reload", wantWarning: "systemd daemon-reload失败"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := runRollbackRuntimeFailureFixture(t, rollback, test.failure)
			logData, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			logText := string(logData)
			if strings.Contains(logText, "start yub-wpanel\n") {
				t.Fatalf("rollback restarted service after %s failure:\n%s", test.failure, logText)
			}
			if !strings.Contains(logText, "stop yub-wpanel\n") || !strings.Contains(logText, test.wantWarning) {
				t.Fatalf("rollback did not fail closed with an explicit warning after %s failure:\n%s", test.failure, logText)
			}
		})
	}

	t.Run("restart failure is explicit", func(t *testing.T) {
		logPath := runRollbackRuntimeFailureFixture(t, rollback, "start")
		logData, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		logText := string(logData)
		if !strings.Contains(logText, "start yub-wpanel\n") || !strings.Contains(logText, "无法恢复原active状态") {
			t.Fatalf("rollback restart failure was not explicit:\n%s", logText)
		}
	})
}

func TestInstallRepairDoesNotParseExistingJSONWithTextTools(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	for _, forbidden := range []string{
		`grep -o '"root_password"`,
		`sed -i "$CONFIG_FILE"`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("install.sh contains forbidden repair/config pattern %q", forbidden)
		}
	}
}

func TestInstallAndRepairRequireExactRuntimeHealthBeforeCommit(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	health := extractShellFunction(t, script, "validate_running_panel_health", "validate_existing_panel_cron_file")
	for _, required := range []string{
		`panel_info=$(timeout 20s "$BIN_PATH" --info --config "$CONFIG_FILE"`,
		`[[ "$reported_version" == "$expected_version" ]]`,
		`--property=MainPID --value`,
		`readlink -- "/proc/${main_pid}/exe"`,
		`ss -H -ltnp "sport = :${tls_port}"`,
		`grep -Fq "pid=${main_pid},"`,
		`--noproxy '*' --insecure --fail`,
		`"https://127.0.0.1:${tls_port}/healthz"`,
		`"{\"ok\":true,\"version\":\"${expected_version}\"}"`,
	} {
		if !strings.Contains(health, required) {
			t.Errorf("runtime health validator missing %q", required)
		}
	}

	runtimeChecks := script[requiredIndex(t, script, "# 运行时健康、版本与监听归属检测"):]
	healthCall := requiredIndex(t, runtimeChecks, `validate_running_panel_health "$INSTALLER_RELEASE_VERSION"`)
	if got := strings.Count(runtimeChecks, `validate_running_panel_health "$INSTALLER_RELEASE_VERSION"`); got != 2 {
		t.Fatalf("runtime health gate calls=%d, want 2 (inactive repair and active/fresh)", got)
	}
	repairCommit := requiredIndex(t, runtimeChecks, `REPAIR_COMMITTED=true`)
	if healthCall >= repairCommit {
		t.Fatalf("runtime health gate offset=%d must precede repair commit offset=%d", healthCall, repairCommit)
	}
	for _, required := range []string{
		`if $REPAIR_MODE && ! $REPAIR_SERVICE_WAS_ACTIVE; then`,
		`systemctl start yub-wpanel || log_error "repair后面板临时启动失败"`,
		`systemctl stop yub-wpanel || log_error "repair健康验证后无法恢复原inactive状态"`,
		`REPAIR_INACTIVE_HEALTH_VERIFIED=true`,
		`elif ! $REPAIR_INACTIVE_HEALTH_VERIFIED; then`,
	} {
		requiredIndex(t, runtimeChecks, required)
	}
}

func TestFreshPanelEnablesOnlyAfterHealthAndCleansUpEveryFailure(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	panelStart := requiredIndex(t, script, `systemctl_start_required yub-wpanel`)
	panelEnable := requiredIndex(t, script, `systemctl enable yub-wpanel || log_error`)
	healthBeforeEnable := strings.LastIndex(script[:panelEnable], `validate_running_panel_health "$INSTALLER_RELEASE_VERSION"`)
	if healthBeforeEnable < 0 || !(panelStart < healthBeforeEnable && healthBeforeEnable < panelEnable) {
		t.Fatalf("fresh service order invalid: start=%d health=%d enable=%d", panelStart, healthBeforeEnable, panelEnable)
	}
	if strings.Contains(script, `systemctl_enable_best_effort yub-wpanel`) {
		t.Fatal("fresh panel must not be enabled before or independently of its health gate")
	}

	cleanupArmed := requiredIndex(t, script, `FRESH_SERVICE_CLEANUP_REQUIRED=true`)
	unitTargetCheck := requiredIndex(t, script, `log_error "写入systemd unit前目标已存在或为链接"`)
	unitWrite := requiredIndex(t, script, `write_panel_service_unit "$SERVICE_PATH" 0644`)
	if !(unitTargetCheck < unitWrite && unitWrite < cleanupArmed && cleanupArmed < panelStart) {
		t.Fatalf("fresh failure cleanup must be armed immediately after the panel unit write: target_check=%d unit_write=%d armed=%d start=%d", unitTargetCheck, unitWrite, cleanupArmed, panelStart)
	}
	exitHandler := extractShellFunction(t, script, "installer_exit", "systemctl_enable_best_effort")
	if requiredIndex(t, exitHandler, "repair_rollback") >= requiredIndex(t, exitHandler, "cleanup_failed_fresh_panel_service") {
		t.Fatal("fresh cleanup must run from the installer failure trap")
	}

	cleanup := extractShellFunction(t, script, "cleanup_failed_fresh_panel_service", "installer_exit")
	logPath := filepath.Join(t.TempDir(), "systemctl.log")
	shell := fmt.Sprintf(`set -eu
REPAIR_MODE=false
FRESH_SERVICE_CLEANUP_REQUIRED=true
SYSTEMCTL_LOG=%s
systemctl() { printf '%%s\n' "$*" >> "$SYSTEMCTL_LOG"; }
%s
cleanup_failed_fresh_panel_service
`, strconv.Quote(logPath), cleanup)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh failure cleanup fixture failed: %v\n%s", err, output)
	}
	assertTestFile(t, logPath, "stop yub-wpanel\ndisable yub-wpanel\n")
}

func TestRuntimeHealthValidatorChecksVersionPIDListenerAndHealthResponse(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc semantics")
	}
	script := readInstallScript(t, installScriptPath)
	health := extractShellFunction(t, script, "validate_running_panel_health", "validate_existing_panel_cron_file")
	dir := t.TempDir()
	binary := filepath.Join(dir, "yub-wpanel")
	mustWriteTestFile(t, binary, "#!/bin/sh\nprintf '\u7248\u672c: v2.0.2\\nHTTPS \u7aef\u53e3: 9443\\n'\n", 0755)
	shell := fmt.Sprintf(`set -euo pipefail
BIN_PATH=%s
CONFIG_FILE=%s
TEST_PID=$$
LISTENER_PID=$TEST_PID
systemctl() {
    case "$1" in
        is-active) return 0 ;;
        show) printf '%%s\n' "$TEST_PID" ;;
        *) return 1 ;;
    esac
}
timeout() { shift; "$@"; }
readlink() { printf '%%s\n' "$BIN_PATH"; }
ss() { printf 'LISTEN 0 4096 0.0.0.0:9443 0.0.0.0:* users:(("yub-wpanel",pid=%%s,fd=7))\n' "$LISTENER_PID"; }
curl() { printf '{"ok":true,"version":"v2.0.2"}'; }
sleep() { :; }
%s
validate_running_panel_health v2.0.2
LISTENER_PID=$((TEST_PID + 1))
if validate_running_panel_health v2.0.2; then exit 91; fi
if validate_running_panel_health v2.0.3; then exit 92; fi
`, strconv.Quote(binary), strconv.Quote(filepath.Join(dir, "config.json")), health)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime health fixture failed: %v\n%s", err, output)
	}
}

func TestInstallerNeverPlacesGeneratedSecretsInChildArguments(t *testing.T) {
	script := readInstallScript(t, installScriptPath)
	for _, forbidden := range []string{
		`-p"${MYSQL_PASS}"`,
		`mysqladmin -u root password`,
		`password_hash('$BASIC_PASS'`,
		`password_hash('$WEB_PASS'`,
		`bcrypt.hashpw(b'$BASIC_PASS'`,
		`bcrypt.hashpw(b'$WEB_PASS'`,
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("installer still exposes a generated secret through child argv: %q", forbidden)
		}
	}
	for _, required := range []string{
		`MYSQL_CLIENT_CONFIG="$INSTALL_WORKDIR/mariadb-client.cnf"`,
		`chmod 0600 "$MYSQL_CLIENT_CONFIG"`,
		`mysql --defaults-extra-file="$MYSQL_CLIENT_CONFIG"`,
		`printf '%s' "$password" | php8.3 -r`,
		`stream_get_contents(STDIN)`,
		`printf '%s' "$password" | python3 -c`,
		`sys.stdin.buffer.read()`,
	} {
		requiredIndex(t, script, required)
	}
}

func TestInstallCNDoesNotDuplicateRepairImplementation(t *testing.T) {
	cnScript := readInstallScript(t, installCNScriptPath)
	for _, forbidden := range []string{
		"REPAIR_MODE",
		"repair-config-check",
		"create_repair_backup",
		"repair_rollback",
	} {
		if strings.Contains(cnScript, forbidden) {
			t.Errorf("install-cn.sh duplicates repair implementation %q", forbidden)
		}
	}
}

func extractShellFunction(t *testing.T, script, name, nextName string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	end := strings.Index(script[start:], "\n"+nextName+"() {")
	if start < 0 || end < 0 {
		t.Fatalf("cannot extract %s", name)
	}
	return script[start : start+end]
}

func runRollbackFixture(t *testing.T, rollback, root, backup, binary, unit, database, logPath string, binExisted, unitExisted, tlsExisted, dbExisted, serviceWasActive, licenseDirExisted bool, tlsAction string, dbRestoreFails bool) {
	t.Helper()
	shell := fmt.Sprintf(`set -e
REPAIR_MODE=true
REPAIR_COMMITTED=false
REPAIR_MUTATED=true
REPAIR_BACKUP_DIR=%s
REPAIR_BIN_EXISTED=%t
REPAIR_UNIT_EXISTED=%t
REPAIR_TLS_EXISTED=%t
REPAIR_DB_EXISTED=%t
REPAIR_LICENSE_DIR_EXISTED=%t
REPAIR_TLS_ACTION=%s
REPAIR_SERVICE_WAS_ACTIVE=%t
BIN_PATH=%s
SERVICE_PATH=%s
DB_PATH=%s
INSTALL_DIR=%s
LICENSE_DOC_DIR=%s
SYSTEMCTL_LOG=%s
log_warn() { :; }
DB_RESTORE_FAILS=%t
SERVICE_ACTIVE=true
atomic_install_managed_file() {
    local source_path="$1"
    local target_path="$2"
    local target_mode="$3"
    if $DB_RESTORE_FAILS && [[ "$target_path" == "$DB_PATH" ]]; then
        return 1
    fi
    cp "$source_path" "$target_path"
    chmod "$target_mode" "$target_path"
}
install() {
    cp "${@: -2:1}" "${@: -1}"
    chmod "$2" "${@: -1}"
}
systemctl() {
    if [[ "$1" == is-active ]]; then
        $SERVICE_ACTIVE
        return
    fi
    printf '%%s\n' "$*" >> "$SYSTEMCTL_LOG"
    if [[ "$1" == stop ]]; then SERVICE_ACTIVE=false; fi
    if [[ "$1" == start ]]; then SERVICE_ACTIVE=true; fi
}
%s
repair_rollback
`, strconv.Quote(backup), binExisted, unitExisted, tlsExisted, dbExisted, licenseDirExisted, strconv.Quote(tlsAction), serviceWasActive, strconv.Quote(binary), strconv.Quote(unit), strconv.Quote(database), strconv.Quote(root), strconv.Quote(filepath.Join(root, "license-root", "yub-wpanel")), strconv.Quote(logPath), dbRestoreFails, rollback)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rollback fixture failed: %v\n%s", err, output)
	}
}

func runRollbackRuntimeFailureFixture(t *testing.T, rollback, failure string) string {
	t.Helper()
	root := t.TempDir()
	backup := filepath.Join(root, "backup")
	binary := filepath.Join(root, "yub-wpanel")
	unit := filepath.Join(root, "yub-wpanel.service")
	database := filepath.Join(root, "panel.db")
	certDir := filepath.Join(root, "certs")
	logPath := filepath.Join(root, "rollback.log")
	for path, contents := range map[string]string{
		filepath.Join(backup, "yub-wpanel"):                      "old-binary",
		filepath.Join(backup, "yub-wpanel.service"):              "old-unit",
		filepath.Join(backup, "panel.db"):                        "old-database",
		filepath.Join(backup, "panel.crt"):                       "old-cert",
		filepath.Join(backup, "panel.key"):                       "old-key",
		filepath.Join(backup, "license-docs", "RELEASE_VERSION"): "v2.0.1\n",
		binary:                              "new-binary",
		unit:                                "new-unit",
		database:                            "new-database",
		filepath.Join(certDir, "panel.crt"): "new-cert",
		filepath.Join(certDir, "panel.key"): "new-key",
		filepath.Join(root, "license-root", "yub-wpanel", "RELEASE_VERSION"): "v2.0.2\n",
	} {
		mustWriteTestFile(t, path, contents, 0600)
	}
	if failure == "missing-backup" {
		if err := os.RemoveAll(backup); err != nil {
			t.Fatal(err)
		}
	}

	shell := fmt.Sprintf(`set -u
REPAIR_MODE=true
REPAIR_COMMITTED=false
REPAIR_MUTATED=true
REPAIR_BACKUP_DIR=%s
REPAIR_BIN_EXISTED=true
REPAIR_UNIT_EXISTED=true
REPAIR_TLS_EXISTED=true
REPAIR_DB_EXISTED=true
REPAIR_LICENSE_DIR_EXISTED=true
REPAIR_TLS_ACTION=preserve
REPAIR_SERVICE_WAS_ACTIVE=true
REPAIR_SERVICE_STOPPED_FOR_SNAPSHOT=true
BIN_PATH=%s
SERVICE_PATH=%s
DB_PATH=%s
INSTALL_DIR=%s
LICENSE_DOC_DIR=%s
ROLLBACK_LOG=%s
FAILURE=%s
SERVICE_ACTIVE=true
log_warn() { printf 'WARN:%%s\n' "$1" >> "$ROLLBACK_LOG"; }
cp() {
    local arg=""
    if [[ "$FAILURE" == license ]]; then
        for arg in "$@"; do
            if [[ "$arg" == "$REPAIR_BACKUP_DIR/license-docs/." ]]; then
                return 1
            fi
        done
    fi
    command cp "$@"
}
atomic_install_managed_file() {
	if [[ "$FAILURE" == database && "$2" == "$DB_PATH" ]]; then
		return 1
	fi
	if [[ "$FAILURE" == binary && "$2" == "$BIN_PATH" ]]; then
		return 1
	fi
    cp "$1" "$2"
    chmod "$3" "$2"
}
install() {
    local target="${@: -1}"
    if [[ "$FAILURE" == unit && "$target" == "$SERVICE_PATH" ]]; then
        return 1
    fi
    if [[ "$FAILURE" == tls && "$target" == "$INSTALL_DIR/certs/panel.crt" ]]; then
        return 1
    fi
    cp "${@: -2:1}" "$target"
    chmod "$2" "$target"
}
systemctl() {
	if [[ "$1" == is-active ]]; then
		$SERVICE_ACTIVE
		return
	fi
    printf '%%s\n' "$*" >> "$ROLLBACK_LOG"
	if [[ "$1" == stop ]]; then
		if [[ "$FAILURE" != stop-still-active ]]; then
			SERVICE_ACTIVE=false
		fi
		return 0
	fi
    if [[ "$FAILURE" == daemon-reload && "$1" == daemon-reload ]]; then
        return 1
    fi
    if [[ "$FAILURE" == start && "$1" == start ]]; then
        return 1
    fi
	if [[ "$1" == start ]]; then
		SERVICE_ACTIVE=true
	fi
    return 0
}
%s
repair_rollback
`, strconv.Quote(backup), strconv.Quote(binary), strconv.Quote(unit), strconv.Quote(database), strconv.Quote(root), strconv.Quote(filepath.Join(root, "license-root", "yub-wpanel")), strconv.Quote(logPath), strconv.Quote(failure), rollback)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rollback runtime failure fixture %q failed: %v\n%s", failure, err, output)
	}
	return logPath
}

func mustWriteTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
