// Package model 定义 meerkat 服务端与 Agent 之间共享的数据结构与通信协议。
package model

import "time"

// Report 是 Agent 周期性上报到服务端的负载数据。
// 速率类字段（net_in/net_out）由 Agent 本地差分计算，单位 bytes/s。
type Report struct {
	// 实时指标
	CPUUsage    float64 `json:"cpu_usage"`     // CPU 使用率 %
	MemTotal    uint64  `json:"mem_total"`     // 物理内存总量 bytes
	MemUsed     uint64  `json:"mem_used"`      // 物理内存已用 bytes
	SwapTotal   uint64  `json:"swap_total"`    // Swap 总量 bytes
	SwapUsed    uint64  `json:"swap_used"`     // Swap 已用 bytes
	DiskTotal   uint64  `json:"disk_total"`    // 根分区总量 bytes
	DiskUsed    uint64  `json:"disk_used"`     // 根分区已用 bytes
	NetIn       uint64  `json:"net_in"`        // 下行速率 bytes/s
	NetOut      uint64  `json:"net_out"`       // 上行速率 bytes/s
	NetTotalIn  uint64  `json:"net_total_in"`  // 开机以来累计下行 bytes
	NetTotalOut uint64  `json:"net_total_out"` // 开机以来累计上行 bytes
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	TCPConns    int     `json:"tcp_conns"`
	UDPConns    int     `json:"udp_conns"`
	ProcCount   int     `json:"proc_count"`
	Uptime      uint64  `json:"uptime"` // 系统运行时长（秒）

	// 出口 IP（由服务端在应答中回填，Agent 无需自己探测）
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`

	// 静态信息（Agent 启动后通常不变，服务端只在首次或变化时更新）
	AgentInfo
}

// AgentInfo 是 Agent 上报的静态环境信息（启动后基本不变）。
type AgentInfo struct {
	OS        string `json:"os,omitempty"`       // linux / darwin / windows
	Arch      string `json:"arch,omitempty"`     // amd64 / arm64 ...
	Platform  string `json:"platform,omitempty"` // e.g. "Ubuntu 22.04"
	KernelVer string `json:"kernel_ver,omitempty"`
	CPUModel  string `json:"cpu_model,omitempty"`
	CPUCores  int    `json:"cpu_cores,omitempty"`
	GPUModel  string `json:"gpu_model,omitempty"`
	Virt      string `json:"virt,omitempty"`    // e.g. kvm / docker
	Version   string `json:"version,omitempty"` // Agent 版本号
}

// ReportResponse 是服务端对 Agent 上报的应答。
type ReportResponse struct {
	OK       bool   `json:"ok"`
	Interval int    `json:"interval"` // 建议的上报间隔（秒），Agent 据此调节
	Message  string `json:"message,omitempty"`
}

// Billing 是一台服务器的账单信息（价格 / 周期 / 到期 / 流量额度）。
type Billing struct {
	Price            float64 `json:"price"`              // 费用
	Currency         string  `json:"currency"`           // 货币符号，如 $ ¥
	BillingCycle     string  `json:"billing_cycle"`      // monthly / quarterly / semiannual / yearly / none
	ExpiredAt        int64   `json:"expired_at"`         // 到期 Unix 秒；0 表示长期
	TrafficLimit     int64   `json:"traffic_limit"`      // 流量额度 bytes；0 表示不限
	TrafficLimitType string  `json:"traffic_limit_type"` // sum / up / down
	TrafficUsed      int64   `json:"traffic_used"`       // 本周期已用 bytes（服务端计算）
	TrafficRemaining int64   `json:"traffic_remaining"`  // 本周期剩余 bytes；-1 表示不限
}

// ServerStatic 是服务端侧一台被监控服务器的静态档案。
type ServerStatic struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"` // 私有备注
	Tag       string `json:"tag,omitempty"`
	Group     string `json:"group,omitempty"` // 分组
	Region    string `json:"region,omitempty"`
	SortOrder int    `json:"sort_order"`
	Hidden    bool   `json:"hidden"`
	CreatedAt int64  `json:"created_at"`
}

