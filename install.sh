#!/bin/sh
# meerkat-agent 一键安装脚本（Ubuntu / Debian / CentOS / macOS）
#
# 用法:
#   curl -fsSL <面板地址>/install.sh | bash -s -- -e https://your-panel.com -t <令牌>
#
# 环境变量:
#   MEERKAT_ENDPOINT   面板地址，等效 -e
#   MEERKAT_TOKEN      接入令牌，等效 -t
#   MEERKAT_GH_TOKEN   私有仓库从 GitHub Release 下载时所需的 PAT
#   MEERKAT_VERSION    指定安装版本（默认 latest）
#
# 二进制下载顺序:
#   1. 面板直传:  <面板地址>/download/meerkat_<os>_<arch>（推荐，离线可用）
#   2. GitHub Release（私有仓库需设置 MEERKAT_GH_TOKEN）
set -eu

REPO="zhanghaobo1987/meerkat-monitor"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="meerkat-agent"

ENDPOINT="${MEERKAT_ENDPOINT:-}"
TOKEN="${MEERKAT_TOKEN:-}"
GH_TOKEN="${MEERKAT_GH_TOKEN:-${GITHUB_TOKEN:-}}"
VERSION="${MEERKAT_VERSION:-latest}"

# ---------- 参数解析 ----------
while [ $# -gt 0 ]; do
  case "$1" in
    -e|--endpoint) ENDPOINT="$2"; shift 2 ;;
    -t|--token)    TOKEN="$2"; shift 2 ;;
    -h|--help)
      cat <<'USAGE'
meerkat-agent 安装脚本

用法:
  install.sh -e <面板地址> -t <接入令牌>

选项:
  -e, --endpoint   面板地址，如 https://monitor.example.com
  -t, --token      管理后台创建服务器时生成的接入令牌
  -h, --help       显示帮助

环境变量:
  MEERKAT_ENDPOINT / MEERKAT_TOKEN   等效 -e / -t
  MEERKAT_GH_TOKEN   私有仓库下载 Release 时所需的 GitHub PAT
  MEERKAT_VERSION    指定版本（默认 latest）
USAGE
      exit 0 ;;
    *) echo "未知参数: $1（-h 查看帮助）" >&2; exit 1 ;;
  esac
done

if [ -z "$ENDPOINT" ] || [ -z "$TOKEN" ]; then
  echo "错误: 必须提供 -e <面板地址> 与 -t <接入令牌>" >&2
  echo "示例: install.sh -e http://panel:8080 -t abc123..." >&2
  exit 1
fi

# 统一处理协议前缀
case "$ENDPOINT" in
  http://*|https://*) ;;
  *) ENDPOINT="https://${ENDPOINT}" ;;
esac
ENDPOINT="${ENDPOINT%/}"

# ---------- 平台检测 ----------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux*)  GOOS="linux" ;;
  Darwin*) GOOS="darwin" ;;
  *) echo "错误: 暂不支持操作系统 $OS（Windows 请到 Release 页手动下载）" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  armv7l|armv6l)  GOARCH="arm" ;;
  *) echo "错误: 暂不支持架构 $ARCH" >&2; exit 1 ;;
esac

echo "==> 系统: ${OS} (${ARCH}) → ${GOOS}/${GOARCH}"
echo "==> 面板: ${ENDPOINT}"

if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then SUDO="sudo"; else SUDO=""; fi
else
  SUDO=""
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BIN_NAME="meerkat_${GOOS}_${GOARCH}"
BIN_PATH="${TMP}/${BIN_NAME}"
DOWNLOADED=""

# ---------- 下载方式一：面板直传 ----------
try_panel_download() {
  _url="${ENDPOINT}/download/${BIN_NAME}"
  _code="$(curl -fsSL -o "$BIN_PATH" -w '%{http_code}' --connect-timeout 10 "$_url" 2>/dev/null || echo 000)"
  if [ "$_code" = "200" ] && [ -s "$BIN_PATH" ]; then
    # 基本校验：至少 1MB 且是 ELF/Mach-O 二进制（od 输出字节间为多空格）
    _size="$(wc -c < "$BIN_PATH" | tr -d ' ')"
    if [ "$_size" -ge 1048576 ] && head -c 4 "$BIN_PATH" | od -An -tx1 | grep -qE '7f +45 +4c +46|cf +fa +ed +fe|ce +fa +ed +fe|ca +fe +ba +be'; then
      DOWNLOADED="panel"
      echo "==> 已从面板直传下载 (${_size} bytes)"
      return 0
    fi
  fi
  rm -f "$BIN_PATH"
  return 1
}

# ---------- 下载方式二：GitHub Release ----------
gh_curl() {
  if [ -n "$GH_TOKEN" ]; then
    curl -fsSL -H "Authorization: Bearer ${GH_TOKEN}" "$@"
  else
    curl -fsSL "$@"
  fi
}

