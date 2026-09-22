#!/bin/bash
set -e
set -o pipefail

# ============================================================
# YUB WPanel 安装脚本 — 适用于 Debian 13 (Trixie)，建议使用纯净系统
# 自动为 PHP 8.3 源选择官方源或国内镜像源，兼容海外和国内 VPS
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

INSTALL_DIR="/www/server/panel"
CONFIG_FILE="$INSTALL_DIR/config.json"
DB_PATH="$INSTALL_DIR/panel.db"
BIN_PATH="/usr/local/bin/yub-wpanel"
SERVICE_PATH="/etc/systemd/system/yub-wpanel.service"
CRON_PATH="/etc/cron.d/yub_wpanel_cron"
LICENSE_DOC_DIR="/usr/share/doc/yub-wpanel"
PANEL_PORT=8888
MYSQL_PASS=""
GHPROXY="${YUB_WPANEL_GITHUB_PROXY:-}"
PREFER_CN=false
PHP_SOURCE_MODE="${YUB_WPANEL_PHP_SOURCE:-auto}"
REPAIR_MODE=false
REPAIR_BACKUP_DIR=""
REPAIR_SERVICE_WAS_ACTIVE=false
REPAIR_COMMITTED=false
INSTALL_WORKDIR=""
PANEL_CANDIDATE=""
PANEL_SHA256_FILE=""
PANEL_SIGNATURE_FILE=""
LICENSE_ARCHIVE=""
LICENSE_SHA256_FILE=""
LICENSE_SIGNATURE_FILE=""
PROJECT_LICENSE_FILE=""
PROJECT_NOTICE_FILE=""
THIRD_PARTY_NOTICE_FILE=""
LICENSE_RELEASE_VERSION_FILE=""
PANEL_CANDIDATE_VERIFIED=false
REPAIR_BIN_EXISTED=false
REPAIR_UNIT_EXISTED=false
REPAIR_TLS_EXISTED=false
REPAIR_DB_EXISTED=false
REPAIR_MUTATED=false
ATOMIC_STAGE_PATH=""
# The signed bootstrap validates this marker before it delegates execution.
# shellcheck disable=SC2034
RELEASE_PUBLIC_KEY_HEX="7351099720eeaf147f4894bc313a5456c01bbd29ad7d401ab6869bc7f7f92af5"
INSTALLER_RELEASE_VERSION="__YUB_WPANEL_RELEASE_VERSION__"
MIN_PANEL_VERSION="v2.0.1"
DEBSURY_KEYRING_PACKAGE="debsuryorg-archive-keyring"
DEBSURY_KEYRING_VERSION="2025.11.18"
DEBSURY_KEYRING_SHA256="7511384559c9ddf1d5ce5f60be429ae9d4e7d01d9480d6f1b7a30c0810cf8b60"
PANEL_ASSET_MAX_BYTES=$((256 * 1024 * 1024))
CHECKSUM_ASSET_MAX_BYTES=$((4 * 1024))
SIGNATURE_ASSET_MAX_BYTES=64
LICENSE_ARCHIVE_MAX_BYTES=$((64 * 1024 * 1024))
PHP_KEYRING_MAX_BYTES=$((1 * 1024 * 1024))
WORDPRESS_ZIP_MAX_BYTES=$((256 * 1024 * 1024))
PUBLIC_IP_MAX_BYTES=$((4 * 1024))
# SubjectPublicKeyInfo PEM derived from RELEASE_PUBLIC_KEY_HEX. Keeping the raw
# key above makes key rotation and cross-checking with the application explicit.
RELEASE_PUBLIC_KEY_PEM='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=
-----END PUBLIC KEY-----'

if [[ "${YUB_WPANEL_PREFER_CN_MIRROR:-0}" == "1" ]] || [[ "${YUB_WPANEL_PREFER_CN_MIRROR:-}" == "true" ]]; then
    PREFER_CN=true
fi

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

assert_supported_platform() {
    local os_id=""
    local version_id=""
    local machine=""
    local dpkg_arch=""
    local platform_cmd=""

    for platform_cmd in dpkg head sed tr uname; do
        command -v "$platform_cmd" >/dev/null 2>&1 || log_error "缺少平台检测命令: ${platform_cmd}"
    done
    [[ -r /etc/os-release ]] || log_error "无法读取 /etc/os-release；仅支持 Debian 13 amd64"
    os_id=$(sed -n 's/^ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    version_id=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    machine=$(uname -m 2>/dev/null || true)
    dpkg_arch=$(dpkg --print-architecture 2>/dev/null || true)

    [[ "$os_id" == "debian" ]] || log_error "此脚本仅支持 Debian 13，当前系统: ${os_id:-unknown}"
    [[ "$version_id" == "13" ]] || log_error "此脚本仅支持 Debian 13，当前版本: ${version_id:-unknown}"
    case "$machine" in
        x86_64|amd64) ;;
        *) log_error "当前只发布 amd64/x86_64 二进制，检测到架构: ${machine:-unknown}" ;;
    esac
    [[ "$dpkg_arch" == "amd64" ]] || \
        log_error "当前只支持 Debian amd64 用户空间，检测到 dpkg 架构: ${dpkg_arch:-unknown}"
}

init_install_workdir() {
    local required_cmd=""
    local previous_umask=""

    for required_cmd in awk chmod cmp dpkg-deb find flock grep head install mktemp mv openssl readlink rm sed sha256sum stat sync systemctl systemd-analyze tar timeout tr uname wc; do
        command -v "$required_cmd" >/dev/null 2>&1 || log_error "缺少安装安全预检命令: ${required_cmd}"
    done
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        log_error "缺少下载工具：请先安装 curl 或 wget"
    fi

    previous_umask=$(umask)
    umask 077
    INSTALL_WORKDIR=$(mktemp -d /tmp/yub-wpanel-install.XXXXXXXXXX) || \
        log_error "无法创建安装临时工作目录"
    chmod 0700 "$INSTALL_WORKDIR"
    umask "$previous_umask"
    PANEL_CANDIDATE="$INSTALL_WORKDIR/yub-wpanel"
    PANEL_SHA256_FILE="$INSTALL_WORKDIR/yub-wpanel.sha256"
    PANEL_SIGNATURE_FILE="$INSTALL_WORKDIR/yub-wpanel.sha256.sig"
    LICENSE_ARCHIVE="$INSTALL_WORKDIR/yub-wpanel-third-party-licenses.tar.gz"
    LICENSE_SHA256_FILE="$INSTALL_WORKDIR/yub-wpanel-third-party-licenses.tar.gz.sha256"
    LICENSE_SIGNATURE_FILE="$INSTALL_WORKDIR/yub-wpanel-third-party-licenses.tar.gz.sha256.sig"
    PROJECT_LICENSE_FILE="$INSTALL_WORKDIR/LICENSE"
    PROJECT_NOTICE_FILE="$INSTALL_WORKDIR/NOTICE.md"
    THIRD_PARTY_NOTICE_FILE="$INSTALL_WORKDIR/THIRD_PARTY_NOTICES.md"
    LICENSE_RELEASE_VERSION_FILE="$INSTALL_WORKDIR/RELEASE_VERSION"
}

cleanup_install_workdir() {
    [[ -n "$INSTALL_WORKDIR" ]] || return 0
    case "$INSTALL_WORKDIR" in
        /tmp/yub-wpanel-install.??????????)
            [[ -d "$INSTALL_WORKDIR" ]] && rm -rf -- "$INSTALL_WORKDIR"
            ;;
        *)
            log_warn "拒绝清理异常临时目录路径: $INSTALL_WORKDIR"
            ;;
    esac
}

cleanup_atomic_stage_file() {
	[[ -n "$ATOMIC_STAGE_PATH" ]] || return 0
	case "$ATOMIC_STAGE_PATH" in
		/usr/local/bin/.yub-wpanel.yub-install.????????|/www/server/panel/.panel.db.yub-install.????????)
			rm -f -- "$ATOMIC_STAGE_PATH"
			;;
		*)
			log_warn "拒绝清理异常原子暂存文件路径: $ATOMIC_STAGE_PATH"
			;;
	esac
	ATOMIC_STAGE_PATH=""
}

installer_exit() {
    local exit_code=$?
    set +e
    if [[ $exit_code -ne 0 ]]; then
        repair_rollback
    fi
	cleanup_atomic_stage_file
    cleanup_install_workdir
    if [[ $exit_code -ne 0 ]]; then
        echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "${RED}  安装未完成 / Installation incomplete${NC}"
        echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
        echo -e "  请先保存本次终端完整输出，不要在未定位原因前直接重装系统。"
        echo -e "  优先检查：网络/DNS 与系统时间、APT 错误、发行包哈希/签名、Debian 13 amd64 平台检测，以及现有服务冲突。"
        echo -e "  可结合 ${BOLD}journalctl -u yub-wpanel -n 100 --no-pager${NC} 和 APT 输出排查，再携带已脱敏日志提交 GitHub Issue。"
        echo -e "  Save the complete terminal output first. Check networking/DNS, system time, APT, release hash/signature, platform validation, and existing service conflicts before considering an OS reinstall."
        echo ""
        echo -e "  GitHub: https://github.com/zangwp/yub-wpanel/issues"
        echo ""
    fi
    trap - EXIT
    exit "$exit_code"
}

systemctl_enable_best_effort() {
    local svc="$1"
    if ! systemctl enable "$svc"; then
        log_warn "${svc} 开机自启设置失败，继续安装。安装后可手动检查: systemctl enable ${svc}"
    fi
}

systemctl_start_required() {
    local svc="$1"
    if ! systemctl start "$svc"; then
        journalctl -u "$svc" -n 20 --no-pager 2>/dev/null || true
        log_error "${svc} 启动失败，请根据上方日志排查"
    fi
}

# ============================================================
# 系统内核优化（BBR+FQ、TCP 缓冲、连接队列、文件描述符）
# ============================================================
apply_system_tuning() {
    log_info "应用系统内核优化..."

    SYSCTL_FILE="/etc/sysctl.d/99-yub-wpanel.conf"
    CPU_CORES=$(nproc)

    cat > "$SYSCTL_FILE" << 'SYSCTLEOF'
# YUB WPanel — 网络与内核优化

# ── 连接队列 ──
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 8192
net.core.netdev_max_backlog = 16384

# ── TCP 缓冲区 ──
net.core.rmem_default = 262144
net.core.wmem_default = 262144
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216

# ── TIME-WAIT 优化 ──
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15
net.ipv4.ip_local_port_range = 1024 65535

# ── Keepalive ──
net.ipv4.tcp_keepalive_time = 300
net.ipv4.tcp_keepalive_intvl = 30
net.ipv4.tcp_keepalive_probes = 5

# ── BBR 辅助参数 ──
net.ipv4.tcp_slow_start_after_idle = 0
net.ipv4.tcp_notsent_lowat = 16384

# ── 基础安全 ──
net.ipv4.tcp_syncookies = 1
net.ipv4.tcp_sack = 1
net.ipv4.tcp_timestamps = 1
SYSCTLEOF

    # BBR + FQ: 仅 2 核及以上机器开启（单核 VPS CPU 争抢时 BBR 吞吐量会暴跌）
    if [[ $CPU_CORES -ge 2 ]]; then
        cat >> "$SYSCTL_FILE" << 'BBREOF'

# ── BBR 拥塞控制 + FQ 调度 ──
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
BBREOF
        modprobe tcp_bbr 2>/dev/null || true
        log_info "BBR + FQ 已启用（${CPU_CORES} 核 CPU）"
    else
        log_info "单核 CPU，跳过 BBR（避免 CPU 争抢副作用）"
    fi

    sysctl --system >/dev/null 2>&1

    # 文件描述符限制
    if ! grep -q "nofile 65535" /etc/security/limits.conf 2>/dev/null; then
        cat >> /etc/security/limits.conf << 'LIMITSEOF'
* soft nofile 65535
* hard nofile 65535
LIMITSEOF
    fi

    log_info "系统内核优化完成"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --prefer-cn|--cn)
            PREFER_CN=true
            shift
            ;;
        --php-source)
            if [[ $# -lt 2 ]]; then
                log_error "--php-source 需要指定 official、ustc、sjtu 或 auto"
            fi
            PHP_SOURCE_MODE="$2"
            shift 2
            ;;
        --php-source=*)
            PHP_SOURCE_MODE="${1#*=}"
            shift
            ;;
        *)
            log_warn "未知参数已忽略: $1"
            shift
            ;;
    esac
done

# 异常退出时回滚 repair，并只清理由 mktemp 创建的本次工作目录。
trap installer_exit EXIT

# ============================================================
# PHP 8.3 源选择（官方源 + 国内镜像多重兜底）
# ============================================================