// ServerPublic 是面向访客的服务器公开信息（不含 token）。
type ServerPublic struct {
	ServerStatic
	AgentInfo
	Billing  Billing `json:"billing"`
	Online   bool    `json:"online"`
	LastSeen int64   `json:"last_seen"`
	Report   *Report `json:"report,omitempty"` // 最新一次负载快照
}

// ServerAdmin 是管理后台可见的完整信息，包含接入令牌与通知配置。
type ServerAdmin struct {
	ServerPublic
	Token                string `json:"token"`
	OfflineNotifyEnabled bool   `json:"offline_notify_enabled"`
	OfflineGraceSeconds  int    `json:"offline_grace_seconds"`
	LastOfflineNotify    int64  `json:"last_offline_notify"`
	MonthlyBaseIn        uint64 `json:"monthly_base_in"`
	MonthlyBaseOut       uint64 `json:"monthly_base_out"`
	MonthlyReset         string `json:"monthly_reset"` // 基准所在月份 YYYY-MM
}

// MinutePoint 是一条按分钟聚合的历史数据，用于历史图表。
type MinutePoint struct {
	Time    int64   `json:"time"` // Unix 秒
	CPU     float64 `json:"cpu"`
	MemPct  float64 `json:"mem_pct"`
	DiskPct float64 `json:"disk_pct"`
	NetIn   float64 `json:"net_in"`
	NetOut  float64 `json:"net_out"`
	Load1   float64 `json:"load1"`
	TCP     int     `json:"tcp"`
	Uptime  uint64  `json:"uptime"`
}

// SiteSettings 是可由管理员修改的站点配置。
type SiteSettings struct {
	SiteName       string `json:"site_name"`
	DataKeepDays   int    `json:"data_keep_days"`  // 历史数据保留天数
	ReportInterval int    `json:"report_interval"` // Agent 上报间隔（秒）
}

// ---- 通知 ----

// NotifySettings 汇总全部通知配置（存储在 settings 表）。
type NotifySettings struct {
	Enabled       bool   `json:"enabled"`
	Channel       string `json:"channel"` // telegram / none
	MessageTPL    string `json:"message_tpl"`
	TGBotToken    string `json:"tg_bot_token"`
	TGChatID      string `json:"tg_chat_id"`
	TGThreadID    string `json:"tg_thread_id"`
	TGEndpoint    string `json:"tg_endpoint"`
	ExpiryEnabled bool   `json:"expiry_enabled"`
	ExpiryDays    int    `json:"expiry_days"` // 提前 N 天提醒
	LoginEnabled  bool   `json:"login_enabled"`
	TrafficPct    int    `json:"traffic_pct"` // 流量用量阈值 %，0 禁用
}

// DefaultNotifySettings 返回通知配置默认值。
func DefaultNotifySettings() NotifySettings {
	return NotifySettings{
		Channel:    "telegram",
		MessageTPL: "{{emoji}}{{emoji}}{{emoji}}\nEvent: {{event}}\nClients: {{client}}",
		TGEndpoint: "https://api.telegram.org/bot",
		ExpiryDays: 10,
		TrafficPct: 80,
	}
}

// LoadRule 是一条负载告警规则。
type LoadRule struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	ServerUUID  string  `json:"server_uuid"`  // 空 = 全部服务器
	Metric      string  `json:"metric"`       // cpu / ram
	Threshold   float64 `json:"threshold"`    // %
	Ratio       float64 `json:"ratio"`        // 时间占比 0-1
	IntervalMin int     `json:"interval_min"` // 重复告警间隔（分钟）
	LastNotify  int64   `json:"last_notify"`
}

// NotifyMessage 是通知引擎产生的一条待发消息。
type NotifyMessage struct {
	Event  string // offline / load / expiry / traffic / login / test
	Client string // 相关服务器名（多个以逗号分隔）
	Detail string
	Emoji  string
}

// ---- 主题 ----

