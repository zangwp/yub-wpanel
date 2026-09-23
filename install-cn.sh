#!/bin/bash
set -e
set -o pipefail

# ============================================================
# YUB WPanel 签名引导脚本
# 主安装逻辑统一维护在 install.sh。
# install-cn.sh 默认启用国内优先策略；Release 同时从本文件生成
# bootstrap.sh，并将默认策略切换为全球直连。
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

BOOTSTRAP_RELEASE_VERSION="__YUB_WPANEL_RELEASE_VERSION__"
BOOTSTRAP_DEFAULT_PREFER_CN=1
GITHUB_INSTALL_URL="https://github.com/zangwp/yub-wpanel/releases/download/${BOOTSTRAP_RELEASE_VERSION}/install.sh"
CUSTOM_PROXY="${YUB_WPANEL_GITHUB_PROXY:-}"
CUSTOM_PROXY="${CUSTOM_PROXY%/}"
RELEASE_PUBLIC_KEY_HEX="7351099720eeaf147f4894bc313a5456c01bbd29ad7d401ab6869bc7f7f92af5"
INSTALLER_ASSET_MAX_BYTES=$((4 * 1024 * 1024))
CHECKSUM_ASSET_MAX_BYTES=$((4 * 1024))
SIGNATURE_ASSET_MAX_BYTES=64
BOOTSTRAP_CHECK_PLATFORM_ONLY=false
RELEASE_PUBLIC_KEY_PEM='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=
-----END PUBLIC KEY-----'

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

for bootstrap_arg in "$@"; do
    case "$bootstrap_arg" in
        --check-platform) BOOTSTRAP_CHECK_PLATFORM_ONLY=true ;;
    esac
done

assert_bootstrap_platform() {
    local os_id=""
    local version_id=""
    local codename=""
    local machine=""
    local dpkg_arch=""
    local required_cmd=""

    [[ $EUID -eq 0 ]] || log_error "请使用 root 权限运行此脚本"
    for required_cmd in dpkg head sed tr uname; do
        command -v "$required_cmd" >/dev/null 2>&1 || log_error "缺少平台检测命令: $required_cmd"
    done
    [[ -r /etc/os-release ]] || log_error "无法读取 /etc/os-release；仅支持 Debian 13 或 Ubuntu 24.04 LTS（amd64/arm64）"
    os_id=$(sed -n 's/^ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    version_id=$(sed -n 's/^VERSION_ID=//p' /etc/os-release | head -n 1 | tr -d '"')
    codename=$(sed -n 's/^VERSION_CODENAME=//p' /etc/os-release | head -n 1 | tr -d '"')
    machine=$(uname -m 2>/dev/null || true)
    dpkg_arch=$(dpkg --print-architecture 2>/dev/null || true)
    case "${os_id}:${version_id}:${codename}" in
        debian:13:trixie|ubuntu:24.04:noble) ;;
        *) log_error "仅支持 Debian 13 (trixie) 或 Ubuntu 24.04 LTS (noble)，当前系统: ${os_id:-unknown} ${version_id:-unknown} ${codename:-unknown}" ;;
    esac
    case "$machine" in
        x86_64|amd64) machine="amd64" ;;
        aarch64|arm64) machine="arm64" ;;
        *) log_error "仅支持 amd64/x86_64 或 arm64/aarch64，检测到架构: ${machine:-unknown}" ;;
    esac
    case "$dpkg_arch" in
        amd64|arm64) ;;
        *) log_error "仅支持 amd64 或 arm64 用户空间，检测到 dpkg 架构: ${dpkg_arch:-unknown}" ;;
    esac
    [[ "$machine" == "$dpkg_arch" ]] || \
        log_error "内核架构 ${machine} 与 dpkg 用户空间架构 ${dpkg_arch} 不一致，拒绝安装"
}

ensure_bootstrap_dependencies() {
    local -a packages=()

    command -v apt-get >/dev/null 2>&1 || log_error "缺少 apt-get；仅支持 Debian 13 或 Ubuntu 24.04 LTS"
    if ! dpkg-query -W -f='${Status}' ca-certificates 2>/dev/null | grep -Fqx 'install ok installed'; then
        packages+=(ca-certificates)
    fi
    command -v openssl >/dev/null 2>&1 || packages+=(openssl)
    if ! command -v wget >/dev/null 2>&1 && ! command -v curl >/dev/null 2>&1; then
        packages+=(curl)
    fi
    command -v awk >/dev/null 2>&1 || packages+=(mawk)
    command -v grep >/dev/null 2>&1 || packages+=(grep)
    for required_cmd in chmod head install mktemp rm sha256sum stat timeout tr wc; do
        command -v "$required_cmd" >/dev/null 2>&1 || {
            packages+=(coreutils)
            break
        }
    done
    [[ "${#packages[@]}" -eq 0 ]] && return 0

    log_info "正在安装引导程序依赖: ${packages[*]}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends "${packages[@]}"
}

