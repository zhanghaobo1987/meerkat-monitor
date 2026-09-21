#!/bin/sh
# meerkat-agent 一键安装脚本
# 参考 komari-agent 安装脚本设计：多 init 系统支持 / GitHub 镜像回退 / 参数透传
#
# 用法（面板已自动注入地址，只需提供令牌）:
#   curl -fsSL <面板地址>/install.sh | bash -s -- -t <令牌>
#
# 安装参数（--install-* 由本脚本消费，其余参数原样透传给 agent）:
#   --install-dir <dir>           安装目录（默认 /opt/meerkat）
#   --install-service-name <name> 服务名（默认 meerkat-agent）
#   --install-ghproxy <url>       GitHub 加速镜像前缀
#   --install-version <ver>       指定版本（默认 latest）
#   --install-no-mirror           关闭自动镜像回退
#
# Agent 参数（透传，支持环境变量）:
#   -e, --endpoint <url>      面板地址（脚本已自动注入，一般无需指定）
#   -t, --token <token>       接入令牌
#   -i, --interval <sec>      采集间隔（秒），0 = 跟随服务端
#   -u, --ignore-unsafe-cert  忽略不安全证书
#   --include-nics <list>     仅统计指定网卡（逗号分隔）
#   --exclude-nics <list>     排除指定网卡（逗号分隔）
#   环境变量: MEERKAT_ENDPOINT / MEERKAT_TOKEN / MEERKAT_INTERVAL / AGENT_ENDPOINT / AGENT_TOKEN
set -eu

# ---- 日志 ----
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; CYAN='\033[0;36m'; WHITE='\033[1;37m'; NC='\033[0m'
log_info()    { printf "${NC} %s\n" "$1"; }
log_success() { printf "${GREEN}✔ %s${NC}\n" "$1"; }
log_warning() { printf "${YELLOW}[WARNING]${NC} %s\n" "$1"; }
log_error()   { printf "${RED}[ERROR]${NC} %s\n" "$1"; }
log_step()    { printf "${CYAN}➜ %s${NC}\n" "$1"; }
log_config()  { printf "${CYAN}[CONFIG]${NC} %s\n" "$1"; }

# $EUID 是 bash 专有，ash/dash 下补 POSIX 回退
EUID=${EUID:-$(id -u)}

# ---- 面板注入地址（服务端分发本脚本时自动替换）----
PANEL_HOST="__MEERKAT_PANEL_HOST__"

REPO="zhanghaobo1987/meerkat-monitor"

# ---- 默认值 ----
service_name="meerkat-agent"
target_dir="/opt/meerkat"
github_proxy=""
install_version=""
install_dir_specified=false
install_no_mirror=false
service_user="${SUDO_USER:-$(id -un)}"

# ---- 平台检测 ----
os_type=$(uname -s)
case $os_type in
    Darwin)
        os_name="darwin"
        target_dir="/usr/local/meerkat"
        if [ ! -w "/usr/local" ] && [ "$EUID" -ne 0 ]; then
            target_dir="$HOME/.meerkat"
            log_info "No write permission to /usr/local, using user directory: $target_dir"
        fi
        ;;
    Linux)    os_name="linux" ;;
    FreeBSD)  os_name="freebsd" ;;
    MINGW*|MSYS*|CYGWIN*)
        os_name="windows"; target_dir="/c/meerkat" ;;
    *)
        log_error "Unsupported operating system: $os_type"; exit 1 ;;
esac

# ---- 参数解析：install 参数消费，其余透传给 agent ----
agent_args=""
while [ $# -gt 0 ]; do
    case "$1" in
        --install-dir)
            target_dir="$2"; install_dir_specified=true; shift 2 ;;
        --install-service-name)
            service_name="$2"; shift 2 ;;
        --install-ghproxy)
            github_proxy="$2"; shift 2 ;;
        --install-version)
            install_version="$2"; shift 2 ;;
        --install-no-mirror)
            install_no_mirror=true; shift ;;
        --install*)
            log_warning "Unknown install parameter: $1"; shift ;;
        *)
            agent_args="$agent_args $1"; shift ;;
    esac
done
agent_args="${agent_args# }"

