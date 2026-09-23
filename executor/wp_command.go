package executor

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const panelCommandScript = `#!/bin/bash
# YUB WPanel CLI — b

BIN=/usr/local/bin/yub-wpanel
CFG=/www/server/panel/config.json
SVC=yub-wpanel

red()  { echo -e "\033[31m$*\033[0m"; }
green(){ echo -e "\033[32m$*\033[0m"; }
dim()  { echo -e "\033[2m$*\033[0m"; }

diag() {
    local issues=0

    # 1. 二进制
    if [ -x "$BIN" ]; then
        green "✓ 二进制: $BIN"
    elif [ -f "$BIN" ]; then
        red "✗ 二进制无执行权限: $BIN"
        echo "   → 修复: chmod +x $BIN"
        issues=$((issues+1))
    else
        red "✗ 二进制不存在: $BIN"
        echo "   → 面板可能未安装或安装不完整，请重新运行 install.sh"
        issues=$((issues+1))
        return $issues
    fi

    # 2. 配置文件
    if [ -f "$CFG" ]; then
        if python3 -c "import json; json.load(open('$CFG'))" 2>/dev/null; then
            green "✓ 配置文件: $CFG"
        else
            red "✗ 配置文件 JSON 格式错误: $CFG"
            echo "   → 修复: 检查文件内容或从备份恢复"
            issues=$((issues+1))
        fi
    else
        red "✗ 配置文件不存在: $CFG"
        echo "   → 面板可能未安装，请重新运行 install.sh"
        issues=$((issues+1))
        return $issues
    fi

    # 3. 数据库
    DB=$(python3 -c "import json; d=json.load(open('$CFG')); print(d.get('sqlite',{}).get('path',''))" 2>/dev/null)
    if [ -n "$DB" ] && [ -f "$DB" ]; then
        green "✓ 数据库: $DB"
    elif [ -n "$DB" ]; then
        red "✗ 数据库文件不存在: $DB"
        echo "   → 数据库文件丢失，检查磁盘空间或从备份恢复"
        issues=$((issues+1))
    else
        dim "? 未能读取数据库路径"
    fi

    # 4. systemd 服务文件
    if [ -f "/etc/systemd/system/${SVC}.service" ]; then
        green "✓ systemd 服务文件: /etc/systemd/system/${SVC}.service"
    else
        red "✗ systemd 服务文件缺失"
        echo "   → 修复: 重新运行 install.sh"
        issues=$((issues+1))
        return $issues
    fi

    # 5. 端口
    PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel'].get('tls_port', d['panel']['port']))" 2>/dev/null)
    if [ -n "$PORT" ]; then
        if ss -tlnp 2>/dev/null | grep -q ":${PORT} "; then
            green "✓ 端口 ${PORT} 已监听"
        else
            dim "? 端口 ${PORT} 未监听（面板未在运行）"
        fi
    fi

    # 6. systemd 状态
    if systemctl is-active --quiet "$SVC"; then
        green "✓ 服务状态: 运行中"
    else
        red "✗ 服务状态: 未运行"
        issues=$((issues+1))
        echo ""
        echo "── 最近的错误日志 ──"
        journalctl -u "$SVC" -n 20 --no-pager --lines=6 2>/dev/null | tail -20
        echo "── 日志结束 ──"
        echo ""
        echo "→ 查看完整日志: journalctl -u $SVC -n 50 --no-pager"
    fi

    if [ $issues -eq 0 ]; then
        echo ""
        green "所有检查通过"
    else
        echo ""
        red "发现 ${issues} 个问题"
    fi
}

case "${1:-}" in
    restart)
        echo "正在重启面板..."
        if systemctl restart "$SVC" 2>/dev/null; then
            sleep 2
            if systemctl is-active --quiet "$SVC"; then
                green "YUB WPanel 已重启，运行中"
            else
                red "YUB WPanel 重启后未能启动"
                echo ""
                echo "── 最近日志 ──"
                journalctl -u "$SVC" -n 20 --no-pager 2>/dev/null | tail -20
                echo "── 结束 ──"
                echo ""
                echo "→ 运行 'b status' 进行完整诊断"
            fi
        else
            red "systemctl restart 失败，服务可能未安装"
            echo "→ 运行 'b status' 进行诊断"
        fi
        ;;
    password)
        $BIN --reset-admin
        ;;
    info)
        $BIN --info
        ;;
    unban)
        $BIN --unban-all
        ;;
    status|check)
        echo "YUB WPanel 诊断检查"
        echo "=================="
        echo ""
        diag
        ;;
    log)
        journalctl -u "$SVC" -n "${2:-30}" --no-pager 2>/dev/null
        ;;
    *)
        $BIN --info 2>/dev/null
        echo ""
        if [ -f "$CFG" ]; then
            PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel']['port'])" 2>/dev/null)
            SUFFIX=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel']['random_suffix'])" 2>/dev/null)
            IP=$(hostname -I 2>/dev/null | awk '{print $1}')
            TLS_PORT=$(python3 -c "import json; d=json.load(open('$CFG')); print(d['panel'].get('tls_port', d['panel']['port']))" 2>/dev/null)
            [ -n "$TLS_PORT" ] && [ -n "$SUFFIX" ] && [ -n "$IP" ] && echo "面板地址: https://$IP:$TLS_PORT/$SUFFIX"
        fi
        if systemctl is-active --quiet "$SVC"; then
            green "运行状态: 运行中"
        else
            red "运行状态: 未运行"
            echo ""
            echo "── 自动诊断 ──"
            diag
        fi
        echo ""
        echo "用法: b <命令>（也可使用大写 B）"
        echo "  b restart     重启面板"
        echo "  b status      完整诊断检查"
        echo "  b log [N]     查看最近 N 条日志（默认30）"
        echo "  b password    一键重置管理员账号密码"
        echo "  b unban       一键清空所有IP封禁"
        ;;
esac
`