case "${YUB_WPANEL_PREFER_CN_MIRROR:-$BOOTSTRAP_DEFAULT_PREFER_CN}" in
    1|true) export YUB_WPANEL_PREFER_CN_MIRROR=1 ;;
    0|false) export YUB_WPANEL_PREFER_CN_MIRROR=0 ;;
    *) log_error "YUB_WPANEL_PREFER_CN_MIRROR 只能是 0、1、true 或 false" ;;
esac
[[ "$BOOTSTRAP_RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
    log_error "入口脚本缺少规范的固定 Release 版本；请使用已签名 GitHub Release 资产"
assert_bootstrap_platform
ensure_bootstrap_dependencies
if [[ "$BOOTSTRAP_CHECK_PLATFORM_ONLY" == true ]]; then
    log_info "YUB WPanel 引导程序平台检查通过"
    exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"

CN_WORKDIR=""
cleanup_cn_workdir() {
    [[ -n "$CN_WORKDIR" ]] || return 0
    case "$CN_WORKDIR" in
        /tmp/yub-wpanel-cn.??????????)
            [[ -d "$CN_WORKDIR" ]] && rm -rf -- "$CN_WORKDIR"
            ;;
        *) log_warn "拒绝清理异常临时目录路径: $CN_WORKDIR" ;;
    esac
}
trap cleanup_cn_workdir EXIT

for REQUIRED_CMD in awk bash chmod grep head install mktemp openssl rm sha256sum stat timeout tr wc; do
    command -v "$REQUIRED_CMD" >/dev/null 2>&1 || log_error "缺少入口脚本必要命令: $REQUIRED_CMD"
done
if ! command -v wget >/dev/null 2>&1 && ! command -v curl >/dev/null 2>&1; then
    log_error "缺少下载工具：请先安装 wget 或 curl"
fi

PREVIOUS_UMASK=$(umask)
umask 077
CN_WORKDIR=$(mktemp -d /tmp/yub-wpanel-cn.XXXXXXXXXX) || log_error "无法创建临时工作目录"
chmod 0700 "$CN_WORKDIR"
umask "$PREVIOUS_UMASK"
INSTALL_SCRIPT="$CN_WORKDIR/install.sh"
INSTALL_SHA256_FILE="$CN_WORKDIR/install.sh.sha256"
INSTALL_SIGNATURE_FILE="$CN_WORKDIR/install.sh.sha256.sig"
PUBLIC_KEY_FILE="$CN_WORKDIR/release-public-key.pem"

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

download_install_script() {
    local url="$1"
    local output="$2"
    local max_bytes="$3"

    [[ "$max_bytes" =~ ^[1-9][0-9]*$ ]] || return 1
    rm -f "$output"
    if command -v wget &>/dev/null; then
        if timeout 120s wget --no-config -q --https-only --no-hsts \
            --connect-timeout=15 \
            --read-timeout=30 \
            --tries=3 \
            --retry-connrefused \
            --waitretry=2 \
            -O - "$url" 2>/dev/null | head -c "$((max_bytes + 1))" > "$output"; then
            file_size_within_limit "$output" "$max_bytes" && return 0
        fi
        rm -f "$output"
    fi
    if command -v curl &>/dev/null; then
        if timeout 120s curl -q -fsSL \
            --proto '=https' \
            --proto-redir '=https' \
            --connect-timeout 15 \
            --max-time 120 \
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
    rm -f "$output"
    return 1
}

verify_install_script_bundle() {
    local script_file="$1"
    local sha_file="$2"
    local sig_file="$3"
    local expected_sha=""
    local signed_name=""
    local extra_field=""
    local actual_sha=""
    local nonempty_lines=""

    file_size_within_limit "$script_file" "$INSTALLER_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$sha_file" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$sig_file" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    [[ "$(wc -c < "$sig_file" | tr -d '[:space:]')" == "64" ]] || return 1

    printf '%s\n' "$RELEASE_PUBLIC_KEY_PEM" > "$PUBLIC_KEY_FILE"
    chmod 0600 "$PUBLIC_KEY_FILE"
    openssl pkeyutl -verify -pubin -inkey "$PUBLIC_KEY_FILE" -rawin \
        -in "$sha_file" -sigfile "$sig_file" >/dev/null 2>&1 || return 1

    nonempty_lines=$(awk 'NF {count++} END {print count+0}' "$sha_file")
    [[ "$nonempty_lines" == "1" ]] || return 1
    read -r expected_sha signed_name extra_field < "$sha_file" || return 1
    [[ -z "$extra_field" ]] || return 1
    [[ "$expected_sha" =~ ^[0-9a-fA-F]{64}$ ]] || return 1
    case "$signed_name" in
        install.sh|'*install.sh') ;;
        *) return 1 ;;
    esac
    actual_sha=$(sha256sum "$script_file" | awk '{print $1}') || return 1
    [[ "${actual_sha,,}" == "${expected_sha,,}" ]] || return 1

    grep -qF "YUB WPanel 安装脚本" "$script_file" && \
        grep -qF "RELEASE_PUBLIC_KEY_HEX=\"${RELEASE_PUBLIC_KEY_HEX}\"" "$script_file" && \
        grep -qFx "INSTALLER_RELEASE_VERSION=\"${BOOTSTRAP_RELEASE_VERSION}\"" "$script_file" && \
        grep -qF "verify_panel_release_bundle" "$script_file"
}