# 非特权安装归属用户目录
if [ "$EUID" -ne 0 ] && [ "$install_dir_specified" = false ]; then
    case "$os_name" in
        linux|freebsd) target_dir="${XDG_DATA_HOME:-$HOME/.local/share}/meerkat" ;;
    esac
fi
agent_path="${target_dir}/agent"

# ---- 自动注入面板地址 ----
endpoint="${MEERKAT_ENDPOINT:-${AGENT_ENDPOINT:-}}"
token="${MEERKAT_TOKEN:-${AGENT_TOKEN:-}}"
if [ -z "$endpoint" ] && [ "$PANEL_HOST" != "__MEERKAT_PANEL_HOST__" ] && [ -n "$PANEL_HOST" ]; then
    endpoint="$PANEL_HOST"
fi
# 未通过环境变量提供时，从透传参数中提取
if [ -z "$endpoint" ] || [ -z "$token" ]; then
    _tmp_args="$agent_args"
    _ep=""; _tk=""
    set -- $_tmp_args 2>/dev/null || true
    _tmp_args=""
    # 简单扫描提取 -e/-t 与长参数
    _scan="$agent_args"
    _ep=$(printf '%s' "$_scan" | sed -n 's/.*\(-e \|--endpoint=\|--endpoint \)\([^ ]*\).*/\2/p' | head -1)
    _tk=$(printf '%s' "$_scan" | sed -n 's/.*\(-t \|--token=\|--token \)\([^ ]*\).*/\2/p' | head -1)
    [ -n "$_ep" ] && endpoint="${_ep}"
    [ -n "$_tk" ] && token="${_tk}"
fi
if [ -z "$endpoint" ]; then
    echo "错误: 未检测到面板地址。请使用 -e <面板地址> 指定。" >&2
    exit 1
fi
if [ -z "$token" ]; then
    echo "错误: 必须提供接入令牌（-t <令牌>）" >&2
    echo "在管理后台「添加服务器」时生成的令牌即为接入令牌" >&2
    exit 1
fi
case "$endpoint" in
    http://*|https://*) ;;
    *) endpoint="https://${endpoint}" ;;
esac
endpoint="${endpoint%/}"

# 非特权 Linux 安装只能用 systemd user 服务
user_service=false
if [ "$EUID" -ne 0 ] && [ "$os_name" = "linux" ]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
        user_service=true
    else
        log_error "A non-root Linux installation requires a running systemd user session"
        log_info "Log in through systemd or install with elevated privileges."
        exit 1
    fi
fi

echo ""
printf "${WHITE}===========================================${NC}\n"
printf "${WHITE}     Meerkat Agent Installation Script     ${NC}\n"
printf "${WHITE}===========================================${NC}\n"
echo ""
log_config "Installation configuration:"
log_config "  Service name:  ${GREEN}$service_name${NC}"
log_config "  Service user:  ${GREEN}$service_user${NC}"
log_config "  Install dir:   ${GREEN}$target_dir${NC}"
log_config "  GitHub proxy:  ${GREEN}${github_proxy:-(auto)}${NC}"
log_config "  Panel:         ${GREEN}$endpoint${NC}"
log_config "  Agent version: ${GREEN}${install_version:-Latest}${NC}"
echo ""

