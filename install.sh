#!/bin/sh
# meerkat-agent 一键安装脚本
# 用法:
#   curl -fsSL <install-url> | bash -s -- -e https://your-panel.com -t <token>
# 也支持环境变量: MEERKAT_ENDPOINT / MEERKAT_TOKEN
set -eu

REPO="meerkat-monitor/meerkat"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="meerkat-agent"

ENDPOINT="${MEERKAT_ENDPOINT:-}"
TOKEN="${MEERKAT_TOKEN:-}"

# ---------- 参数解析 ----------
while [ $# -gt 0 ]; do
  case "$1" in
    -e|--endpoint) ENDPOINT="$2"; shift 2 ;;
    -t|--token)    TOKEN="$2"; shift 2 ;;
    -h|--help)
      echo "用法: install.sh -e <面板地址> -t <接入令牌>"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$ENDPOINT" ] || [ -z "$TOKEN" ]; then
  echo "错误: 必须提供 -e <面板地址> 与 -t <接入令牌>" >&2
  exit 1
fi

# ---------- 平台检测 ----------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux*) GOOS="linux" ;;
  Darwin*) GOOS="darwin" ;;
  *) echo "错误: 暂不支持操作系统 $OS（Windows 请参考 README 手动安装）" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  armv7l|armv6l) GOARCH="arm" ;;
  *) echo "错误: 暂不支持架构 $ARCH" >&2; exit 1 ;;
esac

echo "==> 平台: ${GOOS}/${GOARCH}"
echo "==> 面板: ${ENDPOINT}"

# ---------- 下载二进制 ----------
VERSION="${MEERKAT_VERSION:-latest}"
if [ "$VERSION" = "latest" ]; then
  echo "==> 查询最新版本…"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')"
  if [ -z "$VERSION" ]; then
    echo "错误: 无法获取最新版本号" >&2
    exit 1
  fi
fi
echo "==> 版本: ${VERSION}"

ASSET="meerkat_${GOOS}_${GOARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> 下载 ${URL}"
curl -fSL --progress-bar -o "${TMP}/${ASSET}" "$URL"
tar -xzf "${TMP}/${ASSET}" -C "$TMP"

if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo"
else
  SUDO=""
fi
$SUDO mkdir -p "$INSTALL_DIR"
$SUDO install -m 0755 "${TMP}/meerkat" "${INSTALL_DIR}/meerkat"
echo "==> 已安装到 ${INSTALL_DIR}/meerkat"

# ---------- 注册系统服务 ----------
write_env_file() {
  $SUDO mkdir -p /etc/meerkat
  printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$ENDPOINT" "$TOKEN" | $SUDO tee /etc/meerkat/agent.env >/dev/null
  $SUDO chmod 600 /etc/meerkat/agent.env
}

case "$(uname -s)" in
  Linux*)
    if command -v systemctl >/dev/null 2>&1; then
      write_env_file
      $SUDO tee /etc/systemd/system/${SERVICE_NAME}.service >/dev/null <<EOF
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
      $SUDO systemctl enable --now ${SERVICE_NAME}
      echo "==> 已注册并启动 systemd 服务: ${SERVICE_NAME}"
      echo "    查看状态: systemctl status ${SERVICE_NAME}"
    else
      write_env_file
      echo "==> 未检测到 systemd，请手动运行:"
      echo "    MEERKAT_ENDPOINT=$ENDPOINT MEERKAT_TOKEN=$TOKEN meerkat agent"
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
    echo "==> 已注册并启动 launchd 服务: tech.meerkat.agent"
    echo "    查看状态: launchctl list | grep meerkat"
    ;;
esac

echo "==> 安装完成！回到面板刷新页面即可看到这台服务器。"
