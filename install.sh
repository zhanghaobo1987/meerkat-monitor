#!/bin/sh
# meerkat-agent 一键安装脚本（Ubuntu / Debian / CentOS / macOS）
#
# 用法（面板已自动注入地址，只需提供令牌）:
#   curl -fsSL <面板地址>/install.sh | bash -s -- -t <令牌>
#
# 也支持手动指定:
#   install.sh -e <面板地址> -t <令牌>
#
# 环境变量:
#   MEERKAT_ENDPOINT   面板地址，等效 -e（优先级高于自动检测）
#   MEERKAT_TOKEN      接入令牌，等效 -t
#   MEERKAT_VERSION    指定安装版本（默认 latest）
set -eu

REPO="zhanghaobo1987/meerkat-monitor"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="meerkat-agent"

# 面板服务端在分发本脚本时自动注入地址（替换占位符）
PANEL_HOST="__MEERKAT_PANEL_HOST__"

ENDPOINT="${MEERKAT_ENDPOINT:-}"
TOKEN="${MEERKAT_TOKEN:-}"
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
  install.sh -t <接入令牌>          # 面板已自动注入地址时
  install.sh -e <面板地址> -t <令牌> # 手动指定面板地址

选项:
  -e, --endpoint   面板地址，如 https://monitor.example.com
  -t, --token      管理后台创建服务器时生成的接入令牌
  -h, --help       显示帮助

环境变量:
  MEERKAT_ENDPOINT / MEERKAT_TOKEN   等效 -e / -t
  MEERKAT_VERSION    指定版本（默认 latest）
USAGE
      exit 0 ;;
    *) echo "未知参数: $1（-h 查看帮助）" >&2; exit 1 ;;
  esac
done

# ---------- 自动检测面板地址 ----------
# 如果用户没传 -e，使用服务端注入的地址
if [ -z "$ENDPOINT" ]; then
  if [ "$PANEL_HOST" != "__MEERKAT_PANEL_HOST__" ] && [ -n "$PANEL_HOST" ]; then
    ENDPOINT="$PANEL_HOST"
  else
    echo "错误: 未检测到面板地址。请使用 -e <面板地址> 指定。" >&2
    echo "示例: install.sh -e http://panel:8080 -t <令牌>" >&2
    exit 1
  fi
fi

if [ -z "$TOKEN" ]; then
  echo "错误: 必须提供接入令牌（-t <令牌>）" >&2
  echo "在管理后台「添加服务器」时生成的令牌即为接入令牌" >&2
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

# ---------- 下载方式一：面板直传 ----------
try_panel_download() {
  _url="${ENDPOINT}/download/${BIN_NAME}"
  _code="$(curl -fsSL -o "$BIN_PATH" -w '%{http_code}' --connect-timeout 10 "$_url" 2>/dev/null || echo 000)"
  if [ "$_code" = "200" ] && [ -s "$BIN_PATH" ]; then
    _size="$(wc -c < "$BIN_PATH" | tr -d ' ')"
    if [ "$_size" -ge 1048576 ] && head -c 4 "$BIN_PATH" | od -An -tx1 | grep -qE '7f +45 +4c +46|cf +fa +ed +fe|ce +fa +ed +fe|ca +fe +ba +be'; then
      echo "==> 已从面板直传下载 (${_size} bytes)"
      return 0
    fi
  fi
  rm -f "$BIN_PATH"
  return 1
}

# ---------- 下载方式二：GitHub Release（仓库公开，无需 Token）----------
try_github_download() {
  if [ "$VERSION" = "latest" ]; then
    _api="https://api.github.com/repos/${REPO}/releases/latest"
  else
    _api="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
  fi
  _json="$(curl -fsSL --connect-timeout 15 "$_api" 2>/dev/null)" || _json=""
  if [ -z "$_json" ]; then
    echo "错误: 无法访问 GitHub API（请检查网络连接）" >&2
    return 1
  fi
  if [ "$VERSION" = "latest" ]; then
    VERSION="$(printf '%s' "$_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
    if [ -z "$VERSION" ]; then
      echo "错误: 未找到任何 Release" >&2
      return 1
    fi
  fi
  echo "==> 版本: ${VERSION}"

  _asset="${BIN_NAME}.tar.gz"
  # 从 Release 元数据中提取目标资产的下载地址
  _asset_url="$(printf '%s' "$_json" | sed -n '/"name": "'"${_asset}"'",/{x;s/.*"\(https:[^"]*\)".*/\1/p;d;}; \|"url": "https://api.github.com/repos/[^"]*/assets/|h')"
  if [ -z "$_asset_url" ]; then
    echo "错误: Release ${VERSION} 中未找到资产 ${_asset}" >&2
    return 1
  fi

  echo "==> 从 GitHub Release 下载 ${_asset}…"
  if ! curl -fsSL --connect-timeout 15 -H "Accept: application/octet-stream" -o "${TMP}/${_asset}" "$_asset_url"; then
    echo "错误: GitHub Release 下载失败" >&2
    return 1
  fi
  tar -xzf "${TMP}/${_asset}" -C "$TMP"
  [ -f "$BIN_PATH" ] || { echo "错误: 压缩包内容不符合预期" >&2; return 1; }
  echo "==> 已从 GitHub Release 下载"
}

if ! try_panel_download; then
  echo "==> 面板直传不可用，从 GitHub Release 下载…"
  if ! try_github_download; then
    echo "" >&2
    echo "=================================================================" >&2
    echo " 下载失败。解决方法（任选其一）:" >&2
    echo " 1. 检查网络连接是否可访问 github.com" >&2
    echo " 2. 在面板服务器 data/agents/ 目录放置二进制后重试" >&2
    echo "    （详见 README「Agent 二进制直传」）" >&2
    echo "=================================================================" >&2
    exit 1
  fi
fi

# ---------- 安装二进制 ----------
install_bin() {
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