# ---- 卸载旧安装 ----
uninstall_previous() {
    log_step "Checking for previous installation..."
    if [ "$user_service" = true ]; then
        if systemctl --user list-unit-files 2>/dev/null | grep -q "${service_name}.service"; then
            log_info "Stopping and disabling existing systemd user service..."
            systemctl --user stop "${service_name}.service" 2>/dev/null || true
            systemctl --user disable "${service_name}.service" 2>/dev/null || true
            rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/${service_name}.service"
            systemctl --user daemon-reload
        fi
    elif command -v systemctl >/dev/null 2>&1 && [ -f "/etc/systemd/system/${service_name}.service" ]; then
        log_info "Stopping and disabling existing systemd service..."
        $SUDO systemctl stop "${service_name}.service" 2>/dev/null || true
        $SUDO systemctl disable "${service_name}.service" 2>/dev/null || true
        $SUDO rm -f "/etc/systemd/system/${service_name}.service"
        $SUDO systemctl daemon-reload
    elif command -v rc-service >/dev/null 2>&1 && [ -f "/etc/init.d/${service_name}" ]; then
        log_info "Stopping and disabling existing OpenRC service..."
        $SUDO rc-service "${service_name}" stop 2>/dev/null || true
        $SUDO rc-update del "${service_name}" default 2>/dev/null || true
        $SUDO rm -f "/etc/init.d/${service_name}"
    elif command -v uci >/dev/null 2>&1 && [ -f "/etc/init.d/${service_name}" ]; then
        log_info "Stopping and disabling existing procd service..."
        $SUDO "/etc/init.d/${service_name}" stop 2>/dev/null || true
        $SUDO "/etc/init.d/${service_name}" disable 2>/dev/null || true
        $SUDO rm -f "/etc/init.d/${service_name}"
    elif [ "$os_name" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then
        system_plist="/Library/LaunchDaemons/tech.meerkat.${service_name}.plist"
        user_plist="$HOME/Library/LaunchAgents/tech.meerkat.${service_name}.plist"
        [ -f "$system_plist" ] && { $SUDO launchctl bootout system "$system_plist" 2>/dev/null || true; $SUDO rm -f "$system_plist"; }
        [ -f "$user_plist" ] && { launchctl bootout gui/$(id -u) "$user_plist" 2>/dev/null || true; rm -f "$user_plist"; }
    fi
    [ -f "$agent_path" ] && { log_info "Removing old binary..."; rm -f "$agent_path"; }
}
uninstall_previous

# ---- 依赖检查 ----
install_dependencies() {
    log_step "Checking dependencies..."
    deps="curl"
    missing=""
    for cmd in $deps; do
        command -v $cmd >/dev/null 2>&1 || missing="$missing $cmd"
    done
    if [ -n "$missing" ]; then
        if [ "$EUID" -ne 0 ]; then
            log_error "Missing required dependencies:$missing"
            log_info "Install them with your system package manager, then run this script again."
            exit 1
        fi
        if command -v apt >/dev/null 2>&1; then
            log_info "Using apt to install dependencies..."
            apt update -qq && apt install -y $missing
        elif command -v yum >/dev/null 2>&1; then
            log_info "Using yum to install dependencies..."
            yum install -y $missing
        elif command -v apk >/dev/null 2>&1; then
            log_info "Using apk to install dependencies..."
            apk add $missing
        elif command -v opkg >/dev/null 2>&1; then
            log_info "Using opkg to install dependencies (OpenWrt/iStoreOS)..."
            opkg update && opkg install $missing
        elif command -v brew >/dev/null 2>&1; then
            log_info "Using Homebrew to install dependencies..."
            brew install $missing
        else
            log_error "No supported package manager found (apt/yum/apk/opkg/brew)"
            exit 1
        fi
        log_success "Dependencies installed successfully"
    else
        log_success "Dependencies already satisfied"
    fi
}
install_dependencies

# ---- 架构检测 ----
arch=$(uname -m)
case $arch in
    x86_64)        arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    armv7*|armv6*) arch="arm" ;;
    i386|i686)
        case "$os_name" in linux|freebsd) arch="386" ;;
            *) log_error "32-bit x86 not supported on $os_name"; exit 1 ;; esac ;;
    *) log_error "Unsupported architecture: $arch on $os_name"; exit 1 ;;
esac
log_info "Detected OS: ${GREEN}$os_name${NC}, Architecture: ${GREEN}$arch${NC}"

file_name="meerkat_${os_name}_${arch}"

# ---- 版本解析 ----
version_to_install="latest"
[ -n "$install_version" ] && { log_info "Installing specified version: ${GREEN}$install_version${NC}"; version_to_install="$install_version"; }

if [ "$version_to_install" = "latest" ]; then
    download_path="latest/download"
else
    download_path="download/${version_to_install}"
fi
tarball="${file_name}.tar.gz"

# ---- 下载：面板直传 → GitHub（含镜像回退）----
SUDO=""
[ "$EUID" -ne 0 ] && command -v sudo >/dev/null 2>&1 && SUDO="sudo"

log_step "Creating installation directory: ${GREEN}$target_dir${NC}"
mkdir -p "$target_dir"
if [ "$EUID" -eq 0 ] && [ "$service_user" != "root" ]; then
    chown "$service_user" "$target_dir" 2>/dev/null || true