// ThemeInfo 描述一个已安装主题（komari-theme.json 清单）。
type ThemeInfo struct {
	Name        string `json:"name"`
	Short       string `json:"short"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      string `json:"author"`
	URL         string `json:"url"`
	Preview     string `json:"preview"`
	Dir         string `json:"dir"`
	Active      bool   `json:"active"`
}

// ---- Komari 兼容结构（供 Komari 生态主题如 LuminaPlus 调用）----

// KomariClient 与 Komari 的 models.Client 公开字段形状对齐（访客视角）。
type KomariClient struct {
	UUID             string        `json:"uuid"`
	Name             string        `json:"name"`
	Region           string        `json:"region"`
	Group            string        `json:"group"`
	Tags             string        `json:"tags"`
	CpuName          string        `json:"cpu_name"`
	Virtualization   string        `json:"virtualization"`
	Arch             string        `json:"arch"`
	CpuCores         int           `json:"cpu_cores"`
	OS               string        `json:"os"`
	KernelVersion    string        `json:"kernel_version"`
	GpuName          string        `json:"gpu_name"`
	MemTotal         int64         `json:"mem_total"`
	SwapTotal        int64         `json:"swap_total"`
	DiskTotal        int64         `json:"disk_total"`
	Weight           int           `json:"weight"`
	Price            float64       `json:"price"`
	BillingCycle     int           `json:"billing_cycle"` // 1月 3季 6半年 12年 0无
	Currency         string        `json:"currency"`
	ExpiredAt        *time.Time    `json:"expired_at"`
	TrafficLimit     int64         `json:"traffic_limit"`
	TrafficLimitType string        `json:"traffic_limit_type"`
	Account          KomariAccount `json:"account"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// KomariAccount 是主题消费的账单聚合字段。
type KomariAccount struct {
	ExpiredAt        *time.Time `json:"expired_at"`
	Price            float64    `json:"price"`
	BillingCycle     int        `json:"billing_cycle"`
	Currency         string     `json:"currency"`
	TrafficLimit     int64      `json:"traffic_limit"`
	TrafficLimitType string     `json:"traffic_limit_type"`
	TrafficUsed      int64      `json:"traffic_used"`
	TrafficRemaining int64      `json:"traffic_remaining"`
}

// KomariReport 与 Komari v2.Report 的 JSON 形状对齐。
type KomariReport struct {
	CPU         KomariCPU         `json:"cpu"`
	Ram         KomariAmount      `json:"ram"`
	Swap        KomariAmount      `json:"swap"`
	Load        KomariLoad        `json:"load"`
	Disk        KomariAmount      `json:"disk"`
	Network     KomariNetwork     `json:"network"`
	Connections KomariConnections `json:"connections"`
	Uptime      int64             `json:"uptime"`
	Process     int               `json:"process"`
	UpdatedAt   string            `json:"updated_at"`
}

type KomariCPU struct {
	Name  string  `json:"name,omitempty"`
	Cores int     `json:"cores,omitempty"`
	Arch  string  `json:"arch,omitempty"`
	Usage float64 `json:"usage,omitempty"`
}

type KomariAmount struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

type KomariLoad struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type KomariNetwork struct {
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	TotalUp   int64 `json:"totalUp"`
	TotalDown int64 `json:"totalDown"`
}

type KomariConnections struct {
	TCP int `json:"tcp"`
	UDP int `json:"udp"`
}

// KomariRecord 与 Komari 的历史记录形状对齐（/api/recent/:uuid、/api/records/load）。
type KomariRecord struct {
	Time           time.Time `json:"time"`
	Cpu            float32   `json:"cpu"`
	Ram            int64     `json:"ram"`
	RamTotal       int64     `json:"ram_total"`
	Swap           int64     `json:"swap"`
	SwapTotal      int64     `json:"swap_total"`
	Load           float32   `json:"load"`
	Disk           int64     `json:"disk"`
	DiskTotal      int64     `json:"disk_total"`
	NetIn          int64     `json:"net_in"`
	NetOut         int64     `json:"net_out"`
	NetTotalUp     int64     `json:"net_total_up"`
	NetTotalDown   int64     `json:"net_total_down"`
	Process        int       `json:"process"`
	Connections    int       `json:"connections"`
	ConnectionsUdp int       `json:"connections_udp"`
}

// AgentTask 为未来扩展预留：服务端下发到 Agent 的任务指令。
type AgentTask struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}
