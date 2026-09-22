package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		`atomic_install_managed_file "$PANEL_CANDIDATE" "$BIN_PATH" 0755`,
		`mktemp "${target_dir}/.${target_base}.yub-install.XXXXXXXX"`,
		`sync -f "$ATOMIC_STAGE_PATH"`,
		`sync -f "$target_dir"`,
	} {
		requiredIndex(t, script, required)
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
	backupAfterInstall := strings.Index(script[sqliteInstall:], "\n    create_repair_backup")
	if backupAfterInstall < 0 {
		t.Fatal("repair sqlite dependency install must be followed by create_repair_backup call")
	}
	backup := sqliteInstall + backupAfterInstall
	if !(configCheck < binaryCheck && binaryCheck < unitCheck && unitCheck < cronCheck && cronCheck < sqliteInstall && sqliteInstall < backup) {
		t.Fatalf("repair safety order is invalid: config_check=%d binary_check=%d unit_check=%d cron_check=%d sqlite_install=%d backup=%d", configCheck, binaryCheck, unitCheck, cronCheck, sqliteInstall, backup)
	}

	componentInstall := requiredIndex(t, script, "apt-get install -y \\\n    nginx \\")
	componentComplete := requiredIndex(t, script, `log_info "基础组件安装完成"`)
	if strings.Contains(script[componentInstall:componentComplete], "sqlite3") {
		t.Fatal("fresh install must not install repair-only sqlite3 dependency")
	}
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

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, true, true, true, true, "preserve", false)
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
		logPath := filepath.Join(root, "systemctl.log")

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, false, false, false, false, "generate", false)
		for _, path := range []string{binary, unit, database, database + "-wal", filepath.Join(root, "certs", "panel.crt"), filepath.Join(root, "certs", "panel.key")} {
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

		runRollbackFixture(t, rollback, root, backup, binary, unit, database, logPath, true, false, false, true, "preserve", true)
		assertTestFile(t, binary, "old-binary")
		assertTestFile(t, database, "new-database")
		assertTestFile(t, logPath, "stop yub-wpanel\ndaemon-reload\nstop yub-wpanel\n")
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

func runRollbackFixture(t *testing.T, rollback, root, backup, binary, unit, database, logPath string, binExisted, unitExisted, tlsExisted, dbExisted bool, tlsAction string, dbRestoreFails bool) {
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
REPAIR_TLS_ACTION=%s
REPAIR_SERVICE_WAS_ACTIVE=%t
BIN_PATH=%s
SERVICE_PATH=%s
DB_PATH=%s
INSTALL_DIR=%s
SYSTEMCTL_LOG=%s
log_warn() { :; }
DB_RESTORE_FAILS=%t
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
systemctl() { printf '%%s\n' "$*" >> "$SYSTEMCTL_LOG"; }
%s
repair_rollback
`, strconv.Quote(backup), binExisted, unitExisted, tlsExisted, dbExisted, strconv.Quote(tlsAction), binExisted, strconv.Quote(binary), strconv.Quote(unit), strconv.Quote(database), strconv.Quote(root), strconv.Quote(logPath), dbRestoreFails, rollback)
	cmd := exec.Command("bash", "-c", shell)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rollback fixture failed: %v\n%s", err, output)
	}
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
