// Package model 定义 meerkat 服务端与 Agent 之间共享的数据结构与通信协议。
package model

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

	// 静态信息（Agent 启动后通常不变，服务端只在首次或变化时更新）
	OS        string `json:"os,omitempty"`       // linux / darwin / windows
	Arch      string `json:"arch,omitempty"`     // amd64 / arm64 ...
	Platform  string `json:"platform,omitempty"` // e.g. "Ubuntu 22.04"
	KernelVer string `json:"kernel_ver,omitempty"`
	CPUModel  string `json:"cpu_model,omitempty"`
	CPUCores  int    `json:"cpu_cores,omitempty"`
	GPUModel  string `json:"gpu_model,omitempty"`
	Virt      string `json:"virt,omitempty"` // 虚拟化类型 e.g. kvm / docker

	Version string `json:"version,omitempty"` // Agent 版本号
}

// ReportResponse 是服务端对 Agent 上报的应答。
type ReportResponse struct {
	OK       bool   `json:"ok"`
	Interval int    `json:"interval"` // 建议的上报间隔（秒），Agent 据此调节
	Message  string `json:"message,omitempty"`
}

// ServerStatic 是服务端侧一台被监控服务器的静态档案。
type ServerStatic struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	Tag       string `json:"tag,omitempty"`
	SortOrder int    `json:"sort_order"`
	CreatedAt int64  `json:"created_at"`
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

// ServerPublic 是面向访客的服务器公开信息（不含 token）。
type ServerPublic struct {
	ServerStatic
	AgentInfo
	Online   bool    `json:"online"`
	LastSeen int64   `json:"last_seen"`
	Report   *Report `json:"report,omitempty"` // 最新一次负载快照
}

// ServerAdmin 是管理后台可见的完整信息，包含接入令牌。
type ServerAdmin struct {
	ServerPublic
	Token string `json:"token"`
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

// AgentTask 为未来扩展预留：服务端下发到 Agent 的任务指令。
// 首版未启用，协议字段先行占位。
type AgentTask struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}