try_github_download() {
  # 统一取 Release 元数据（latest 或指定 tag）
  if [ "$VERSION" = "latest" ]; then
    _api="https://api.github.com/repos/${REPO}/releases/latest"
  else
    _api="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
  fi
  _json="$(gh_curl --connect-timeout 15 "$_api" 2>/dev/null)" || _json=""
  if [ -z "$_json" ]; then
    echo "错误: 无法访问 GitHub API（私有仓库需设置 MEERKAT_GH_TOKEN 环境变量）" >&2
    return 1
  fi
  if [ "$VERSION" = "latest" ]; then
    VERSION="$(printf '%s' "$_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
    if [ -z "$VERSION" ] || [ "$VERSION" = "Not Found" ]; then
      echo "错误: 未找到任何 Release（私有仓库请设置 MEERKAT_GH_TOKEN）" >&2
      return 1
    fi
  fi
  echo "==> 版本: ${VERSION}"

  _asset="${BIN_NAME}.tar.gz"
  # 从 Release 元数据中提取目标资产的 API 地址。
  # 注意：github.com 的 /releases/download/ 直链不接受 Bearer 认证，
  # 私有仓库必须走 Assets API + Accept: octet-stream（curl 跨主机重定向会自动剥离认证头）。
  # name 模式以逗号结尾锚定，避免误匹配 .sha256 等同前缀资产。
  _asset_url="$(printf '%s' "$_json" | sed -n '/"name": "'"${_asset}"'",/{x;s/.*"\(https:[^"]*\)".*/\1/p;d;}; \|"url": "https://api.github.com/repos/[^"]*/assets/|h')"
  if [ -z "$_asset_url" ]; then
    echo "错误: Release ${VERSION} 中未找到资产 ${_asset}" >&2
    return 1
  fi

  echo "==> 从 GitHub Release 下载 ${_asset}…"
  if ! gh_curl --connect-timeout 15 -H "Accept: application/octet-stream" -o "${TMP}/${_asset}" "$_asset_url"; then
    echo "错误: GitHub Release 下载失败（私有仓库需 MEERKAT_GH_TOKEN；或改用面板直传，见 README）" >&2
    return 1
  fi
  tar -xzf "${TMP}/${_asset}" -C "$TMP"
  [ -f "$BIN_PATH" ] || { echo "错误: 压缩包内容不符合预期" >&2; return 1; }
  DOWNLOADED="github"
  echo "==> 已从 GitHub Release 下载"
}

if ! try_panel_download; then
  echo "==> 面板直传不可用，回退 GitHub Release…"
  if ! try_github_download; then
    echo "" >&2
    echo "=================================================================" >&2
    echo " 两种下载方式均失败。解决方法（任选其一）:" >&2
    echo " 1. 面板直传: 把 Agent 二进制上传到面板服务器数据目录" >&2
    echo "    data/agents/${BIN_NAME} 后重试（推荐，详见 README）" >&2
    echo " 2. GitHub:    仓库为私有时，安装命令前加 MEERKAT_GH_TOKEN=<你的PAT>" >&2
    echo "=================================================================" >&2
    exit 1
  fi
fi

# ---------- 安装二进制 ----------
install_bin() {
  # 优先尝试无 sudo（目录可写时），失败再提权
  if [ -w "$INSTALL_DIR" ] 2>/dev/null; then
    install -m 0755 "$BIN_PATH" "${INSTALL_DIR}/meerkat" && return 0
  fi
  if [ -n "$SUDO" ]; then
    $SUDO mkdir -p "$INSTALL_DIR"
    $SUDO install -m 0755 "$BIN_PATH" "${INSTALL_DIR}/meerkat" && return 0
  fi
  mkdir -p "$INSTALL_DIR" 2>/dev/null || true
  install -m 0755 "$BIN_PATH" "${INSTALL_DIR}/meerkat" && return 0
  return 1
}

if ! install_bin; then
  echo "错误: 无法写入 ${INSTALL_DIR}（请检查权限）" >&2
  exit 1
fi
echo "==> 已安装: ${INSTALL_DIR}/meerkat"

# ---------- 注册系统服务 ----------
case "$(uname -s)" in
  Linux*)
    if command -v systemctl >/dev/null 2>&1; then
      $SUDO mkdir -p /etc/meerkat
      printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$ENDPOINT" "$TOKEN" \
        | $SUDO tee /etc/meerkat/agent.env >/dev/null
      $SUDO chmod 600 /etc/meerkat/agent.env
      $SUDO tee "/etc/systemd/system/${SERVICE_NAME}.service" >/dev/null <<EOF
[Unit]
Description=Meerkat Monitor Agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/meerkat/agent.env
ExecStart=${INSTALL_DIR}/meerkat agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
      $SUDO systemctl daemon-reload
      $SUDO systemctl enable --now "${SERVICE_NAME}"
      echo "==> systemd 服务已启动: ${SERVICE_NAME}"
      echo "    查看状态:   systemctl status ${SERVICE_NAME}"
      echo "    查看日志:   journalctl -u ${SERVICE_NAME} -f"
    else
      echo "==> 未检测到 systemd，请手动运行:"
      echo "    MEERKAT_ENDPOINT=$ENDPOINT MEERKAT_TOKEN=$TOKEN ${INSTALL_DIR}/meerkat agent"
    fi
    ;;
  Darwin*)
    PLIST="$HOME/Library/LaunchAgents/tech.meerkat.agent.plist"
    mkdir -p "$(dirname "$PLIST")"
    cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>tech.meerkat.agent</string>
  <key>ProgramArguments</key><array>
    <string>${INSTALL_DIR}/meerkat</string><string>agent</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>MEERKAT_ENDPOINT</key><string>${ENDPOINT}</string>
    <key>MEERKAT_TOKEN</key><string>${TOKEN}</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST"
    echo "==> launchd 服务已启动: tech.meerkat.agent"
    ;;
esac

echo "==> 安装完成！回到面板刷新页面即可看到这台服务器。"