fi

dl_ok=""
# 方式一：面板直传（离线可用）
panel_url="${endpoint}/download/${file_name}"
log_step "Trying panel-served binary first..."
if _code=$(curl -fsSL -o "$agent_path" -w '%{http_code}' --connect-timeout 10 "$panel_url" 2>/dev/null) \
    && [ "$_code" = "200" ] && [ -s "$agent_path" ] \
    && head -c 4 "$agent_path" | od -An -tx1 | grep -qE '7f +45 +4c +46|cf +fa +ed +fe|ce +fa +ed +fe|ca +fe +ba +be'; then
    dl_ok="panel"
    log_success "Downloaded from panel (${GREEN}offline-capable${NC})"
else
    rm -f "$agent_path"
    log_info "Panel-served binary not available, falling back to GitHub..."

    # 方式二：GitHub Release + 自动镜像回退
    if [ -n "$github_proxy" ] || [ "$install_no_mirror" = "true" ]; then
        download_urls="https://github.com/${REPO}/releases/${download_path}/${tarball}"
        [ -n "$github_proxy" ] && download_urls="${github_proxy}/https://github.com/${REPO}/releases/${download_path}/${tarball}"
    else
        gh_url="https://github.com/${REPO}/releases/${download_path}/${tarball}"
        download_urls="
${gh_url}
https://ghfast.top/${gh_url}
https://gh-proxy.com/${gh_url}
https://ghproxy.net/${gh_url}
"
    fi
    for u in $download_urls; do
        log_step "Downloading $tarball ..."
        log_info "URL: ${CYAN}$u${NC}"
        if curl -fL --connect-timeout 15 -o "${agent_path}.tar.gz" "$u" && [ -s "${agent_path}.tar.gz" ]; then
            if tar -xzf "${agent_path}.tar.gz" -C "$target_dir" && mv -f "${target_dir}/${file_name}" "$agent_path" 2>/dev/null; then
                dl_ok="github"; break
            fi
        fi
        rm -f "${agent_path}.tar.gz" "$agent_path"
    done
fi

if [ -z "$dl_ok" ]; then
    log_error "Download failed from all sources (panel + GitHub + mirrors)"
    log_error "Retry later, or specify --install-ghproxy <mirror-prefix> manually"
    exit 1
fi

chmod +x "$agent_path"
if [ "$EUID" -eq 0 ] && [ "$service_user" != "root" ]; then
    chown "$service_user" "$agent_path" 2>/dev/null || true
fi
log_success "meerkat-agent installed to ${GREEN}$agent_path${NC}"

# ---- Agent 启动参数 ----
# endpoint/token 统一走环境变量（服务文件中注入），避免命令行泄露
full_args="$agent_args"