set_php_source_meta() {
    case "$1" in
        official)
            PHP_SOURCE_LABEL="Ondřej Surý 官方源"
            PHP_KEY_URL="https://packages.sury.org/debsuryorg-archive-keyring.deb"
            PHP_REPO_URL="https://packages.sury.org/php/"
            ;;
        ustc)
            PHP_SOURCE_LABEL="中科大 PHP Sury 镜像"
            PHP_KEY_URL="https://mirrors.ustc.edu.cn/sury/debsuryorg-archive-keyring.deb"
            PHP_REPO_URL="https://mirrors.ustc.edu.cn/sury/php/"
            ;;
        sjtu)
            PHP_SOURCE_LABEL="上海交大 PHP Sury 镜像"
            PHP_KEY_URL="https://mirror.sjtu.edu.cn/sury/debsuryorg-archive-keyring.deb"
            PHP_REPO_URL="https://mirror.sjtu.edu.cn/sury/php/"
            ;;
        *)
            return 1
            ;;
    esac
}

file_size_within_limit() {
    local path="$1"
    local max_bytes="$2"
    local actual_bytes=""

    [[ "$max_bytes" =~ ^[1-9][0-9]*$ ]] || return 1
    [[ -f "$path" ]] && [[ ! -L "$path" ]] || return 1
    actual_bytes=$(stat -c '%s' -- "$path" 2>/dev/null) || return 1
    [[ "$actual_bytes" =~ ^[0-9]+$ ]] || return 1
    (( actual_bytes > 0 && actual_bytes <= max_bytes ))
}

download_file() {
    local url="$1"
    local output="$2"
    local total_timeout="${3:-120}"
    local max_bytes="${4:-0}"
    local connect_timeout=15

    [[ "$max_bytes" =~ ^[1-9][0-9]*$ ]] || return 1
    if [[ "$total_timeout" -lt "$connect_timeout" ]]; then
        connect_timeout="$total_timeout"
    fi

    rm -f "$output"
    if command -v curl &>/dev/null; then
        if timeout "${total_timeout}s" curl -q -fsSL \
            --proto '=https' \
            --proto-redir '=https' \
            --connect-timeout "$connect_timeout" \
            --max-time "$total_timeout" \
            --max-filesize "$max_bytes" \
            --speed-limit 1024 \
            --speed-time 30 \
            --retry 3 \
            --retry-delay 2 \
            --retry-all-errors \
            "$url" 2>/dev/null | head -c "$((max_bytes + 1))" > "$output"; then
            file_size_within_limit "$output" "$max_bytes" && return 0
        fi
        rm -f "$output"
    fi
    if command -v wget &>/dev/null; then
        if timeout "${total_timeout}s" wget --no-config -q --https-only --no-hsts \
            --connect-timeout="$connect_timeout" \
            --read-timeout=30 \
            --tries=3 \
            --retry-connrefused \
            --waitretry=2 \
            -O - "$url" 2>/dev/null | head -c "$((max_bytes + 1))" > "$output"; then
            file_size_within_limit "$output" "$max_bytes" && return 0
        fi
        rm -f "$output"
    fi
    rm -f "$output"
    return 1
}

verify_signed_release_asset() {
    local asset_file="$1"
    local sha_file="$2"
    local sig_file="$3"
    local expected_name="$4"
    local asset_max_bytes="$5"
    local public_key_file="$INSTALL_WORKDIR/release-public-key.pem"
    local expected_sha=""
    local signed_name=""
    local extra_field=""
    local actual_sha=""
    local nonempty_lines=""

    file_size_within_limit "$asset_file" "$asset_max_bytes" || return 1
    file_size_within_limit "$sha_file" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$sig_file" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    [[ "$(wc -c < "$sig_file" | tr -d '[:space:]')" == "64" ]] || return 1

    printf '%s\n' "$RELEASE_PUBLIC_KEY_PEM" > "$public_key_file"
    chmod 0600 "$public_key_file"
    openssl pkeyutl -verify -pubin -inkey "$public_key_file" -rawin \
        -in "$sha_file" -sigfile "$sig_file" >/dev/null 2>&1 || return 1

    nonempty_lines=$(awk 'NF {count++} END {print count+0}' "$sha_file")
    [[ "$nonempty_lines" == "1" ]] || return 1
    read -r expected_sha signed_name extra_field < "$sha_file" || return 1
    [[ -z "$extra_field" ]] || return 1
    [[ "$expected_sha" =~ ^[0-9a-fA-F]{64}$ ]] || return 1
    [[ "$signed_name" == "$expected_name" ]] || return 1

    actual_sha=$(sha256sum "$asset_file" | awk '{print $1}') || return 1
    [[ "${actual_sha,,}" == "${expected_sha,,}" ]] || return 1
}

verify_panel_release_bundle() {
    verify_signed_release_asset "$1" "$2" "$3" "yub-wpanel" "$PANEL_ASSET_MAX_BYTES"
}

