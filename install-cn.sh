#!/bin/bash
set -e
set -o pipefail

# ============================================================
# YUB WPanel 国内入口脚本
# 主安装逻辑统一维护在 install.sh。
# 这里仅启用国内优先策略，并在管道安装时通过多个固定来源拉取主脚本。
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

GITHUB_INSTALL_URL="https://raw.githubusercontent.com/zangwp/yub-wpanel/main/install.sh"
INSTALL_SCRIPT_SOURCES=(
    "jsDelivr CDN|https://cdn.jsdelivr.net/gh/zangwp/yub-wpanel@main/install.sh"
    "GitHub 直连|${GITHUB_INSTALL_URL}"
)

if [[ -n "${YUB_WPANEL_GITHUB_PROXY:-}" ]]; then
    CUSTOM_PROXY="${YUB_WPANEL_GITHUB_PROXY%/}"
    INSTALL_SCRIPT_SOURCES=(
        "自定义 GitHub 反代|${CUSTOM_PROXY}/${GITHUB_INSTALL_URL}"
        "${INSTALL_SCRIPT_SOURCES[@]}"
    )
fi

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

export YUB_WPANEL_PREFER_CN_MIRROR=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
if [[ -n "$SCRIPT_DIR" ]] && [[ -f "$SCRIPT_DIR/install.sh" ]]; then
    log_info "使用同目录 install.sh，并启用国内优先策略"
    exec bash "$SCRIPT_DIR/install.sh" --prefer-cn "$@"
fi

download_install_script() {
    local url="$1"
    if command -v wget &>/dev/null; then
        wget -qO- "$url" 2>/dev/null && return 0
    fi
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" 2>/dev/null && return 0
    fi
    return 1
}

validate_install_script() {
    local content="$1"
    [[ "$content" == *"YUB WPanel 安装脚本"* ]] && [[ "$content" == *"部署面板二进制"* ]]
}

INSTALL_SCRIPT=""
for source in "${INSTALL_SCRIPT_SOURCES[@]}"; do
    label="${source%%|*}"
    url="${source#*|}"

    log_info "通过 ${label} 获取主安装脚本..."
    CANDIDATE_SCRIPT="$(download_install_script "$url" || true)"
    if [[ -n "$CANDIDATE_SCRIPT" ]] && validate_install_script "$CANDIDATE_SCRIPT"; then
        INSTALL_SCRIPT="$CANDIDATE_SCRIPT"
        log_info "主安装脚本获取成功: ${label}"
        break
    fi
    log_warn "${label} 获取失败或内容异常，尝试下一个来源..."
done

if [[ -z "$INSTALL_SCRIPT" ]]; then
    log_error "无法获取 install.sh。建议方案：
  1. 检查服务器能否访问 cdn.jsdelivr.net 或 GitHub
  2. 手动下载 install.sh 后执行：bash install.sh --prefer-cn
  3. 如有可信的 GitHub 反代，可设置 YUB_WPANEL_GITHUB_PROXY 后重试
  4. 如 GitHub Releases 也不可访问，请同时下载 release 附件 yub-wpanel，并与 install.sh 放在同一目录后重新运行"
fi

exec bash -s -- --prefer-cn "$@" <<< "$INSTALL_SCRIPT"