# ---- init 系统检测 ----
detect_init_system() {
    if [ -f /etc/NIXOS ]; then echo "nixos"; return; fi
    if [ -f /etc/alpine-release ]; then
        if command -v rc-service >/dev/null 2>&1 || [ -f /sbin/openrc-run ]; then echo "openrc"; return; fi
    fi
    pid1_process=$(ps -p 1 -o comm= 2>/dev/null | tr -d ' ')
    if [ "$pid1_process" = "systemd" ] || [ -d /run/systemd/system ]; then
        if command -v systemctl >/dev/null 2>&1 && systemctl list-units >/dev/null 2>&1; then echo "systemd"; return; fi
    fi
    if [ "$pid1_process" = "openrc-init" ] && command -v rc-service >/dev/null 2>&1; then echo "openrc"; return; fi
    if [ "$pid1_process" = "init" ] && [ ! -f /etc/alpine-release ]; then
        if { [ -d /run/openrc ] || [ -f /sbin/openrc ]; } && command -v rc-service >/dev/null 2>&1; then echo "openrc"; return; fi
    fi
    if command -v uci >/dev/null 2>&1 && [ -f /etc/rc.common ]; then echo "procd"; return; fi
    if [ "$os_name" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then echo "launchd"; return; fi
    if command -v systemctl >/dev/null 2>&1 && systemctl list-units >/dev/null 2>&1; then echo "systemd"; return; fi
    if command -v rc-service >/dev/null 2>&1 && [ -d /etc/init.d ]; then echo "openrc"; return; fi
    if command -v initctl >/dev/null 2>&1 && [ -d /etc/init ]; then echo "upstart"; return; fi
    echo "unknown"
}

init_system=$(detect_init_system)
[ "$user_service" = true ] && init_system="systemd-user"
log_info "Detected init system: ${GREEN}$init_system${NC}"

# ---- 注册服务 ----
if [ "$init_system" = "nixos" ]; then
    log_warning "NixOS detected. System services must be configured declaratively."
    cat <<EOF2
systemd.services.${service_name} = {
  description = "Meerkat Agent Service";
  after = [ "network.target" ];
  wantedBy = [ "multi-user.target" ];
  serviceConfig = {
    Type = "simple";
    Environment = [ "MEERKAT_ENDPOINT=${endpoint}" "MEERKAT_TOKEN=${token}" ];
    ExecStart = "${agent_path} agent";
    WorkingDirectory = "${target_dir}";
    Restart = "always";
    User = "${service_user}";
  };
};
EOF2
    log_info "Add the above to your NixOS configuration, then: sudo nixos-rebuild switch"
elif [ "$init_system" = "openrc" ]; then
    log_info "Using OpenRC for service management"
    service_file="/etc/init.d/${service_name}"
    $SUDO mkdir -p /etc/meerkat
    printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$endpoint" "$token" | $SUDO tee /etc/meerkat/agent.env >/dev/null
    $SUDO chmod 600 /etc/meerkat/agent.env
    $SUDO tee "$service_file" >/dev/null <<EOF
#!/sbin/openrc-run

name="Meerkat Agent Service"
description="Meerkat monitoring agent"
command="${agent_path}"
command_args="agent ${full_args}"
directory="${target_dir}"
pidfile="/run/${service_name}.pid"
retry="SIGTERM/30"
supervisor=supervise-daemon

depend() {
    need net
    after network
}
EOF
    $SUDO chmod +x "$service_file"
    $SUDO rc-update add "${service_name}" default
    $SUDO rc-service "${service_name}" start
    log_success "OpenRC service configured and started"
elif [ "$init_system" = "systemd-user" ]; then
    log_info "Using systemd user service management"
    service_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    service_file="${service_dir}/${service_name}.service"
    mkdir -p "$service_dir" "$HOME/.config/meerkat"
    printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$endpoint" "$token" > "$HOME/.config/meerkat/agent.env"
    chmod 600 "$HOME/.config/meerkat/agent.env"
    cat > "$service_file" <<EOF
[Unit]
Description=Meerkat Agent Service
After=network.target

[Service]
Type=simple
EnvironmentFile=%h/.config/meerkat/agent.env
ExecStart=${agent_path} agent ${full_args}
WorkingDirectory=${target_dir}
Restart=always

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload
    systemctl --user enable --now "${service_name}.service"
    log_success "Systemd user service configured and started"
elif [ "$init_system" = "systemd" ]; then
    log_info "Using systemd for service management"
    service_file="/etc/systemd/system/${service_name}.service"
    $SUDO mkdir -p /etc/meerkat
    printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$endpoint" "$token" | $SUDO tee /etc/meerkat/agent.env >/dev/null
    $SUDO chmod 600 /etc/meerkat/agent.env
    $SUDO tee "$service_file" >/dev/null <<EOF
[Unit]
Description=Meerkat Agent Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/meerkat/agent.env
ExecStart=${agent_path} agent ${full_args}
WorkingDirectory=${target_dir}
Restart=always
RestartSec=5
User=${service_user}

[Install]
WantedBy=multi-user.target
EOF
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable "${service_name}.service"
    $SUDO systemctl start "${service_name}.service"
    log_success "Systemd service configured and started"
elif [ "$init_system" = "procd" ]; then
    log_info "Using procd for service management (OpenWrt)"
    service_file="/etc/init.d/${service_name}"
    $SUDO mkdir -p /etc/meerkat
    printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$endpoint" "$token" | $SUDO tee /etc/meerkat/agent.env >/dev/null
    $SUDO chmod 600 /etc/meerkat/agent.env
    $SUDO tee "$service_file" >/dev/null <<EOF
#!/bin/sh /etc/rc.common

START=99
STOP=10

USE_PROCD=1

PROG="${agent_path}"

start_service() {
    procd_open_instance
    procd_set_param env MEERKAT_ENDPOINT="${endpoint}" MEERKAT_TOKEN="${token}"
    procd_set_param command "\$PROG" agent
    procd_append_param command ${full_args}
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_set_param user ${service_user}
    procd_close_instance
}

reload_service() {
    stop
    start
}
EOF
    $SUDO chmod +x "$service_file"
    $SUDO "/etc/init.d/${service_name}" enable
    $SUDO "/etc/init.d/${service_name}" start
    log_success "procd service configured and started"
elif [ "$init_system" = "launchd" ]; then
    log_info "Using launchd for service management"
    is_user_install=false
    case "$target_dir" in /Users/*) is_user_install=true ;; esac
    [ "$EUID" -ne 0 ] && is_user_install=true
    if [ "$is_user_install" = true ]; then
        plist_dir="$HOME/Library/LaunchAgents"
        plist_file="$plist_dir/tech.meerkat.${service_name}.plist"
        log_info "Installing as user-level service (LaunchAgent)"
        mkdir -p "$plist_dir"
        service_user="$(whoami)"
        log_dir="$HOME/Library/Logs"
    else
        plist_dir="/Library/LaunchDaemons"
        plist_file="$plist_dir/tech.meerkat.${service_name}.plist"
        log_info "Installing as system-level service (LaunchDaemon)"
        log_dir="/var/log"
    fi
    cat > "$plist_file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>tech.meerkat.${service_name}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${agent_path}</string>
        <string>agent</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>MEERKAT_ENDPOINT</key>
        <string>${endpoint}</string>
        <key>MEERKAT_TOKEN</key>
        <string>${token}</string>
    </dict>
    <key>WorkingDirectory</key>
    <string>${target_dir}</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>UserName</key>
    <string>${service_user}</string>
    <key>StandardOutPath</key>
    <string>${log_dir}/${service_name}.log</string>
    <key>StandardErrorPath</key>
    <string>${log_dir}/${service_name}.log</string>
</dict>
</plist>
EOF
    if [ "$EUID" -eq 0 ]; then
        launchctl bootstrap system "$plist_file" 2>/dev/null || launchctl load -w "$plist_file"
    else
        launchctl bootstrap gui/$(id -u) "$plist_file" 2>/dev/null || launchctl load -w "$plist_file"
    fi
    log_success "launchd service configured and started"
elif [ "$init_system" = "upstart" ]; then
    log_info "Using upstart for service management"
    service_file="/etc/init/${service_name}.conf"
    $SUDO mkdir -p /etc/meerkat
    printf 'MEERKAT_ENDPOINT=%s\nMEERKAT_TOKEN=%s\n' "$endpoint" "$token" | $SUDO tee /etc/meerkat/agent.env >/dev/null
    $SUDO chmod 600 /etc/meerkat/agent.env
    $SUDO tee "$service_file" >/dev/null <<EOF
description "Meerkat Agent Service"

chdir ${target_dir}
start on filesystem or runlevel [2345]
stop on runlevel [!2345]

respawn
respawn limit 10 5
umask 022
console none
setuid ${service_user}

script
    . /etc/meerkat/agent.env
    export MEERKAT_ENDPOINT MEERKAT_TOKEN
    exec ${agent_path} agent ${full_args}
end script
EOF
    $SUDO initctl reload-configuration
    $SUDO initctl start "${service_name}"
    log_success "Upstart service configured and started"
else
    log_error "Unsupported or unknown init system: $init_system"
    log_info "Please run the agent manually:"
    echo "    MEERKAT_ENDPOINT=$endpoint MEERKAT_TOKEN=<token> $agent_path agent"
    exit 1
fi

echo ""
printf "${WHITE}===========================================${NC}\n"
log_success "meerkat-agent installation completed!"
log_config "Service:    ${GREEN}$service_name${NC}"
log_config "Binary:     ${GREEN}$agent_path${NC}"
log_config "Panel:      ${GREEN}$endpoint${NC}"
[ -n "$full_args" ] && log_config "Arguments:  ${GREEN}$full_args${NC}"
printf "${WHITE}===========================================${NC}\n"