copy_local_install_script_bundle() {
    local script_dir="$1"

    file_size_within_limit "$script_dir/install.sh" "$INSTALLER_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/install.sh.sha256" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    file_size_within_limit "$script_dir/install.sh.sha256.sig" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
    install -m 0600 "$script_dir/install.sh" "$INSTALL_SCRIPT"
    install -m 0600 "$script_dir/install.sh.sha256" "$INSTALL_SHA256_FILE"
    install -m 0600 "$script_dir/install.sh.sha256.sig" "$INSTALL_SIGNATURE_FILE"
}

download_install_script_bundle() {
    local script_url="$1"

    rm -f "$INSTALL_SCRIPT" "$INSTALL_SHA256_FILE" "$INSTALL_SIGNATURE_FILE"
    download_install_script "$script_url" "$INSTALL_SCRIPT" "$INSTALLER_ASSET_MAX_BYTES" || return 1
    download_install_script "${script_url}.sha256" "$INSTALL_SHA256_FILE" "$CHECKSUM_ASSET_MAX_BYTES" || return 1
    download_install_script "${script_url}.sha256.sig" "$INSTALL_SIGNATURE_FILE" "$SIGNATURE_ASSET_MAX_BYTES" || return 1
}

INSTALL_SCRIPT_SOURCE=""
if [[ -n "$SCRIPT_DIR" ]] && \
   { [[ -e "$SCRIPT_DIR/install.sh" ]] || [[ -e "$SCRIPT_DIR/install.sh.sha256" ]] || [[ -e "$SCRIPT_DIR/install.sh.sha256.sig" ]]; }; then
    copy_local_install_script_bundle "$SCRIPT_DIR" || \
        log_error "同目录离线安装包不完整；必须同时提供 install.sh、install.sh.sha256 和 install.sh.sha256.sig"
    verify_install_script_bundle "$INSTALL_SCRIPT" "$INSTALL_SHA256_FILE" "$INSTALL_SIGNATURE_FILE" || \
        log_error "同目录 install.sh 未通过 Ed25519 签名和 SHA256 校验，拒绝执行"
    INSTALL_SCRIPT_SOURCE="同目录已签名 Release 资产"
elif [[ -n "$CUSTOM_PROXY" ]] && \
     download_install_script_bundle "${CUSTOM_PROXY}/${GITHUB_INSTALL_URL}" && \
     verify_install_script_bundle "$INSTALL_SCRIPT" "$INSTALL_SHA256_FILE" "$INSTALL_SIGNATURE_FILE"; then
    INSTALL_SCRIPT_SOURCE="自定义 GitHub 反代"
elif download_install_script_bundle "$GITHUB_INSTALL_URL" && \
     verify_install_script_bundle "$INSTALL_SCRIPT" "$INSTALL_SHA256_FILE" "$INSTALL_SIGNATURE_FILE"; then
    INSTALL_SCRIPT_SOURCE="GitHub Release"
else
    if [[ -n "$CUSTOM_PROXY" ]]; then
        log_warn "自定义反代和 GitHub Release 均未返回有效签名安装包"
    fi
    log_error "无法获取通过签名校验的 install.sh。建议方案：
  1. 检查服务器能否访问 GitHub Releases
  2. 如有可信的 GitHub 反代，设置 YUB_WPANEL_GITHUB_PROXY 后重试
  3. 手工下载同一 Release 的 install.sh、install.sh.sha256、install.sh.sha256.sig，与本脚本放在同一目录后重试
  未签名的 @main/jsDelivr 脚本不会被执行"
fi

chmod 0700 "$INSTALL_SCRIPT"
log_info "主安装脚本 Ed25519 签名与 SHA256 校验通过: $INSTALL_SCRIPT_SOURCE"
if [[ "$YUB_WPANEL_PREFER_CN_MIRROR" == "1" ]]; then
    bash "$INSTALL_SCRIPT" --prefer-cn "$@"
else
    bash "$INSTALL_SCRIPT" "$@"
fi