const (
	panelCommandMarker      = "# YUB WPanel CLI — b"
	legacyYUBWCommandMarker = "# YUB WPanel CLI — yubw"
	legacyWPCommandMarker   = "# YUB WPanel CLI — wp"
	managedMarkerScanLines  = 5
)

// legacyWPCommandMarker is the exact second line of the pre-panel-CLI-rename
// script (see git history of this file). It's used to recognize a leftover
// /usr/local/bin/wp created by an older version of this panel, as opposed
// to a real WP-CLI install that happens to live at the same path. Matching
// requires an exact, line-anchored match — not a substring — so it can't be
// tripped by unrelated text elsewhere in a user's own script.

// EnsurePanelCommands installs the short lowercase and uppercase CLI entry
// points. Because these are generic one-character names, existing paths are
// replaced only when they already carry YUB WPanel's exact ownership marker.
func EnsurePanelCommands() {
	paths := []string{"/usr/local/bin/b", "/usr/local/bin/B"}
	if err := migratePanelCommandsAt(paths, "/usr/local/bin/yubw"); err != nil {
		log.Printf("面板 b/B 命令迁移失败: %v", err)
		return
	}
	removeLegacyWPCommand()
}

func migratePanelCommandsAt(paths []string, legacyPath string) error {
	if err := ensurePanelCommandsAt(paths...); err != nil {
		return err
	}
	if err := removeManagedCommandAt(legacyPath, legacyYUBWCommandMarker); err != nil {
		return fmt.Errorf("remove managed legacy command %s: %w", legacyPath, err)
	}
	return nil
}

func ensurePanelCommandsAt(paths ...string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no panel command paths supplied")
	}
	for _, path := range paths {
		replaceable, err := managedCommandPathReplaceable(path, panelCommandMarker)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if !replaceable {
			return fmt.Errorf("refusing to replace non-YUB command at %s", path)
		}
	}
	for _, path := range paths {
		if err := writeFileAtomic(path, []byte(panelCommandScript), 0755); err != nil {
			return fmt.Errorf("install %s: %w", path, err)
		}
	}
	return nil
}

func managedCommandPathReplaceable(path, marker string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	return fileHasExactMarker(path, marker)
}

// removeLegacyWPCommand cleans up the old /usr/local/bin/wp shortcut from
// versions prior to the dedicated panel command, so it stops shadowing WP-CLI
// install. It only removes the file if one of its first few lines is an
// exact match for legacyWPCommandMarker; a user-installed WP-CLI (or any
// other file) at the same path is left untouched.
func removeLegacyWPCommand() {
	if err := removeManagedCommandAt("/usr/local/bin/wp", legacyWPCommandMarker); err != nil {
		log.Printf("清理旧版 wp 命令失败 (/usr/local/bin/wp): %v", err)
	}
}

// removeLegacyWPCommandAt implements removeLegacyWPCommand against an
// explicit path so it can be exercised against a temp file in tests.
func removeLegacyWPCommandAt(legacyPath string) {
	if err := removeManagedCommandAt(legacyPath, legacyWPCommandMarker); err != nil {
		log.Printf("清理旧版 wp 命令失败 (%s): %v", legacyPath, err)
	}
}

func removeManagedCommandAt(path, marker string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	matched, err := fileHasExactMarker(path, marker)
	if err != nil {
		return err
	}
	if !matched {
		return nil
	}
	return os.Remove(path)
}

// fileHasExactMarker only scans the first few lines, so an unrelated file is
// never read in full merely because it occupies a managed command path.
func fileHasExactMarker(path, marker string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for i := 0; i < managedMarkerScanLines && scanner.Scan(); i++ {
		if scanner.Text() == marker {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// writeFileAtomic writes data to path via a temp file + rename in the same
// directory, so a concurrent reader (e.g. someone running b mid-upgrade)
// never observes a partially-written script.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".yub-wpanel-cli-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
