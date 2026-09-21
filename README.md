---
AIGC:
  ContentProducer: '001191110102MAD55U9H0F10002'
  ContentPropagator: '001191110102MAD55U9H0F10002'
  Label: '1'
  ProduceID: 'c256f1d7-8236-44ca-a023-5a68706f29f8'
  PropagateID: 'c256f1d7-8236-44ca-a023-5a68706f29f8'
  ReservedCode1: 'ebfba740-120c-448d-90cf-24eed128b5f4'
  ReservedCode2: 'ebfba740-120c-448d-90cf-24eed128b5f4'
---

# Meerkat

[English](README_EN.md) | 简体中文

<p align="center">
  <img src="docs/screenshots/meerkat-home.png" alt="Meerkat 面板" width="820">
</p>

**Meerkat**（猫鼬，天生的哨兵）是一个轻量级、自托管的服务器监控面板。单二进制部署、资源占用极低，通过一个 Web 界面实时掌握你所有服务器的运行状态。

> 本项目受 [Komari](https://github.com/komari-monitor/komari) 的产品理念启发，但是一份**完全独立原创**的实现（协议、代码、界面均从零设计），面向喜爱轻量方案与零构建前端的自托管玩家。

## 特性

- **实时监控**：Agent 默认每 2 秒上报一次，WebSocket 秒级推送到面板
- **开箱即用**：服务端单二进制、SQLite 存储、前端原生 JS 零依赖零构建
- **指标完整**：CPU / 内存 / Swap / 磁盘 / 网络速率与累计流量 / 负载 / TCP·UDP 连接数 / 进程数 / 运行时长
- **历史图表**：分钟级聚合落库，默认保留 30 天（可配置），24 小时走势一目了然
- **多服务器管理**：令牌接入、标签分组、排序、重置令牌、一键复制安装命令
- **安全**：Agent 令牌哈希落库、管理端 HttpOnly 会话、bcrypt 密码哈希
- **跨平台**：Linux / macOS / Windows（amd64 / arm64 / arm），支持 Docker
- **一键安装**：面板内置安装脚本分发，复制命令即可在目标机完成 Agent 部署（systemd / launchd 自动注册）

## 截图

| 首页总览 | 详情图表 | 管理后台 |
| --- | --- | --- |
| ![home](docs/screenshots/meerkat-home.png) | ![detail](docs/screenshots/meerkat-detail.png) | ![admin](docs/screenshots/meerkat-admin.png) |

## 快速开始

### Docker 部署服务端

```bash
git clone https://github.com/meerkat-monitor/meerkat.git
cd meerkat
docker compose up -d
```

或直接使用二进制：

```bash
# 下载对应平台的压缩包并解压得到 meerkat
./meerkat server --listen :8080 --db ./data/meerkat.db
```

首次启动会在日志中打印自动生成的管理员密码（用户名 `admin`），登录 `http://<your-host>:8080/admin` 后请立即修改。

### 添加服务器并安装 Agent

1. 进入 **管理后台 → 服务器列表 → 添加服务器**，得到接入令牌
2. 在目标服务器上执行面板给出的安装命令（也可手动运行）：

```bash
curl -fsSL http://<your-panel-host>:8080/install.sh | bash -s -- \
  -e http://<your-panel-host>:8080 -t <接入令牌>
```

脚本会自动识别平台与架构、下载对应版本，并注册为 `systemd`（Linux）或 `launchd`（macOS）常驻服务。

### 手动运行 Agent

```bash
./meerkat agent -endpoint http://<your-panel-host>:8080 -token <接入令牌>

# 或使用环境变量
MEERKAT_ENDPOINT=http://host:8080 MEERKAT_TOKEN=xxx ./meerkat agent
```

## 从源码构建

依赖：Go ≥ 1.24（无 CGO 要求）

```bash
make build          # 构建当前平台 → dist/meerkat
make release-local  # 本地交叉编译全部平台 → dist/
```

## 架构一览

```
┌──────────────┐   HTTP POST /api/agents/report (Bearer Token)
│ meerkat agent│ ────────────────────────────────────────────▶ ┌───────────────┐
│ (每台服务器) │ ◀──────────────────────────────────────────── │ meerkat server│
└──────────────┘        响应携带建议上报间隔                    │   单二进制    │
                                                                │  SQLite 持久化 │
        浏览器 ── WebSocket /ws 实时推送 ◀────────────────────▶ │  内嵌前端资源  │
        浏览器 ── REST /api/public/* /api/admin/* ──────────▶  └───────────────┘
```

- `internal/model` — 通信协议与数据结构
- `internal/server` — 存储层 / API / 实时 Hub / 静态资源嵌入
- `internal/agent` — 指标采集（gopsutil）与上报循环
- `web_dist` — 原生 HTML/CSS/JS 面板，`go:embed` 打包进二进制

## 安全须知

Meerkat 是自托管监控工具，请仅部署在你拥有或获得授权的系统上。请勿将面板直接暴露在公网且不做任何防护；建议放在反向代理之后并启用 HTTPS。

## Roadmap

- [ ] 告警通知（Webhook / Telegram / 邮件）
- [ ] Agent 在线终端与文件管理
- [ ] 主题系统与主题市场
- [ ] ICMP / TCP 端口拨测
- [ ] 服务端多用户与只读分享链接

## License

[MIT](LICENSE)

> AI生成