verify_license_release_bundle() {
    local archive_file="$1"
    local sha_file="$2"
    local sig_file="$3"
    local listing_file="$INSTALL_WORKDIR/license-archive.list"
    local archive_member=""
    local normalized_member=""
    local required_member=""
    local archive_release_version=""

    verify_signed_release_asset \
        "$archive_file" "$sha_file" "$sig_file" \
        "yub-wpanel-third-party-licenses.tar.gz" "$LICENSE_ARCHIVE_MAX_BYTES" || return 1
    tar -tzf "$archive_file" > "$listing_file" 2>/dev/null || return 1
    [[ -s "$listing_file" ]] || return 1
    while IFS= read -r archive_member; do
        [[ -n "$archive_member" ]] || return 1
        [[ "$archive_member" == ./* ]] || return 1
        [[ "$archive_member" != *\\* ]] || return 1
        [[ "$archive_member" == "./" ]] && continue
        normalized_member="${archive_member#./}"
        [[ -n "$normalized_member" ]] || return 1
        [[ ! "/$normalized_member/" =~ /\.\.?/ ]] || return 1
    done < "$listing_file"
    for required_member in \
        ./LICENSE \
        ./NOTICE.md \
        ./THIRD_PARTY_NOTICES.md \
        ./RELEASE_VERSION \
        ./go-toolchain/LICENSE \
        ./go-toolchain/VERSION \
        ./adminer-6.0.1/LICENSE-APACHE-2.0.txt \
        ./adminer-6.0.1/NOTICE.txt; do
        [[ "$(grep -Fxc -- "$required_member" "$listing_file")" == "1" ]] || return 1
    done

    tar -xOzf "$archive_file" ./RELEASE_VERSION > "$LICENSE_RELEASE_VERSION_FILE" 2>/dev/null || return 1
    [[ "$(wc -l < "$LICENSE_RELEASE_VERSION_FILE" | tr -d '[:space:]')" == "1" ]] || return 1
    archive_release_version=$(cat "$LICENSE_RELEASE_VERSION_FILE") || return 1
    [[ "$archive_release_version" == "$INSTALLER_RELEASE_VERSION" ]] || return 1
    tar -xOzf "$archive_file" ./LICENSE > "$PROJECT_LICENSE_FILE" 2>/dev/null || return 1
    tar -xOzf "$archive_file" ./NOTICE.md > "$PROJECT_NOTICE_FILE" 2>/dev/null || return 1
    tar -xOzf "$archive_file" ./THIRD_PARTY_NOTICES.md > "$THIRD_PARTY_NOTICE_FILE" 2>/dev/null || return 1
    [[ -s "$PROJECT_LICENSE_FILE" ]] || return 1
    [[ -s "$PROJECT_NOTICE_FILE" ]] || return 1
    [[ -s "$THIRD_PARTY_NOTICE_FILE" ]] || return 1
    chmod 0600 "$PROJECT_LICENSE_FILE" "$PROJECT_NOTICE_FILE" "$THIRD_PARTY_NOTICE_FILE"
}

verify_complete_release_bundle() {
    verify_panel_release_bundle \
        "$PANEL_CANDIDATE" "$PANEL_SHA256_FILE" "$PANEL_SIGNATURE_FILE" &&
    verify_license_release_bundle \
        "$LICENSE_ARCHIVE" "$LICENSE_SHA256_FILE" "$LICENSE_SIGNATURE_FILE"
}

validate_installer_release_version() {
    [[ "$INSTALLER_RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] &&
        panel_version_at_least "$INSTALLER_RELEASE_VERSION" "$MIN_PANEL_VERSION"
}

preflight_panel_candidate() {
    local preflight_config="$INSTALL_WORKDIR/preflight-config.json"
    local preflight_output="$INSTALL_WORKDIR/preflight-info.txt"
    local candidate_version=""

    cat > "$preflight_config" << PREFLIGHTEOF
{
  "panel": {
    "port": 8888,
    "tls_port": 8443,
    "random_suffix": "installer-preflight",
    "data_dir": "$INSTALL_WORKDIR",
    "backup_dir": "$INSTALL_WORKDIR",
    "log_dir": "$INSTALL_WORKDIR"
  },
  "sqlite": {"path": "$INSTALL_WORKDIR/preflight.db"},
  "mariadb": {"root_password": "installer-preflight-only"},
  "admin": {"username": "preflight", "password_hash": "preflight"},
  "paths": {"cron_file": "/etc/cron.d/yub_wpanel_cron"},
  "systemd": {
    "service_name": "yub-wpanel",
    "service_path": "/etc/systemd/system/yub-wpanel.service",
    "binary_path": "/usr/local/bin/yub-wpanel"
  }
}
PREFLIGHTEOF
    chmod 0600 "$preflight_config"
    chmod 0700 "$PANEL_CANDIDATE"
    timeout 20s "$PANEL_CANDIDATE" --info --config "$preflight_config" \
        > "$preflight_output" 2>&1 || return 1
    grep -q "YUB WPanel" "$preflight_output" || return 1
    candidate_version=$(sed -n 's/^版本: \(v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)\([[:space:]].*\)\{0,1\}$/\1/p' "$preflight_output")
    [[ $(printf '%s\n' "$candidate_version" | awk 'NF {count++} END {print count+0}') == "1" ]] || return 1
    panel_version_at_least "$candidate_version" "$MIN_PANEL_VERSION" || return 1
    [[ "$candidate_version" == "$INSTALLER_RELEASE_VERSION" ]] || return 1
}

panel_version_at_least() {
    [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || return 1
    [[ "$2" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || return 1
    local actual="${1#v}"
    local minimum="${2#v}"
    local actual_major="" actual_minor="" actual_patch=""
    local minimum_major="" minimum_minor="" minimum_patch=""

    [[ "$actual" =~ ^([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})$ ]] || return 1
    actual_major="${BASH_REMATCH[1]}"
    actual_minor="${BASH_REMATCH[2]}"
    actual_patch="${BASH_REMATCH[3]}"
    [[ "$minimum" =~ ^([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})$ ]] || return 1
    minimum_major="${BASH_REMATCH[1]}"
    minimum_minor="${BASH_REMATCH[2]}"
    minimum_patch="${BASH_REMATCH[3]}"

    (( 10#$actual_major > 10#$minimum_major )) && return 0
    (( 10#$actual_major < 10#$minimum_major )) && return 1
    (( 10#$actual_minor > 10#$minimum_minor )) && return 0
    (( 10#$actual_minor < 10#$minimum_minor )) && return 1
    (( 10#$actual_patch >= 10#$minimum_patch ))
}

write_panel_service_unit() {
    local target="$1"
    local mode="$2"

    cat > "$target" << SYSTEMDEOF
[Unit]
Description=WordPress Server Management Panel
After=network.target mariadb.service redis-server.service

[Service]
Type=simple
User=root
Group=root
ExecStart=$BIN_PATH --config=$CONFIG_FILE
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=yub-wpanel
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SYSTEMDEOF
    chmod "$mode" "$target"
}

validate_repair_service_unit() {
    local expected_unit="$INSTALL_WORKDIR/expected-yub-wpanel.service"
    local unit_owner=""
    local unit_mode=""
    local unit_links=""

    [[ -f "$SERVICE_PATH" ]] && [[ ! -L "$SERVICE_PATH" ]] || return 1
    unit_owner=$(stat -c '%u' "$SERVICE_PATH" 2>/dev/null) || return 1
    unit_mode=$(stat -c '%a' "$SERVICE_PATH" 2>/dev/null) || return 1
    unit_links=$(stat -c '%h' "$SERVICE_PATH" 2>/dev/null) || return 1
    [[ "$unit_owner" == "0" ]] || return 1
    [[ "$unit_links" == "1" ]] || return 1
    [[ "$unit_mode" =~ ^[0-7]{3,4}$ ]] || return 1
    (( (8#$unit_mode & 0022) == 0 )) || return 1

    write_panel_service_unit "$expected_unit" 0600 || return 1
    cmp -s -- "$expected_unit" "$SERVICE_PATH" || return 1

    validate_no_panel_service_dropins || return 1
    systemctl daemon-reload >/dev/null 2>&1 || return 1
    validate_effective_panel_service_unit
}

validate_existing_panel_binary() {
    local binary_owner=""
    local binary_mode=""
    local binary_links=""
    local binary_path=""

    [[ -f "$BIN_PATH" ]] && [[ ! -L "$BIN_PATH" ]] && [[ -s "$BIN_PATH" ]] && [[ -x "$BIN_PATH" ]] || return 1
    binary_owner=$(stat -c '%u' "$BIN_PATH" 2>/dev/null) || return 1
    binary_mode=$(stat -c '%a' "$BIN_PATH" 2>/dev/null) || return 1
    binary_links=$(stat -c '%h' "$BIN_PATH" 2>/dev/null) || return 1
    binary_path=$(readlink -f -- "$BIN_PATH" 2>/dev/null) || return 1
    [[ "$binary_owner" == "0" ]] || return 1
    [[ "$binary_mode" == "755" ]] || return 1
    [[ "$binary_links" == "1" ]] || return 1
    [[ "$binary_path" == "$BIN_PATH" ]]
}

validate_no_panel_service_dropins() {
    local unit_paths=""
    local unit_dir=""
    local dropin_name=""
    local dropin_dir=""

    unit_paths=$(systemd-analyze unit-paths 2>/dev/null) || return 1
    [[ -n "$unit_paths" ]] || return 1

    # systemd applies exact-name, dash-truncated and type-wide drop-ins from
    # every configured unit load path (including *.control, transient and
    # generator paths). Reject any effective source rather than maintaining a
    # hand-written partial directory list.
    while IFS= read -r unit_dir; do
        [[ -n "$unit_dir" ]] || continue
        for dropin_name in yub-wpanel.service.d yub-.service.d service.d; do
            dropin_dir="${unit_dir%/}/${dropin_name}"
            if [[ -e "$dropin_dir" ]] || [[ -L "$dropin_dir" ]]; then
                [[ -d "$dropin_dir" ]] && [[ ! -L "$dropin_dir" ]] || return 1
                if find "$dropin_dir" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null | grep -q .; then
                    return 1
                fi
            fi
        done
    done <<< "$unit_paths"
}

validate_effective_panel_service_unit() {
    local load_state=""
    local fragment_path=""
    local dropin_paths=""
    local exec_start=""
    local expected_exec_prefix="{ path=$BIN_PATH ; argv[]=$BIN_PATH --config=$CONFIG_FILE ; "

    load_state=$(systemctl show yub-wpanel.service --property=LoadState --value 2>/dev/null) || return 1
    fragment_path=$(systemctl show yub-wpanel.service --property=FragmentPath --value 2>/dev/null) || return 1
    dropin_paths=$(systemctl show yub-wpanel.service --property=DropInPaths --value 2>/dev/null) || return 1
    exec_start=$(systemctl show yub-wpanel.service --property=ExecStart --value 2>/dev/null) || return 1

    [[ "$load_state" == "loaded" ]] || return 1
    [[ "$fragment_path" == "$SERVICE_PATH" ]] || return 1
    [[ -z "${dropin_paths//[[:space:]]/}" ]] || return 1
    [[ "$exec_start" == "$expected_exec_prefix"* ]] || return 1
    [[ "$exec_start" != *"} {"* ]] || return 1
}

validate_running_panel_service_identity() {
    local main_pid=""
    local running_executable=""
    local installed_executable=""

    [[ -f "$BIN_PATH" ]] && [[ ! -L "$BIN_PATH" ]] || return 1
    installed_executable=$(readlink -f -- "$BIN_PATH" 2>/dev/null) || return 1
    [[ "$installed_executable" == "$BIN_PATH" ]] || return 1

    for _ in 1 2 3 4 5 6 7 8 9 10; do
        main_pid=$(systemctl show yub-wpanel.service --property=MainPID --value 2>/dev/null || true)
        if [[ "$main_pid" =~ ^[1-9][0-9]*$ ]] && [[ -e "/proc/${main_pid}/exe" ]]; then
            running_executable=$(readlink -- "/proc/${main_pid}/exe" 2>/dev/null || true)
            if [[ "$running_executable" == "$BIN_PATH" ]]; then
                return 0
            fi
        fi
        sleep 1
    done
    return 1
}

validate_existing_panel_cron_file() {
	local cron_parent="${CRON_PATH%/*}"
	local parent_owner=""
	local parent_mode=""
    local cron_owner=""
    local cron_mode=""
    local cron_links=""
    local first_line=""

	[[ -d "$cron_parent" ]] && [[ ! -L "$cron_parent" ]] || return 1
	parent_owner=$(stat -c '%u' "$cron_parent" 2>/dev/null) || return 1
	parent_mode=$(stat -c '%a' "$cron_parent" 2>/dev/null) || return 1
	[[ "$parent_owner" == "0" ]] || return 1
	[[ "$parent_mode" =~ ^[0-7]{3,4}$ ]] || return 1
	(( (8#$parent_mode & 0022) == 0 )) || return 1

    if [[ ! -e "$CRON_PATH" ]] && [[ ! -L "$CRON_PATH" ]]; then
        return 0
    fi
    [[ -f "$CRON_PATH" ]] && [[ ! -L "$CRON_PATH" ]] || return 1
    cron_owner=$(stat -c '%u' "$CRON_PATH" 2>/dev/null) || return 1
    cron_mode=$(stat -c '%a' "$CRON_PATH" 2>/dev/null) || return 1
    cron_links=$(stat -c '%h' "$CRON_PATH" 2>/dev/null) || return 1
    [[ "$cron_owner" == "0" ]] || return 1
    [[ "$cron_links" == "1" ]] || return 1
    [[ "$cron_mode" =~ ^[0-7]{3,4}$ ]] || return 1
    (( (8#$cron_mode & 0022) == 0 )) || return 1
    IFS= read -r first_line < "$CRON_PATH" || return 1
    [[ "$first_line" == "# YUB WPanel Cron Jobs — DO NOT EDIT MANUALLY" ]]
}

validate_fresh_panel_cron_location() {
	local cron_parent="${CRON_PATH%/*}"
	local nearest_parent="$cron_parent"
	local parent_owner=""
	local parent_mode=""
	local next_parent=""

	if [[ -e "$cron_parent" ]] || [[ -L "$cron_parent" ]]; then
		validate_existing_panel_cron_file
		return
	fi
	# Minimal Debian images may not have /etc/cron.d until cron-daemon-common is
	# installed. Reject broken links and validate the nearest existing parent;
	# after package installation the exact /etc/cron.d directory is rechecked.
	while [[ ! -e "$nearest_parent" ]]; do
		[[ ! -L "$nearest_parent" ]] || return 1
		next_parent="${nearest_parent%/*}"
		[[ -n "$next_parent" ]] && [[ "$next_parent" != "$nearest_parent" ]] || return 1
		nearest_parent="$next_parent"
	done
	[[ -d "$nearest_parent" ]] && [[ ! -L "$nearest_parent" ]] || return 1
	parent_owner=$(stat -c '%u' "$nearest_parent" 2>/dev/null) || return 1
	parent_mode=$(stat -c '%a' "$nearest_parent" 2>/dev/null) || return 1
	[[ "$parent_owner" == "0" ]] || return 1
	[[ "$parent_mode" =~ ^[0-7]{3,4}$ ]] || return 1
	(( (8#$parent_mode & 0022) == 0 ))
}

copy_local_release_bundle() {
    local script_dir="$1"

    file_size_within_limit "$script_dir/yub-wpanel" "$PANEL_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/yub-wpanel.sha256" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/yub-wpanel.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz" "$LICENSE_ARCHIVE_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    install -m 0600 "$script_dir/yub-wpanel" "$PANEL_CANDIDATE"
    install -m 0600 "$script_dir/yub-wpanel.sha256" "$PANEL_SHA256_FILE"
    install -m 0600 "$script_dir/yub-wpanel.sha256.sig" "$PANEL_SIGNATURE_FILE"
    install -m 0600 "$script_dir/yub-wpanel-third-party-licenses.tar.gz" "$LICENSE_ARCHIVE"
    install -m 0600 "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256" "$LICENSE_SHA256_FILE"
    install -m 0600 "$script_dir/yub-wpanel-third-party-licenses.tar.gz.sha256.sig" "$LICENSE_SIGNATURE_FILE"
}

download_release_bundle() {
    local release_base_url="$1"
    local binary_url="${release_base_url}/yub-wpanel"
    local license_url="${release_base_url}/yub-wpanel-third-party-licenses.tar.gz"

    rm -f \
        "$PANEL_CANDIDATE" "$PANEL_SHA256_FILE" "$PANEL_SIGNATURE_FILE" \
        "$LICENSE_ARCHIVE" "$LICENSE_SHA256_FILE" "$LICENSE_SIGNATURE_FILE"
    download_file "$binary_url" "$PANEL_CANDIDATE" 180 "$PANEL_ASSET_MAX_BYTES" || return 1
    download_file "${binary_url}.sha256" "$PANEL_SHA256_FILE" 60 "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    download_file "${binary_url}.sha256.sig" "$PANEL_SIGNATURE_FILE" 60 "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    download_file "$license_url" "$LICENSE_ARCHIVE" 120 "$LICENSE_ARCHIVE_MAX_BYTES" || return 1
    download_file "${license_url}.sha256" "$LICENSE_SHA256_FILE" 60 "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    download_file "${license_url}.sha256.sig" "$LICENSE_SIGNATURE_FILE" 60 "$SIGNATURE_ASSET_MAX_BYTES" || return 1
}

prepare_panel_candidate() {
    local script_dir=""
    local github_release_base=""
    local proxy_release_base=""
    local bundle_source=""

    validate_installer_release_version || \
        log_error "安装器缺少规范、受支持的固定 Release 版本；请使用已签名 GitHub Release 资产"
    github_release_base="https://github.com/zangwp/yub-wpanel/releases/download/${INSTALLER_RELEASE_VERSION}"
    if [[ -n "$GHPROXY" ]]; then
        proxy_release_base="${GHPROXY%/}/${github_release_base}"
    fi

    if $PANEL_CANDIDATE_VERIFIED && [[ -x "$PANEL_CANDIDATE" ]]; then
        return 0
    fi
    [[ -n "$INSTALL_WORKDIR" ]] && [[ -d "$INSTALL_WORKDIR" ]] || \
        log_error "安装临时工作目录尚未初始化"
    script_dir="$(cd "$(dirname "$0")" && pwd)"

    if copy_local_release_bundle "$script_dir"; then
        bundle_source="同目录离线发布包"
        if ! verify_complete_release_bundle; then
            log_error "同目录面板与许可发布包未通过 Ed25519 签名、SHA256 或内容校验"
        fi
    elif $PREFER_CN && [[ -n "$proxy_release_base" ]]; then
        if download_release_bundle "$proxy_release_base" && \
           verify_complete_release_bundle; then
            bundle_source="自定义 GitHub 反代"
        elif download_release_bundle "$github_release_base" && \
             verify_complete_release_bundle; then
            bundle_source="GitHub Releases"
        else
            log_error "无法获取通过 Ed25519 签名、SHA256 和内容校验的面板与许可发布包"
        fi
    else
        if download_release_bundle "$github_release_base" && \
           verify_complete_release_bundle; then
            bundle_source="GitHub Releases"
        elif [[ -n "$proxy_release_base" ]] && download_release_bundle "$proxy_release_base" && \
             verify_complete_release_bundle; then
            bundle_source="自定义 GitHub 反代"
        else
            log_error "无法获取通过 Ed25519 签名、SHA256 和内容校验的面板与许可发布包"
        fi
    fi

    preflight_panel_candidate || log_error "已验签面板二进制未通过 --info 安全预检"
    PANEL_CANDIDATE_VERIFIED=true
    log_info "固定版本 ${INSTALLER_RELEASE_VERSION} 的面板与许可发布包验签、内容校验和预检通过: ${bundle_source}"
}

create_repair_backup() {
    local timestamp=""
    timestamp=$(date -u +%Y%m%dT%H%M%SZ)
    REPAIR_BACKUP_DIR="$INSTALL_DIR/backups/install-repair/${timestamp}-$$"
    [[ -s "$BIN_PATH" ]] && REPAIR_BIN_EXISTED=true
    [[ -f "$SERVICE_PATH" ]] && REPAIR_UNIT_EXISTED=true
    [[ -f "$DB_PATH" ]] && REPAIR_DB_EXISTED=true
    if [[ -f "$INSTALL_DIR/certs/panel.crt" ]] && [[ -f "$INSTALL_DIR/certs/panel.key" ]]; then
        REPAIR_TLS_EXISTED=true
    fi
    install -d -m 0700 "$REPAIR_BACKUP_DIR"

    install -m 0600 "$CONFIG_FILE" "$REPAIR_BACKUP_DIR/config.json"
    if [[ -f "$DB_PATH" ]]; then
        sqlite3 "$DB_PATH" ".timeout 10000" ".backup $REPAIR_BACKUP_DIR/panel.db"
        [[ "$(sqlite3 "$REPAIR_BACKUP_DIR/panel.db" 'PRAGMA integrity_check;')" == "ok" ]] || \
            log_error "repair备份SQLite完整性检查失败"
    fi
    if $REPAIR_BIN_EXISTED; then
        install -m 0755 "$BIN_PATH" "$REPAIR_BACKUP_DIR/yub-wpanel"
    fi
    if $REPAIR_UNIT_EXISTED; then
        install -m 0644 "$SERVICE_PATH" "$REPAIR_BACKUP_DIR/yub-wpanel.service"
    fi
    if $REPAIR_TLS_EXISTED; then
        install -m 0644 "$INSTALL_DIR/certs/panel.crt" "$REPAIR_BACKUP_DIR/panel.crt"
        install -m 0600 "$INSTALL_DIR/certs/panel.key" "$REPAIR_BACKUP_DIR/panel.key"
    fi
    find "$REPAIR_BACKUP_DIR" -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > "$REPAIR_BACKUP_DIR/SHA256SUMS"
    sha256sum -c "$REPAIR_BACKUP_DIR/SHA256SUMS" >/dev/null || log_error "repair备份校验失败"
}

atomic_install_managed_file() {
	local source_path="$1"
	local target_path="$2"
	local target_mode="$3"
	local target_dir="${target_path%/*}"
	local target_base="${target_path##*/}"
	local dir_owner=""
	local dir_mode=""

	[[ "$target_path" == "$BIN_PATH" ]] || [[ "$target_path" == "$DB_PATH" ]] || return 1
	[[ -f "$source_path" ]] && [[ ! -L "$source_path" ]] || return 1
	[[ -d "$target_dir" ]] && [[ ! -L "$target_dir" ]] || return 1
	dir_owner=$(stat -c '%u' "$target_dir" 2>/dev/null) || return 1
	dir_mode=$(stat -c '%a' "$target_dir" 2>/dev/null) || return 1
	[[ "$dir_owner" == "0" ]] || return 1
	[[ "$dir_mode" =~ ^[0-7]{3,4}$ ]] || return 1
	(( (8#$dir_mode & 0022) == 0 )) || return 1

	cleanup_atomic_stage_file
	ATOMIC_STAGE_PATH=$(mktemp "${target_dir}/.${target_base}.yub-install.XXXXXXXX") || return 1
	if ! install -m "$target_mode" "$source_path" "$ATOMIC_STAGE_PATH" || \
	   ! sync -f "$ATOMIC_STAGE_PATH" || \
	   ! mv -f -- "$ATOMIC_STAGE_PATH" "$target_path"; then
		cleanup_atomic_stage_file
		return 1
	fi
	ATOMIC_STAGE_PATH=""
	sync -f "$target_dir"
}

validate_license_document_directory() {
    local doc_parent="${LICENSE_DOC_DIR%/*}"
    local path_owner=""
    local path_mode=""

    [[ "$doc_parent" == "/usr/share/doc" ]] || return 1
    [[ -d "$doc_parent" ]] && [[ ! -L "$doc_parent" ]] || return 1
    [[ "$(readlink -f -- "$doc_parent" 2>/dev/null)" == "$doc_parent" ]] || return 1
    path_owner=$(stat -c '%u' "$doc_parent" 2>/dev/null) || return 1
    path_mode=$(stat -c '%a' "$doc_parent" 2>/dev/null) || return 1
    [[ "$path_owner" == "0" ]] || return 1
    [[ "$path_mode" =~ ^[0-7]{3,4}$ ]] || return 1
    (( (8#$path_mode & 0022) == 0 )) || return 1

    if [[ -e "$LICENSE_DOC_DIR" ]] || [[ -L "$LICENSE_DOC_DIR" ]]; then
        [[ -d "$LICENSE_DOC_DIR" ]] && [[ ! -L "$LICENSE_DOC_DIR" ]] || return 1
    else
        install -d -m 0755 "$LICENSE_DOC_DIR" || return 1
    fi
    [[ "$(readlink -f -- "$LICENSE_DOC_DIR" 2>/dev/null)" == "$LICENSE_DOC_DIR" ]] || return 1
    path_owner=$(stat -c '%u' "$LICENSE_DOC_DIR" 2>/dev/null) || return 1
    path_mode=$(stat -c '%a' "$LICENSE_DOC_DIR" 2>/dev/null) || return 1
    [[ "$path_owner" == "0" ]] || return 1
    [[ "$path_mode" =~ ^[0-7]{3,4}$ ]] || return 1
    (( (8#$path_mode & 0022) == 0 ))
}

atomic_install_license_document() {
    local source_path="$1"
    local target_path="$2"
    local target_base="${target_path##*/}"
    local stage_path=""

    [[ -f "$source_path" ]] && [[ ! -L "$source_path" ]] || return 1
    case "$target_path" in
        "$LICENSE_DOC_DIR/LICENSE"|\
        "$LICENSE_DOC_DIR/NOTICE.md"|\
        "$LICENSE_DOC_DIR/THIRD_PARTY_NOTICES.md"|\
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz"|\
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz.sha256"|\
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz.sha256.sig"|\
        "$LICENSE_DOC_DIR/release-public-key.pem") ;;
        *) return 1 ;;
    esac
    if [[ -e "$target_path" ]] || [[ -L "$target_path" ]]; then
        [[ -f "$target_path" ]] && [[ ! -L "$target_path" ]] || return 1
    fi

    stage_path=$(mktemp "$LICENSE_DOC_DIR/.${target_base}.yub-install.XXXXXXXX") || return 1
    if ! install -m 0644 "$source_path" "$stage_path" || \
       ! sync -f "$stage_path" || \
       ! mv -f -- "$stage_path" "$target_path"; then
        rm -f -- "$stage_path"
        return 1
    fi
    sync -f "$LICENSE_DOC_DIR"
}

install_release_license_documentation() {
    local public_key_file="$INSTALL_WORKDIR/release-public-key.pem"

    validate_license_document_directory || return 1
    atomic_install_license_document "$PROJECT_LICENSE_FILE" "$LICENSE_DOC_DIR/LICENSE" || return 1
    atomic_install_license_document "$PROJECT_NOTICE_FILE" "$LICENSE_DOC_DIR/NOTICE.md" || return 1
    atomic_install_license_document "$THIRD_PARTY_NOTICE_FILE" "$LICENSE_DOC_DIR/THIRD_PARTY_NOTICES.md" || return 1
    atomic_install_license_document "$LICENSE_ARCHIVE" \
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz" || return 1
    atomic_install_license_document "$LICENSE_SHA256_FILE" \
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz.sha256" || return 1
    atomic_install_license_document "$LICENSE_SIGNATURE_FILE" \
        "$LICENSE_DOC_DIR/yub-wpanel-third-party-licenses.tar.gz.sha256.sig" || return 1
    atomic_install_license_document "$public_key_file" "$LICENSE_DOC_DIR/release-public-key.pem" || return 1
}

repair_rollback() {
    [[ "$REPAIR_MODE" == true ]] || return 0
    [[ "$REPAIR_COMMITTED" == false ]] || return 0
    [[ "$REPAIR_MUTATED" == true ]] || return 0
    [[ -n "$REPAIR_BACKUP_DIR" ]] && [[ -d "$REPAIR_BACKUP_DIR" ]] || return 0

    local db_rollback_ok=true
    log_warn "repair未完成，正在恢复repair前的面板程序和数据库"
    # 新二进制可能已经升级SQLite结构。必须先停服并恢复同一时间点的数据库，
    # 不能让恢复后的旧二进制继续读取新结构。
    if ! systemctl stop yub-wpanel 2>/dev/null; then
        log_warn "严重：无法停止yub-wpanel，未恢复面板数据库，保持服务停止后请联系开发者处理"
        db_rollback_ok=false
    elif $REPAIR_DB_EXISTED; then
        if atomic_install_managed_file "$REPAIR_BACKUP_DIR/panel.db" "$DB_PATH" 0600; then
            rm -f "${DB_PATH}-wal" "${DB_PATH}-shm"
        else
            log_warn "严重：repair前的面板数据库恢复失败，旧面板不会重新启动，请联系开发者处理"
            db_rollback_ok=false
        fi
    else
        rm -f "$DB_PATH" "${DB_PATH}-wal" "${DB_PATH}-shm"
    fi

    if $REPAIR_BIN_EXISTED; then
		if ! atomic_install_managed_file "$REPAIR_BACKUP_DIR/yub-wpanel" "$BIN_PATH" 0755; then
			log_warn "严重：repair前的面板二进制原子恢复失败，旧面板不会重新启动，请联系开发者处理"
			db_rollback_ok=false
		fi
    else
        rm -f "$BIN_PATH"
    fi
    if $REPAIR_UNIT_EXISTED; then
        install -m 0644 "$REPAIR_BACKUP_DIR/yub-wpanel.service" "$SERVICE_PATH"
    else
        rm -f "$SERVICE_PATH"
    fi
    if $REPAIR_TLS_EXISTED; then
        install -m 0644 "$REPAIR_BACKUP_DIR/panel.crt" "$INSTALL_DIR/certs/panel.crt"
        install -m 0600 "$REPAIR_BACKUP_DIR/panel.key" "$INSTALL_DIR/certs/panel.key"
    elif [[ "${REPAIR_TLS_ACTION:-}" == "generate" ]]; then
        rm -f "$INSTALL_DIR/certs/panel.crt" "$INSTALL_DIR/certs/panel.key"
    fi
    systemctl daemon-reload 2>/dev/null || true
    if $REPAIR_SERVICE_WAS_ACTIVE && $db_rollback_ok; then
        systemctl start yub-wpanel 2>/dev/null || true
    else
        systemctl stop yub-wpanel 2>/dev/null || true
    fi
}

apt_package_available() {
    local pkg="$1"
    local candidate=""

    candidate=$(LC_ALL=C apt-cache policy "$pkg" 2>/dev/null | awk '/Candidate:/ {print $2; exit}' || true)
    if [[ -n "$candidate" ]] && [[ "$candidate" != "(none)" ]]; then
        return 0
    fi

    LC_ALL=C apt-cache show "$pkg" >/dev/null 2>&1
}

php_package_available() {
    local pkg="$1"

    apt_package_available "$pkg"
}

set_debian_source_meta() {
    case "$1" in
        nju)
            DEBIAN_SOURCE_LABEL="南京大学 Debian 镜像"
            DEBIAN_REPO_URL="http://mirror.nju.edu.cn/debian"
            DEBIAN_SECURITY_URL="http://mirror.nju.edu.cn/debian-security"
            ;;
        ustc)
            DEBIAN_SOURCE_LABEL="中科大 Debian 镜像"
            DEBIAN_REPO_URL="http://mirrors.ustc.edu.cn/debian"
            DEBIAN_SECURITY_URL="http://mirrors.ustc.edu.cn/debian-security"
            ;;
        tuna)
            DEBIAN_SOURCE_LABEL="清华大学 Debian 镜像"
            DEBIAN_REPO_URL="http://mirrors.tuna.tsinghua.edu.cn/debian"
            DEBIAN_SECURITY_URL="http://mirrors.tuna.tsinghua.edu.cn/debian-security"
            ;;
        official)
            DEBIAN_SOURCE_LABEL="Debian 官方源"
            DEBIAN_REPO_URL="http://deb.debian.org/debian"
            DEBIAN_SECURITY_URL="http://security.debian.org/debian-security"
            ;;
        *)
            return 1
            ;;
    esac
}

backup_default_debian_sources() {
    local source_file=""

    mkdir -p /etc/apt/sources.list.d

    if [[ -f /etc/apt/sources.list.d/debian.sources ]]; then
        if [[ ! -f /etc/apt/sources.list.d/debian.sources.yub-wpanel.bak ]]; then
            cp /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/debian.sources.yub-wpanel.bak
        fi
        mv /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/debian.sources.yub-wpanel.disabled
    fi

    for source_file in /etc/apt/sources.list /etc/apt/sources.list.d/*.list; do
        [[ -f "$source_file" ]] || continue
        if [[ ! -f "${source_file}.yub-wpanel.bak" ]]; then
            cp "$source_file" "${source_file}.yub-wpanel.bak"
        fi
        sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]].*(\/debian-security|\/debian([[:space:]\/]|$)|deb\.debian\.org|security\.debian\.org)/ s/^/# disabled by YUB WPanel: /' "$source_file"
    done
}

write_debian_sources() {
    local codename="$1"

    cat > /etc/apt/sources.list.d/yub-wpanel-debian.sources << DEBIANSOURCESEOF
Types: deb
URIs: ${DEBIAN_REPO_URL}
Suites: ${codename} ${codename}-updates
Components: main contrib non-free non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: ${DEBIAN_SECURITY_URL}
Suites: ${codename}-security
Components: main contrib non-free non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
DEBIANSOURCESEOF
}

debian_packages_available() {
    local packages=(ca-certificates wget curl gnupg lsb-release nginx mariadb-server redis-server)
    local pkg=""

    for pkg in "${packages[@]}"; do
        if ! apt_package_available "$pkg"; then
            log_warn "APT 源缺少关键包候选版本: ${pkg}"
            return 1
        fi
    done
    return 0
}

configure_debian_source() {
    local source_id="$1"
    local codename="$2"
    local apt_log="$INSTALL_WORKDIR/debian-apt-update.log"

    set_debian_source_meta "$source_id" || return 1
    log_info "尝试 Debian 源: ${DEBIAN_SOURCE_LABEL}"
    write_debian_sources "$codename"

    if apt-get update > "$apt_log" 2>&1 && debian_packages_available; then
        rm -f "$apt_log"
        log_info "Debian 源可用: ${DEBIAN_SOURCE_LABEL}"
        return 0
    fi

    log_warn "${DEBIAN_SOURCE_LABEL} 不可用或同步不完整，准备尝试下一个 Debian 源"
    if [[ -f "$apt_log" ]]; then
        tail -n 8 "$apt_log" 2>/dev/null || true
    fi
    rm -f "$apt_log"
    return 1
}

select_debian_source() {
    local codename="$1"
    local candidates=()
    local source_id=""

    if $PREFER_CN; then
        candidates=(nju ustc tuna official)
        backup_default_debian_sources
    else
        log_info "使用系统默认 Debian APT 源"
        apt-get update
        debian_packages_available || log_error "系统默认 APT 源缺少关键包，请检查 /etc/apt/sources.list 或 /etc/apt/sources.list.d/"
        return 0
    fi

    for source_id in "${candidates[@]}"; do
        if configure_debian_source "$source_id" "$codename"; then
            if [[ "$source_id" == "official" ]]; then
                log_warn "国内镜像同步可能延迟，已回退官方源"
            fi
            return 0
        fi
    done

    log_error "所有 Debian APT 源均不可用。请检查网络、DNS、系统时间，或手动配置可用镜像源后重试。"
}

configure_php_source() {
    local source_id="$1"
    local codename="$2"
    local keyring_file="/usr/share/keyrings/debsuryorg-archive-keyring.gpg"
    local tmp_key="$INSTALL_WORKDIR/debsuryorg-archive-keyring.deb"
    local apt_log="$INSTALL_WORKDIR/php-apt-update.log"
    local actual_sha=""
    local package_name=""
    local package_version=""
    local package_arch=""

    set_php_source_meta "$source_id" || return 1
    log_info "尝试 PHP 源: ${PHP_SOURCE_LABEL}"

    rm -f "$tmp_key"
    if download_file "$PHP_KEY_URL" "$tmp_key" 20 "$PHP_KEYRING_MAX_BYTES"; then
        actual_sha=$(sha256sum "$tmp_key" 2>/dev/null | awk '{print $1}') || actual_sha=""
        if [[ "$actual_sha" != "$DEBSURY_KEYRING_SHA256" ]]; then
            rm -f "$tmp_key"
            log_warn "${PHP_SOURCE_LABEL} keyring SHA-256 不匹配，拒绝执行下载的安装包"
            return 1
        fi

        package_name=$(dpkg-deb -f "$tmp_key" Package 2>/dev/null || true)
        package_version=$(dpkg-deb -f "$tmp_key" Version 2>/dev/null || true)
        package_arch=$(dpkg-deb -f "$tmp_key" Architecture 2>/dev/null || true)
        if [[ "$package_name" != "$DEBSURY_KEYRING_PACKAGE" ]] || \
            [[ "$package_version" != "$DEBSURY_KEYRING_VERSION" ]] || \
            [[ "$package_arch" != "all" ]]; then
            rm -f "$tmp_key"
            log_warn "${PHP_SOURCE_LABEL} keyring 包元数据不匹配，拒绝执行"
            return 1
        fi

        if ! dpkg -i "$tmp_key" >/dev/null 2>&1; then
            rm -f "$tmp_key"
            log_warn "${PHP_SOURCE_LABEL} GPG key 安装失败"
            return 1
        fi
        rm -f "$tmp_key"
    else
        rm -f "$tmp_key"
        log_warn "${PHP_SOURCE_LABEL} GPG key 下载失败；不会复用未由本次安装验证的 keyring"
        return 1
    fi

    [[ -f "$keyring_file" ]] && [[ ! -L "$keyring_file" ]] || {
        log_warn "${PHP_SOURCE_LABEL} 未生成预期的常规 keyring 文件"
        return 1
    }

    cat > /etc/apt/sources.list.d/php.sources << PHPSOURCESEOF
Types: deb
URIs: ${PHP_REPO_URL}
Suites: ${codename}
Components: main
Signed-By: ${keyring_file}
PHPSOURCESEOF

    if apt-get update > "$apt_log" 2>&1 && \
        php_package_available php8.3-cli && \
        php_package_available php8.3-fpm; then
        rm -f "$apt_log"
        log_info "PHP 源可用: ${PHP_SOURCE_LABEL}"
        return 0
    fi

    log_warn "${PHP_SOURCE_LABEL} 不可用，准备尝试下一个 PHP 源"
    if [[ -f "$apt_log" ]]; then
        tail -n 8 "$apt_log" 2>/dev/null || true
    fi
    rm -f "$apt_log"
    return 1
}

select_php_source() {
    local codename="$1"
    local candidates=()

    case "$PHP_SOURCE_MODE" in
        auto|"")
            if $PREFER_CN; then
                candidates=(ustc sjtu official)
            else
                candidates=(official ustc sjtu)
            fi
            ;;
        official|ustc|sjtu)
            candidates=("$PHP_SOURCE_MODE")
            ;;
        *)
            log_warn "未知 PHP 源模式 ${PHP_SOURCE_MODE}，回退到 auto"
            candidates=(official ustc sjtu)
            ;;
    esac

    for source_id in "${candidates[@]}"; do
        if configure_php_source "$source_id" "$codename"; then
            return 0
        fi
    done

    log_error "所有 PHP 8.3 源均不可用。请检查网络、DNS、证书时间，或稍后重试。"
}

# ============================================================
# 卸载函数（定义在前，兼容管道执行）
# ============================================================

is_exact_purge_confirmation() {
    [[ "${1:-}" == "PURGE" ]]
}

cleanup_yub_runtime_integrations() {
    local jail=""
    local logrotate_file=""

    # Stop only fixed YUB units and jails. Every operation is best-effort so a
    # stale or partially installed integration cannot prevent uninstallation.
    systemctl stop yubwpanel-whitelist.timer 2>/dev/null || true
    systemctl stop yubwpanel-whitelist.service 2>/dev/null || true
    systemctl disable yub-wpanel 2>/dev/null || true
    systemctl disable yubwpanel-whitelist.timer 2>/dev/null || true
    systemctl disable yubwpanel-whitelist.service 2>/dev/null || true

    if command -v fail2ban-client >/dev/null 2>&1; then
        for jail in yubwpanel yubwpanel-404 yubwpanel-login yubwpanel-sshd yubwpanel-sqli; do
            fail2ban-client stop "$jail" >/dev/null 2>&1 || true
        done
    fi

    rm -f -- \
        /etc/cron.d/yub_wpanel_cron \
        /etc/systemd/system/yub-wpanel.service \
        /etc/systemd/system/multi-user.target.wants/yub-wpanel.service \
        /run/systemd/transient/yub-wpanel.service \
        /etc/systemd/system/yubwpanel-whitelist.timer \
        /etc/systemd/system/yubwpanel-whitelist.service \
        /etc/systemd/system/timers.target.wants/yubwpanel-whitelist.timer \
        /etc/systemd/system/nginx.service.d/yub-wpanel.conf \
        /etc/systemd/system/php8.3-fpm.service.d/yub-wpanel.conf \
        /etc/systemd/system/mariadb.service.d/yub-wpanel.conf \
        /etc/systemd/system/redis-server.service.d/yub-wpanel.conf \
        /etc/fail2ban/jail.d/yubwpanel.conf \
        /etc/fail2ban/action.d/yubwpanel-nginx.conf \
        /etc/fail2ban/action.d/yubwpanel-record.conf \
        /etc/fail2ban/filter.d/yubwpanel.conf \
        /etc/fail2ban/filter.d/yubwpanel-404.conf \
        /etc/fail2ban/filter.d/yubwpanel-login.conf \
        /etc/fail2ban/filter.d/yubwpanel-sqli.conf \
        /etc/nginx/conf.d/yubwpanel.conf \
        /etc/nginx/conf.d/yubwpanel-cache-bypass.conf \
        /etc/nginx/conf.d/yubwpanel-ssl-default.conf \
        /etc/nginx/conf.d/yubwpanel-ratelimit.conf \
        /etc/nginx/conf.d/yubwpanel-botlimit.conf \
        /etc/nginx/conf.d/yubwpanel-limit-status.conf \
        /etc/nginx/conf.d/yubwpanel-cache.conf \
        /etc/nginx/conf.d/yubwpanel-log.conf \
        /etc/nginx/conf.d/yubwpanel-realip.conf \
        /etc/nginx/conf.d/yubwpanel-banned-ips.conf \
        2>/dev/null || true

    # Remove only unit-specific overrides owned by this panel. Hierarchical or
    # global drop-ins are never deleted automatically; the installer detects
    # and rejects them before it changes the host.
    rm -rf -- \
        /etc/systemd/system/yub-wpanel.service.d \
        /run/systemd/system/yub-wpanel.service.d \
        /etc/systemd/system.control/yub-wpanel.service.d \
        /run/systemd/system.control/yub-wpanel.service.d \
        2>/dev/null || true

    # Site logrotate names are dynamic. Delete only regular files in the fixed
    # directory whose first line carries YUB WPanel's ownership marker.
    while IFS= read -r -d '' logrotate_file; do
        [[ -f "$logrotate_file" ]] && [[ ! -L "$logrotate_file" ]] || continue
        if head -n 1 -- "$logrotate_file" 2>/dev/null | \
            grep -Eq '^# YUB WPanel Generated - [A-Za-z0-9._-]+$'; then
            rm -f -- "$logrotate_file" 2>/dev/null || true
        fi
    done < <(find /etc/logrotate.d -maxdepth 1 -type f -name 'yubwpanel-*' -print0 2>/dev/null)

    systemctl daemon-reload 2>/dev/null || true
}

do_uninstall() {
    echo ""
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}  普通卸载将永久删除 /www/server/panel 全部内容，包括：${NC}"
    echo -e "  - 面板数据库 panel.db 与 config.json"
    echo -e "  - 面板自身 TLS 证书和私钥"
    echo -e "  - 面板本地备份、共享安装包缓存与该目录内的登录凭据/密钥"
    echo -e "${GREEN}  普通卸载保留站点文件、站点日志、站点证书、站点 Nginx/PHP 配置、MariaDB 数据库和共享系统软件。${NC}"
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo -e "${BOLD}正在卸载面板，请稍候...${NC}"

    echo -e "  → 停止面板服务..."
    systemctl stop yub-wpanel 2>/dev/null || true
    systemctl disable yub-wpanel 2>/dev/null || true
    cleanup_yub_runtime_integrations
    echo -e "  ${GREEN}✓${NC} 面板服务已停止"

    echo -e "  → 删除面板文件..."
    rm -f "$BIN_PATH"
    rm -f /usr/local/bin/yubw
    grep -qx '# YUB WPanel CLI — wp' /usr/local/bin/wp 2>/dev/null && rm -f /usr/local/bin/wp
    rm -rf "$INSTALL_DIR"
    rm -rf -- "$LICENSE_DOC_DIR"
    echo -e "  ${GREEN}✓${NC} 面板文件已删除"

    echo -e "  → 重新加载 Nginx..."
    nginx -s reload 2>/dev/null || true
    echo -e "  ${GREEN}✓${NC} YUB Nginx 配置已清理"

    echo ""
    log_info "面板已卸载。以下内容已保留："
    log_info "  - /www/wwwroot（网站文件）"
    log_info "  - /www/wwwlogs（网站日志）"
    log_info "  - /www/server/certificates（站点 SSL 证书，不包括已删除的面板 TLS 身份）"
    log_info "  - /etc/nginx/sites-available 与 sites-enabled（站点 Nginx 配置）"
    log_info "  - /etc/php/8.3/fpm/pool.d（站点 PHP-FPM pools）"
    log_info "  - MariaDB 数据库"
    log_info "  - 系统软件包（nginx/php/mariadb/redis/fail2ban）"
}

do_purge() {
    echo ""
    echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${RED}  高风险警告：彻底清空会删除下列数据和配置：${NC}"
    echo -e "  - /etc/nginx/sites-enabled/* 和 sites-available/*（全部 Nginx site 配置）"
    echo -e "  - /etc/php/8.3/fpm/pool.d/*.conf（全部 PHP-FPM pools）"
    echo -e "  - /www/wwwroot、/www/wwwlogs、/www/server/certificates"
    echo -e "  - /www/server/panel（面板状态、凭据、备份和共享安装包缓存）"
    echo -e "  - 共享系统软件：Nginx、PHP 8.3、MariaDB、Redis、Fail2ban"
    echo -e "${RED}  这些目录和软件可能同时被非 YUB 工作负载使用；操作可能使其停机或永久丢失数据。${NC}"
    echo -e "${RED}  此操作不可逆。${NC}"
    echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo -e "  这是选择“彻底清空”后的第二次确认。"
    echo -e "  请输入精确的 ${BOLD}PURGE${NC} 继续，其他任何输入（含直接回车）都会取消。"

    local purge_confirmation=""
    read -r -p "  > " purge_confirmation < /dev/tty 2>/dev/null || purge_confirmation=""
    if ! is_exact_purge_confirmation "$purge_confirmation"; then
        log_info "未输入精确的 PURGE，已取消彻底清空"
        return 0
    fi

    echo ""
    echo -e "${BOLD}正在清空，请耐心等待...${NC}"

    echo -e "  → 停止所有服务..."
    systemctl stop yub-wpanel 2>/dev/null || true
    systemctl stop nginx 2>/dev/null || true
    systemctl stop php8.3-fpm 2>/dev/null || true
    systemctl stop mariadb 2>/dev/null || true
    systemctl stop redis-server 2>/dev/null || true
    systemctl stop fail2ban 2>/dev/null || true
    echo -e "  ${GREEN}✓${NC} 服务已停止"

    echo -e "  → 清理 YUB 定时任务、systemd、Fail2ban 和 logrotate 集成..."
    cleanup_yub_runtime_integrations
    echo -e "  ${GREEN}✓${NC} YUB 运行时集成已清理"

    echo -e "  → 清理网站 Nginx 和 PHP-FPM 配置..."
    rm -f /etc/nginx/sites-enabled/*
    rm -f /etc/nginx/sites-available/*
    rm -f /etc/php/8.3/fpm/pool.d/*.conf
    echo -e "  ${GREEN}✓${NC} 配置已清理"

    echo -e "  → 卸载软件包（可能需要 1-2 分钟）..."
    DEBIAN_FRONTEND=noninteractive apt-get purge -y nginx nginx-common mariadb-server mariadb-common redis-server fail2ban php8.3-* 2>/dev/null || true
    DEBIAN_FRONTEND=noninteractive apt-get autoremove -y 2>/dev/null || true
    echo -e "  ${GREEN}✓${NC} 软件包已卸载"

    echo -e "  → 移除 YUB 系统调优配置..."
    rm -f /etc/sysctl.d/99-yub-wpanel.conf
    sysctl --system >/dev/null 2>&1
    sed -i '/nofile 65535/d' /etc/security/limits.conf 2>/dev/null || true
    echo -e "  ${GREEN}✓${NC} YUB 系统调优配置已移除"

    echo -e "  → 删除面板文件..."
    rm -f "$BIN_PATH"
    rm -f /usr/local/bin/yubw
    grep -qx '# YUB WPanel CLI — wp' /usr/local/bin/wp 2>/dev/null && rm -f /usr/local/bin/wp
    rm -rf "$INSTALL_DIR"
    rm -rf -- "$LICENSE_DOC_DIR"
    echo -e "  ${GREEN}✓${NC} 面板文件已删除"

    echo -e "  → 删除网站数据..."
    rm -rf /www/wwwroot /www/wwwlogs /www/server/certificates
    rm -rf /var/cache/nginx/fastcgi
    echo -e "  ${GREEN}✓${NC} 网站数据已删除"

    if grep -q "/swapfile" /etc/fstab 2>/dev/null; then
        echo -e "  → 清理 Swap 文件..."
        swapoff /swapfile 2>/dev/null || true
        rm -f /swapfile
        sed -i '/\/swapfile/d' /etc/fstab
        echo -e "  ${GREEN}✓${NC} Swap 已删除"
    fi

    echo ""
    log_info "彻底清理流程已完成；仅执行了上方明确列出的删除和卸载，不承诺恢复其他系统变更。"
}

# ============================================================
# 权限、平台与发布包安全预检
# ============================================================
if [[ $EUID -ne 0 ]]; then
    log_error "请使用 root 权限运行此脚本"
fi
assert_supported_platform
init_install_workdir
prepare_panel_candidate
exec 9>/run/lock/yub-wpanel-install.lock
flock -n 9 || log_error "另一个 YUB WPanel 安装或 repair 进程正在运行"
log_info "权限、Debian 13 amd64 平台与发布包安全预检通过"

# ============================================================
# 重复安装/残留安装检测
# ============================================================
INSTALL_COMPLETE=false
INSTALL_TRACES=false

if [[ -f "$CONFIG_FILE" ]] && [[ -s "$BIN_PATH" ]] && [[ -x "$BIN_PATH" ]]; then
    INSTALL_COMPLETE=true
fi

if [[ -e "$CONFIG_FILE" ]] || [[ -L "$CONFIG_FILE" ]] || \
   [[ -e "$BIN_PATH" ]] || [[ -L "$BIN_PATH" ]] || \
   [[ -d "$INSTALL_DIR" ]] || \
   [[ -e "$SERVICE_PATH" ]] || [[ -L "$SERVICE_PATH" ]] || \
   [[ -e "$CRON_PATH" ]] || [[ -L "$CRON_PATH" ]] || \
   [[ -e /etc/systemd/system/yub-wpanel.service.d ]] || [[ -L /etc/systemd/system/yub-wpanel.service.d ]] || \
   [[ -e /run/systemd/system/yub-wpanel.service.d ]] || [[ -L /run/systemd/system/yub-wpanel.service.d ]] || \
   [[ -e /etc/systemd/system.control/yub-wpanel.service.d ]] || [[ -L /etc/systemd/system.control/yub-wpanel.service.d ]] || \
   [[ -e /run/systemd/system.control/yub-wpanel.service.d ]] || [[ -L /run/systemd/system.control/yub-wpanel.service.d ]] || \
   [[ -e /run/systemd/transient/yub-wpanel.service ]] || [[ -L /run/systemd/transient/yub-wpanel.service ]]; then
    INSTALL_TRACES=true
fi

if $INSTALL_COMPLETE; then
    echo ""
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}  检测到 YUB WPanel 已安装${NC}"
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo -e "  1) 继续/修复安装（${GREEN}保留面板配置、凭据和TLS身份${NC}）"
    echo -e "  2) 卸载后重新安装（${YELLOW}重建面板身份和状态${NC}）"
    echo -e "  3) 仅卸载面板（${GREEN}删除面板状态；保留站点数据/数据库/软件${NC}）"
    echo -e "  4) 彻底清空（${RED}高风险：删除站点目录/配置并卸载共享软件${NC}）"
    echo -e "  5) 退出"
    echo ""
    echo -e "  输入数字后回车进行选择。"

    read -r -p "  > " choice < /dev/tty 2>/dev/null || read -r choice

    case "${choice:-5}" in
        1)
            REPAIR_MODE=true
            log_info "继续/修复安装：将保留现有面板身份和配置"
            ;;
        2)
            do_uninstall
            log_info "开始重新安装..."
            ;;
        3)
            do_uninstall
            exit 0
            ;;
        4)
            do_purge
            exit 0
            ;;
        *)
            echo -e "${GREEN}已取消，面板保持现有状态${NC}"
            exit 0
            ;;
    esac
elif $INSTALL_TRACES; then
    echo ""
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${YELLOW}  检测到 YUB WPanel 上次安装未完成或存在残留${NC}"
    echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    if [[ -f "$CONFIG_FILE" ]]; then
        echo -e "  1) 继续/修复安装（${GREEN}保留面板配置、凭据和TLS身份${NC}）"
        echo -e "  2) 清理面板残留后重新安装（${YELLOW}重建面板身份和状态${NC}）"
        echo -e "  3) 仅卸载面板残留（${GREEN}删除面板状态；保留站点数据/数据库/软件${NC}）"
        echo -e "  4) 彻底清空（${RED}高风险：删除站点目录/配置并卸载共享软件${NC}）"
        echo -e "  5) 退出"
        echo ""
        echo -e "  直接回车将继续/修复安装。"
    else
        echo -e "${RED}  检测到面板状态但缺少config.json，无法安全repair。${NC}"
        echo -e "  1) 清理面板残留后重新安装（${YELLOW}重建面板身份和状态${NC}）"
        echo -e "  2) 仅卸载面板残留（${GREEN}删除面板状态；保留站点数据/数据库/软件${NC}）"
        echo -e "  3) 彻底清空（${RED}高风险：删除站点目录/配置并卸载共享软件${NC}）"
        echo -e "  4) 退出"
    fi

    read -r -p "  > " choice < /dev/tty 2>/dev/null || read -r choice

    if [[ -f "$CONFIG_FILE" ]]; then
        case "${choice:-1}" in
            1) REPAIR_MODE=true; log_info "继续/修复安装：将保留现有面板身份和配置" ;;
            2) do_uninstall; log_info "开始重新安装..." ;;
            3) do_uninstall; exit 0 ;;
            4) do_purge; exit 0 ;;
            *) echo -e "${GREEN}已取消，系统保持现有状态${NC}"; exit 0 ;;
        esac
    else
        case "${choice:-4}" in
            1) do_uninstall; log_info "开始重新安装..." ;;
            2) do_uninstall; exit 0 ;;
            3) do_purge; exit 0 ;;
            *) echo -e "${GREEN}已取消，系统保持现有状态${NC}"; exit 0 ;;
        esac
    fi
fi

if $REPAIR_MODE; then
    prepare_panel_candidate
    verify_complete_release_bundle || \
        log_error "repair执行前面板与许可发布包完整性复核失败"
    repair_check=$($PANEL_CANDIDATE --repair-config-check --config "$CONFIG_FILE") || \
        log_error "现有config.json未通过repair安全校验，未修改服务器状态"
    validate_existing_panel_binary || \
        log_error "现有面板二进制不是root持有的单链接常规0755文件；未修改服务器状态"
    validate_repair_service_unit || \
        log_error "现有systemd unit不是YUB WPanel生成的精确安全版本，或存在drop-in；未修改服务器状态"
    validate_existing_panel_cron_file || \
        log_error "现有cron文件不是root安全持有的YUB WPanel受管文件；未修改服务器状态"
    case "$repair_check" in
        *'"tls_action":"preserve"'*) REPAIR_TLS_ACTION="preserve" ;;
        *'"tls_action":"generate"'*) REPAIR_TLS_ACTION="generate" ;;
        *) log_error "repair配置检查返回未知TLS状态" ;;
    esac
    case "$repair_check" in
        *'"tls_certificate_expired"'*) log_warn "现有面板TLS证书已过期；repair将保留证书身份，请另行更新" ;;
        *'"tls_certificate_expires_soon"'*) log_warn "现有面板TLS证书将在30天内到期；repair将保留证书身份" ;;
    esac
    if ! command -v sqlite3 >/dev/null 2>&1; then
        log_warn "repair需要sqlite3创建面板数据库在线备份，正在自动安装"
        command -v apt-get >/dev/null 2>&1 || log_error "repair缺少apt-get，无法自动安装sqlite3"
        DEBIAN_FRONTEND=noninteractive apt-get install -y sqlite3 || \
            log_error "repair自动安装sqlite3失败，请检查APT后重试"
        command -v sqlite3 >/dev/null 2>&1 || log_error "repair安装sqlite3后仍无法找到该命令"
    fi
    if systemctl is-active --quiet yub-wpanel 2>/dev/null; then
        REPAIR_SERVICE_WAS_ACTIVE=true
    fi
    create_repair_backup
    log_info "repair预检与备份完成"
else
    if [[ -e "$SERVICE_PATH" ]] || [[ -L "$SERVICE_PATH" ]]; then
        log_error "fresh安装前仍存在systemd unit或链接，拒绝继续"
    fi
	validate_fresh_panel_cron_location || \
		log_error "fresh安装前cron父目录或现有cron文件身份不安全，拒绝继续"
    if [[ -e "$CRON_PATH" ]] || [[ -L "$CRON_PATH" ]]; then
        log_error "fresh安装前仍存在cron文件或链接，拒绝继续"
    fi
    validate_no_panel_service_dropins || \
        log_error "检测到会影响yub-wpanel.service的遗留、层级或全局systemd drop-in；未修改服务器状态"
fi

# ============================================================
# 系统检测与Swap配置
# ============================================================
TOTAL_MEM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
TOTAL_MEM_MB=$((TOTAL_MEM_KB / 1024))
log_info "物理内存: ${TOTAL_MEM_MB}MB"

if ! $REPAIR_MODE && [[ $TOTAL_MEM_MB -le 8192 ]]; then
    SWAP_FILE="/swapfile"
    SWAP_SIZE_BYTES=$((2 * 1024 * 1024 * 1024))
    ROOT_STATS=$(df -P -B1 / | awk 'NR == 2 {print $2, $3, $4}')
    read -r ROOT_TOTAL ROOT_USED ROOT_AVAILABLE <<< "$ROOT_STATS"

    if awk 'NR > 1 && NF {found=1} END {exit !found}' /proc/swaps; then
        log_info "系统已有启用的 Swap，跳过自动创建"
    elif [[ -e "$SWAP_FILE" ]]; then
        log_warn "${SWAP_FILE} 已存在但未启用，跳过自动创建"
    elif [[ -z "$ROOT_TOTAL" || -z "$ROOT_USED" || -z "$ROOT_AVAILABLE" ]]; then
        log_warn "无法读取根分区空间，跳过自动创建 Swap"
    elif [[ $ROOT_TOTAL -le 0 ]]; then
        log_warn "根分区总容量异常，跳过自动创建 Swap"
    elif [[ $ROOT_AVAILABLE -lt $((8 * 1024 * 1024 * 1024)) ]]; then
        log_warn "根分区可用空间不足 8GB，跳过自动创建 Swap"
    elif [[ $(((ROOT_USED + SWAP_SIZE_BYTES) * 100 / ROOT_TOTAL)) -gt 85 ]]; then
        log_warn "创建 Swap 后根分区使用率将超过 85%，跳过自动创建"
    else
        log_info "创建 2GB Swap 安全缓冲..."
        if dd if=/dev/zero of="$SWAP_FILE" bs=1M count=2048 status=progress &&
           chmod 600 "$SWAP_FILE" &&
           mkswap "$SWAP_FILE" &&
           swapon "$SWAP_FILE"; then
            if {
                echo ""
                echo "# YUB WPanel managed swap"
                echo "$SWAP_FILE none swap sw 0 0"
            } >> /etc/fstab; then
                if ! cat > /etc/sysctl.d/99-yub-wpanel-swap.conf << 'SWAPSYSCTLEOF'
# YUB WPanel managed swap
vm.swappiness = 10
SWAPSYSCTLEOF
                then
                    log_warn "Swap 已启用，但写入 vm.swappiness 配置失败"
                else
                    sysctl -p /etc/sysctl.d/99-yub-wpanel-swap.conf >/dev/null 2>&1 || \
                        log_warn "Swap 已启用，但应用 vm.swappiness=10 失败"
                fi
                log_info "2GB Swap 创建完成"
            else
                swapoff "$SWAP_FILE" 2>/dev/null || true
                rm -f "$SWAP_FILE"
                log_warn "写入 /etc/fstab 失败，已回滚 Swap 并继续安装"
            fi
        else
            swapoff "$SWAP_FILE" 2>/dev/null || true
            rm -f "$SWAP_FILE"
            log_warn "Swap 创建失败，已清理临时文件并继续安装"
        fi
    fi
fi

# ============================================================
# APT 源配置
# ============================================================
log_info "配置 APT 源..."
export DEBIAN_FRONTEND=noninteractive
DEBIAN_CODENAME=""
if command -v lsb_release &>/dev/null; then
    DEBIAN_CODENAME=$(lsb_release -sc 2>/dev/null || true)
fi
if [[ -z "$DEBIAN_CODENAME" ]] && [[ -f /etc/os-release ]]; then
    DEBIAN_CODENAME=$(grep '^VERSION_CODENAME=' /etc/os-release 2>/dev/null | cut -d= -f2 || true)
fi
if [[ -z "$DEBIAN_CODENAME" ]]; then
    log_error "无法识别 Debian 版本代号"
fi
log_info "Debian 版本: ${DEBIAN_CODENAME}"

# 国内模式会优先选择 Debian 镜像，并同时覆盖 debian-security / debian-updates。
if ! $REPAIR_MODE; then
select_debian_source "$DEBIAN_CODENAME"

# 安装基础依赖
apt-get install -y curl wget unzip ca-certificates gnupg lsb-release

# PHP 8.3 源单独做多重兜底，国内模式优先中科大 / 上交镜像。
select_php_source "$DEBIAN_CODENAME"

# ============================================================
# 安装基础组件
# ============================================================
log_info "安装系统组件..."

apt-get install -y \
    nginx \
    mariadb-server \
    redis-server \
    fail2ban \
    nftables \
    sshpass \
    rsyslog \
    cron \
    php8.3-fpm \
    php8.3-mysql \
    php8.3-curl \
    php8.3-gd \
    php8.3-exif \
    jpegoptim \
    optipng \
    php8.3-mbstring \
    php8.3-xml \
    php8.3-zip \
    php8.3-intl \
    php8.3-redis \
    php8.3-opcache \
    php8.3-cli

log_info "基础组件安装完成"
else
    log_info "repair模式保留APT源和现有软件包，不执行安装或升级"
fi

if ! $REPAIR_MODE; then
	validate_existing_panel_cron_file || \
		log_error "cron安装后 /etc/cron.d 身份不安全，拒绝继续"
fi

# ============================================================
# systemd 进程守护配置
# ============================================================
log_info "配置 systemd 进程守护..."

if ! $REPAIR_MODE; then
for svc in nginx php8.3-fpm mariadb redis-server; do
    DROPDIR="/etc/systemd/system/${svc}.service.d"
    mkdir -p "$DROPDIR"
    cat > "$DROPDIR/yub-wpanel.conf" << SYSTEMDEOF
[Unit]
StartLimitIntervalSec=0

[Service]
Restart=always
RestartSec=5s
SYSTEMDEOF
done

systemctl daemon-reload
log_info "systemd 进程守护配置完成"

systemctl_start_required php8.3-fpm
systemctl_start_required nginx
else
    log_info "repair模式保留Nginx、PHP-FPM、MariaDB和Redis的systemd配置与状态"
fi

# ============================================================
# Nginx 基础配置
# ============================================================
log_info "配置 Nginx 基础..."

if ! $REPAIR_MODE; then
mkdir -p /etc/nginx/conf.d

cat > /etc/nginx/conf.d/yubwpanel-ratelimit.conf << 'RATELIMITEOF'
# YUB WPanel — 请求频率限制
# 已登录 WordPress 用户不限速
map $http_cookie $wp_rate_limit_key {
    ~*wordpress_logged_in "";
    default $binary_remote_addr;
}

limit_req_zone $wp_rate_limit_key zone=wp_req_limit:10m rate=60r/m;
RATELIMITEOF

cat > /etc/nginx/conf.d/yubwpanel-limit-status.conf << 'LIMITSTATUSEOF'
# YUB WPanel Generated - shared limit_req status
limit_req_status 429;
LIMITSTATUSEOF

# FastCGI 缓存
mkdir -p /var/cache/nginx/fastcgi
cat > /etc/nginx/conf.d/yubwpanel-cache.conf << 'CACHEEOF'
fastcgi_cache_path /var/cache/nginx/fastcgi levels=1:2 keys_zone=WP_CACHE:200m inactive=60m max_size=2g;
CACHEEOF

nginx -t && nginx -s reload 2>/dev/null || true
log_info "Nginx 基础配置完成"
else
    log_info "repair模式不改写或reload Nginx配置"
fi

# ============================================================
# 防火墙放行 8443 面板端口
# ============================================================
log_info "放行面板端口 8443..."

if ! $REPAIR_MODE; then
# nftables
if command -v nft &>/dev/null && nft list ruleset 2>/dev/null | grep -q "hook input"; then
    nft add rule inet filter input tcp dport 8443 accept 2>/dev/null || \
    nft add rule ip filter input tcp dport 8443 accept 2>/dev/null || true
    log_info "nftables 已放行 8443"
fi

# ufw
if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow 8443/tcp 2>/dev/null || true
    log_info "ufw 已放行 8443"
fi
else
    log_info "repair模式保留现有防火墙规则"
fi

# ============================================================
# MariaDB 安全加固
# ============================================================
log_info "配置 MariaDB..."

if ! $REPAIR_MODE; then
    systemctl_start_required mariadb
    systemctl_enable_best_effort mariadb
fi

if $REPAIR_MODE; then
    log_info "repair模式保留现有MariaDB身份与配置"
else
    if [[ -z "$MYSQL_PASS" ]]; then
        MYSQL_PASS=$(head -c 24 /dev/urandom | sha256sum | head -c 32)
    fi

    if mysql -u root -p"${MYSQL_PASS}" -e "SELECT 1" 2>/dev/null; then
        log_info "MariaDB root 密码已验证"
    elif mysql -u root -e "SELECT 1" 2>/dev/null; then
        mysqladmin -u root password "${MYSQL_PASS}" 2>/dev/null
        log_info "MariaDB root 密码已设置"
    else
        log_warn "MariaDB 密码状态异常，面板首次启动时将自动修复"
    fi

    mysql -u root -p"${MYSQL_PASS}" -e "
        DELETE FROM mysql.user WHERE User='';
        DELETE FROM mysql.user WHERE User='root' AND Host!='localhost';
        DROP DATABASE IF EXISTS test;
        DELETE FROM mysql.db WHERE Db='test' OR Db='test\\_%';
        FLUSH PRIVILEGES;
    " 2>/dev/null || log_warn "部分安全加固跳过(密码可能已设置)"

    if [[ $TOTAL_MEM_MB -le 1024 ]]; then
        log_info "低内存环境，优化 MariaDB 配置..."
        cat > /etc/mysql/mariadb.conf.d/99-yub-wpanel.cnf << 'MARIADBEOF'
[mysqld]
innodb_buffer_pool_size = 128M
innodb_log_buffer_size = 8M
table_open_cache = 128
max_connections = 30
performance_schema = OFF
MARIADBEOF
        systemctl restart mariadb || systemctl_start_required mariadb
    fi
fi

# ============================================================
# 目录结构创建
# ============================================================
log_info "创建目录结构..."

mkdir -p "$INSTALL_DIR"/{backups,packages,logs,certs}
mkdir -p /www/wwwroot
mkdir -p /www/wwwlogs
mkdir -p /www/server/certificates
chmod 700 "$INSTALL_DIR"

# ============================================================
# 生成自签名 SSL 证书（有效期 10 年）
# ============================================================
log_info "检查面板 TLS 证书..."

CERT_DIR="$INSTALL_DIR/certs"
CERT_FILE="$CERT_DIR/panel.crt"
KEY_FILE="$CERT_DIR/panel.key"

if $REPAIR_MODE && [[ "$REPAIR_TLS_ACTION" == "preserve" ]]; then
    log_info "repair模式保留现有TLS证书与私钥"
else
    TLS_TMP_DIR="$INSTALL_WORKDIR/tls"
    install -d -m 0700 "$TLS_TMP_DIR"
    openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
        -keyout "$TLS_TMP_DIR/panel.key" \
        -out "$TLS_TMP_DIR/panel.crt" \
        -subj "/C=CN/O=YUB WPanel/OU=Panel/CN=YUB-WPanel-SelfSigned" \
        -addext "subjectAltName=IP:127.0.0.1" \
        2>/dev/null
    openssl x509 -in "$TLS_TMP_DIR/panel.crt" -noout >/dev/null
    openssl pkey -in "$TLS_TMP_DIR/panel.key" -noout >/dev/null
    chmod 600 "$TLS_TMP_DIR/panel.key"
    chmod 644 "$TLS_TMP_DIR/panel.crt"
    if $REPAIR_MODE; then REPAIR_MUTATED=true; fi
    mv "$TLS_TMP_DIR/panel.key" "$KEY_FILE"
    mv "$TLS_TMP_DIR/panel.crt" "$CERT_FILE"
    rmdir "$TLS_TMP_DIR"
    log_info "自签名证书已生成（有效期 10 年）"
fi

# ============================================================
# 下载 WordPress 备用包
# ============================================================
log_info "检查 WordPress 备用包..."
WP_ZIP="$INSTALL_DIR/packages/wordpress.zip"
WP_ZIP_TMP="$INSTALL_WORKDIR/wordpress.zip"
if $REPAIR_MODE && file_size_within_limit "$WP_ZIP" "$WORDPRESS_ZIP_MAX_BYTES"; then
    log_info "repair模式保留现有WordPress备用包"
else
    if [[ -e "$WP_ZIP" ]] || [[ -L "$WP_ZIP" ]]; then
        log_warn "现有WordPress备用包不是安全常规文件或超过大小上限，正在替换"
        rm -f -- "$WP_ZIP"
    fi
    for i in 1 2 3; do
        if download_file "https://wordpress.org/latest.zip" "$WP_ZIP_TMP" 60 "$WORDPRESS_ZIP_MAX_BYTES"; then
            mv "$WP_ZIP_TMP" "$WP_ZIP"
            log_info "WordPress 下载完成"
            break
        fi
        log_warn "下载失败，重试 ($i/3)..."
        sleep 3
    done
    rm -f "$WP_ZIP_TMP"
    if [[ ! -s "$WP_ZIP" ]]; then
        rm -f "$WP_ZIP"
        log_warn "WordPress 下载失败，将在首次建站时使用联网下载"
    fi
fi

# ============================================================
# 生成面板安全凭证
# ============================================================
log_info "检查安全凭证..."

if ! $REPAIR_MODE; then
PANEL_SUFFIX=$(head -c 20 /dev/urandom | sha256sum | head -c 8)

BASIC_USER="admin"
BASIC_PASS=$(head -c 12 /dev/urandom | base64 | head -c 16)
WEB_USER="wpadmin"
WEB_PASS=$(head -c 12 /dev/urandom | base64 | head -c 16)

BASIC_HASH=""
WEB_HASH=""
if command -v php8.3 &>/dev/null; then
    BASIC_HASH=$(php8.3 -r "echo password_hash('$BASIC_PASS', PASSWORD_BCRYPT, ['cost' => 12]);" 2>/dev/null)
    WEB_HASH=$(php8.3 -r "echo password_hash('$WEB_PASS', PASSWORD_BCRYPT, ['cost' => 12]);" 2>/dev/null)
fi
if [[ -z "$BASIC_HASH" ]] && command -v python3 &>/dev/null; then
    BASIC_HASH=$(python3 -c "import bcrypt; print(bcrypt.hashpw(b'$BASIC_PASS', bcrypt.gensalt(12)).decode())" 2>/dev/null)
    WEB_HASH=$(python3 -c "import bcrypt; print(bcrypt.hashpw(b'$WEB_PASS', bcrypt.gensalt(12)).decode())" 2>/dev/null)
fi
if [[ -z "$BASIC_HASH" ]]; then
    log_warn "无法生成 bcrypt 哈希，面板首次启动时将自动重置密码"
    # Literal invalid bcrypt fallbacks: dollar signs must not expand.
    # shellcheck disable=SC2016
    BASIC_HASH='$2a$12$00000000000000000000000000000000000000000000000000000'
    # shellcheck disable=SC2016
    WEB_HASH='$2a$12$00000000000000000000000000000000000000000000000000000'
fi
else
    log_info "repair模式保留现有登录凭据和安全入口"
fi

# ============================================================
# 写入 config.json
# ============================================================
log_info "检查配置文件..."

if ! $REPAIR_MODE; then
cat > "$CONFIG_FILE" << CONFIGEOF
{
  "panel": {
    "version": "1.0.0-mvp",
    "port": $PANEL_PORT,
    "tls_port": 8443,
    "tls_cert_path": "$CERT_FILE",
    "tls_key_path": "$KEY_FILE",
    "random_suffix": "$PANEL_SUFFIX",
    "data_dir": "$INSTALL_DIR",
    "backup_dir": "$INSTALL_DIR/backups",
    "log_dir": "$INSTALL_DIR/logs"
  },
  "sqlite": {
    "path": "$DB_PATH"
  },
  "mariadb": {
    "host": "localhost",
    "port": 3306,
    "socket": "/run/mysqld/mysqld.sock",
    "root_user": "root",
    "root_password": "$MYSQL_PASS"
  },
  "admin": {
    "username": "$WEB_USER",
    "password_hash": "$WEB_HASH"
  },
  "basic_auth": {
    "username": "$BASIC_USER",
    "password_hash": "$BASIC_HASH"
  },
  "paths": {
    "www_root": "/www/wwwroot",
    "www_logs": "/www/wwwlogs",
    "nginx_sites_available": "/etc/nginx/sites-available",
    "nginx_sites_enabled": "/etc/nginx/sites-enabled",
    "php_fpm_pool": "/etc/php/8.3/fpm/pool.d",
    "php_fpm_sock": "/run/php",
    "certificates": "/www/server/certificates",
    "wordpress_package": "$INSTALL_DIR/packages/wordpress.zip",
    "cron_file": "/etc/cron.d/yub_wpanel_cron"
  },
  "security": {
    "basic_auth_enabled": true,
    "max_login_attempts": 5,
    "attempt_window_minutes": 5,
    "ban_duration_hours": 24,
    "auto_whitelist_enabled": true,
    "core_ports": [22, $PANEL_PORT, 80, 443, 8443]
  },
  "systemd": {
    "service_name": "yub-wpanel",
    "service_path": "$SERVICE_PATH",
    "binary_path": "$BIN_PATH"
  }
}
CONFIGEOF

chmod 600 "$CONFIG_FILE"
else
    log_info "repair模式保持config.json字节不变"
fi

# ============================================================
# 部署 Go 二进制
# ============================================================
log_info "部署面板二进制..."

prepare_panel_candidate
verify_complete_release_bundle || \
    log_error "部署前面板与许可发布包完整性复核失败"
if $REPAIR_MODE; then REPAIR_MUTATED=true; fi
atomic_install_managed_file "$PANEL_CANDIDATE" "$BIN_PATH" 0755 || \
	log_error "面板二进制同目录原子部署失败"
log_info "面板二进制已原子部署"
install_release_license_documentation || \
    log_error "无法安全安装已验签的 YUB WPanel 许可文档"
log_info "许可文档已安装到 $LICENSE_DOC_DIR"

# ============================================================
# 创建 systemd 服务
# ============================================================
log_info "检查 systemd 服务..."

if ! $REPAIR_MODE || [[ ! -f "$SERVICE_PATH" ]]; then
if $REPAIR_MODE; then REPAIR_MUTATED=true; fi
[[ ! -e "$SERVICE_PATH" ]] && [[ ! -L "$SERVICE_PATH" ]] || \
    log_error "写入systemd unit前目标已存在或为链接"
write_panel_service_unit "$SERVICE_PATH" 0644 || log_error "写入yub-wpanel systemd unit失败"
else
    log_info "repair模式保留现有systemd unit"
fi

systemctl daemon-reload
validate_effective_panel_service_unit || \
    log_error "systemd实际加载的unit、drop-in或ExecStart与YUB WPanel不一致"
if $REPAIR_MODE; then
    if $REPAIR_SERVICE_WAS_ACTIVE; then
        systemctl restart yub-wpanel || log_error "yub-wpanel重启失败"
    else
        log_info "repair前yub-wpanel未运行，保持inactive状态"
    fi
else
    systemctl_enable_best_effort yub-wpanel
    systemctl_start_required yub-wpanel
    validate_running_panel_service_identity || \
        log_error "yub-wpanel MainPID未运行已部署的YUB WPanel二进制"
    apply_system_tuning
fi

# ============================================================
# 端口监听检测
# ============================================================
PORT_OK=false
if systemctl is-active --quiet yub-wpanel; then
    sleep 3
    for i in 1 2 3 4 5 6 7 8; do
        if ss -tlnp 2>/dev/null | grep -q ":8443"; then
            PORT_OK=true
            break
        fi
        sleep 2
    done
fi

# ============================================================
# 最终输出
# ============================================================
if systemctl is-active --quiet yub-wpanel; then
    STATUS="${GREEN}运行中${NC}"
else
    STATUS="${RED}未运行${NC}"
fi

if $REPAIR_MODE; then
    if $REPAIR_SERVICE_WAS_ACTIVE; then
        systemctl is-active --quiet yub-wpanel || log_error "repair后面板服务未运行"
        validate_running_panel_service_identity || \
            log_error "repair后yub-wpanel MainPID未运行已部署的YUB WPanel二进制"
    elif systemctl is-active --quiet yub-wpanel; then
        log_error "repair改变了面板服务原始inactive状态"
    fi
    "$BIN_PATH" --repair-config-check --config "$CONFIG_FILE" >/dev/null || \
        log_error "repair后配置复核失败"
    REPAIR_COMMITTED=true
fi

LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[[ -z "$LOCAL_IP" ]] && LOCAL_IP="<未知>"

PUBLIC_IP_FILE="$INSTALL_WORKDIR/public-ip.txt"
if download_file "https://ip.sb" "$PUBLIC_IP_FILE" 15 "$PUBLIC_IP_MAX_BYTES" || \
   download_file "https://ifconfig.me/ip" "$PUBLIC_IP_FILE" 15 "$PUBLIC_IP_MAX_BYTES"; then
    PUBLIC_IP=$(tr -d '\r\n' < "$PUBLIC_IP_FILE")
else
    PUBLIC_IP=""
fi
if [[ ${#PUBLIC_IP} -gt 45 ]] || [[ ! "$PUBLIC_IP" =~ ^[0-9A-Fa-f:.]+$ ]]; then
    PUBLIC_IP=""
fi
[[ -z "$PUBLIC_IP" ]] && PUBLIC_IP="<未知>"

echo ""
echo -e "${BOLD}============================================${NC}"
echo -e "${BOLD}  YUB WPanel 安装完成 / Installation Complete${NC}"
echo -e "${BOLD}============================================${NC}"
echo ""
echo -e "${BOLD}官方来源 / Official Sources:${NC}"
echo -e "  项目主页 / Project: ${BOLD}https://github.com/zangwp/yub-wpanel${NC}"
echo -e "  其他域名均非本项目官网，与本项目无关。"
echo -e "  Other domains are not official YUB WPanel websites."
echo ""
echo -e "公网 IP / Public IP:  ${BOLD}${PUBLIC_IP}${NC}"
echo -e "内网 IP / Private IP: ${BOLD}${LOCAL_IP}${NC}"
echo ""
if $REPAIR_MODE; then
    echo -e "面板地址和两层登录凭据已保持不变，本次repair不重新显示秘密。"
    echo -e "Panel URL and both login credentials are unchanged. Repair mode does not print secrets again."
elif [[ "$PUBLIC_IP" != "<未知>" ]]; then
    echo -e "面板地址 / Panel URL: ${BOLD}https://${PUBLIC_IP}:8443/${PANEL_SUFFIX}/${NC}"
    if [[ "$LOCAL_IP" != "<未知>" && "$LOCAL_IP" != "$PUBLIC_IP" ]]; then
        echo -e "内网地址 / LAN URL:   ${BOLD}https://${LOCAL_IP}:8443/${PANEL_SUFFIX}/${NC}"
    fi
else
    echo -e "面板地址 / Panel URL: ${BOLD}https://${LOCAL_IP}:8443/${PANEL_SUFFIX}/${NC}"
fi
echo -e "面板状态 / Panel Status: ${STATUS}"
if $PORT_OK; then
    echo -e "端口监听 / Port:         ${GREEN}8443 is listening${NC}"
else
    echo -e "端口监听 / Port:         ${YELLOW}8443 is not listening. Check logs: journalctl -u yub-wpanel -n 20${NC}"
fi
if ! $REPAIR_MODE; then
    echo ""
    echo -e "  ┌─────────────────────────────────────────┐"
    echo -e "  │  第 1 层 / Layer 1 — BasicAuth          │"
    echo -e "  ├─────────────────────────────────────────┤"
    echo -e "  │  用户名 / Username: ${BOLD}${BASIC_USER}${NC}"
    echo -e "  │  密码 / Password:   ${BOLD}${BASIC_PASS}${NC}"
    echo -e "  └─────────────────────────────────────────┘"
    echo ""
    echo -e "  ┌─────────────────────────────────────────┐"
    echo -e "  │  第 2 层 / Layer 2 — Web Login          │"
    echo -e "  ├─────────────────────────────────────────┤"
    echo -e "  │  用户名 / Username: ${BOLD}${WEB_USER}${NC}"
    echo -e "  │  密码 / Password:   ${BOLD}${WEB_PASS}${NC}"
    echo -e "  └─────────────────────────────────────────┘"
    echo ""
    echo -e "  ${BOLD}登录流程 / Login Steps:${NC}"
    echo -e "  1. 打开上方地址，输入第 1 层 BasicAuth 凭据"
    echo -e "     Open the URL above and enter the Layer 1 BasicAuth credentials"
    echo -e "  2. 进入登录页后，输入第 2 层 Web 登录凭据"
    echo -e "     On the login page, enter the Layer 2 Web Login credentials"
    echo -e "  3. 进入控制台 / Enter the dashboard"
fi
echo ""
echo -e "${YELLOW}⚠ 安装器默认生成自签名证书；如浏览器显示证书警告，不要盲目点击继续。${NC}"
echo -e "${YELLOW}  The installer creates a self-signed certificate by default. Do not bypass a browser warning without verifying it.${NC}"
echo -e "${YELLOW}  请先通过 SSH 在服务器执行下列命令，再与浏览器显示的 SHA-256 证书指纹逐字比对：${NC}"
echo -e "  ${BOLD}openssl x509 -in ${CERT_FILE} -noout -fingerprint -sha256${NC}"
echo -e "${YELLOW}  指纹不一致时立即停止，不要输入任何凭据。${NC}"
echo -e "${YELLOW}  Verify the SHA-256 fingerprint over SSH against the browser. Stop immediately if it differs.${NC}"
echo -e "${YELLOW}  长期公网使用请替换为由受信任 CA 签发、且与访问域名匹配的证书。${NC}"
echo -e "${YELLOW}  For long-term public access, replace it with a trusted CA certificate matching the panel hostname.${NC}"
echo -e "${YELLOW}  面板使用 8443 端口（HTTPS），与 Nginx 网站 443 端口不冲突${NC}"
echo -e "${YELLOW}  The panel uses HTTPS port 8443 and does not conflict with Nginx port 443.${NC}"
echo ""
echo -e "${BOLD}无法访问？ / Cannot Access?${NC}"
echo -e "  1. 云服务器请检查安全组/防火墙是否放行 8443 端口"
echo -e "     For cloud servers, allow port 8443 in the security group/firewall"
echo -e "  2. 检查本地防火墙 / Check local firewall: ${BOLD}nft list ruleset${NC}"
echo -e "  3. 查看面板日志 / View panel logs: ${BOLD}journalctl -u yub-wpanel -f${NC}"
echo ""
echo -e "${BOLD}软件安装路径 / Installed Paths:${NC}"
echo -e "  Nginx:      /etc/nginx/"
echo -e "  PHP-FPM:    /etc/php/8.3/fpm/"
echo -e "  MariaDB:    /etc/mysql/"
echo -e "  Redis:      /etc/redis/"
echo -e "  面板程序 / Panel binary: /usr/local/bin/yub-wpanel"
echo -e "  面板数据 / Panel data:   /www/server/panel/"
echo -e "  SSL 证书 / SSL certs:    ${CERT_DIR}/"
echo ""
echo -e "${BOLD}面板 CLI (yubw):${NC}"
echo -e "  yubw              查看面板信息 / Show panel info"
echo -e "  yubw restart      重启面板 / Restart panel"
echo -e "  yubw password     一键重置管理员密码 / Reset admin password"
echo -e "  yubw unban        一键清空所有IP封禁 / Clear all IP bans"
echo -e "  yubw status       查看运行状态 / Show runtime status"
echo ""
if ! $REPAIR_MODE; then
    echo -e "${YELLOW}请立即保存以上凭据，此信息仅显示一次${NC}"
    echo -e "${YELLOW}Save these credentials now. They are shown only once.${NC}"
fi
echo ""
echo -e "${BOLD}可选运行统计 / Optional Runtime Telemetry${NC}"
echo -e "  YUB WPanel 默认关闭可选运行统计。"
echo -e "  Optional runtime telemetry is disabled by default in YUB WPanel."
echo ""
