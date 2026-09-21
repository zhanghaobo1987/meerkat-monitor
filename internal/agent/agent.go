// Package agent 实现 meerkat 采集端：本地指标采集与周期上报。
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// Config Agent 运行配置（与 komari-agent 参数集对齐）。
type Config struct {
	Endpoint          string // 服务端地址（-e / AGENT_ENDPOINT / MEERKAT_ENDPOINT）
	Token             string // 接入令牌（-t / AGENT_TOKEN / MEERKAT_TOKEN）
	Interval          int    // 上报间隔秒（-i / AGENT_INTERVAL），0 = 跟随服务端
	Version           string
	DisableAutoUpdate bool   // 预留：禁用自动更新
	IgnoreUnsafeCert  bool   // 忽略不安全证书（-u）
	IncludeNICs       string // 仅统计指定网卡（逗号分隔）
	ExcludeNICs       string // 排除指定网卡（逗号分隔）
	PreferIPVersion   string // 4 / 6
}

// Agent 一个运行中的采集端。
type Agent struct {
	cfg Config
	cli *http.Client

	mu       sync.Mutex
	lastIn   uint64
	lastOut  uint64
	lastTime time.Time
}

// ParseFlags 解析 agent 子命令参数。
func ParseFlags(args []string) *Config {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	cfg := &Config{}
	fs.StringVar(&cfg.Endpoint, "endpoint", "", "面板地址，如 https://monitor.example.com")
	fs.StringVar(&cfg.Endpoint, "e", "", "面板地址（简写）")
	fs.StringVar(&cfg.Token, "token", "", "接入令牌")
	fs.StringVar(&cfg.Token, "t", "", "接入令牌（简写）")
	fs.IntVar(&cfg.Interval, "interval", 0, "数据采集间隔（秒），0 = 跟随服务端建议")
	fs.IntVar(&cfg.Interval, "i", 0, "数据采集间隔（秒，简写）")
	fs.BoolVar(&cfg.DisableAutoUpdate, "disable-auto-update", false, "禁用自动更新（预留）")
	fs.BoolVar(&cfg.IgnoreUnsafeCert, "ignore-unsafe-cert", false, "忽略不安全证书")
	fs.BoolVar(&cfg.IgnoreUnsafeCert, "u", false, "忽略不安全证书（简写）")
	fs.StringVar(&cfg.IncludeNICs, "include-nics", "", "仅统计指定网卡，逗号分隔")
	fs.StringVar(&cfg.ExcludeNICs, "exclude-nics", "", "排除指定网卡，逗号分隔")
	fs.StringVar(&cfg.PreferIPVersion, "prefer-ip-version", "", "优先使用 IP 版本：4 或 6")
	_ = fs.Parse(args)
	return cfg
}

// FromEnv 用环境变量补全未设置的字段。
func FromEnv(cfg *Config) *Config {
	env := func(keys ...string) string {
		for _, k := range keys {
			if v := os.Getenv(k); v != "" {
				return v
			}
		}
		return ""
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = env("MEERKAT_ENDPOINT", "AGENT_ENDPOINT")
	}
	if cfg.Token == "" {
		cfg.Token = env("MEERKAT_TOKEN", "AGENT_TOKEN")
	}
	if cfg.Interval == 0 {
		if v := env("MEERKAT_INTERVAL", "AGENT_INTERVAL"); v != "" {
			n := 0
			for _, c := range v {
				if c < '0' || c > '9' {
					n = 0
					break
				}
				n = n*10 + int(c-'0')
			}
			cfg.Interval = n
		}
	}
	return cfg
}

// Run 阻塞运行 Agent，直到 ctx 取消。
func Run(ctx context.Context, cfg Config) error {
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		cfg.Endpoint = "https://" + cfg.Endpoint
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")

	if cfg.Endpoint == "https://" || cfg.Token == "" {
		return fmt.Errorf("必须提供 -endpoint 与 -token（或环境变量 MEERKAT_ENDPOINT / MEERKAT_TOKEN）")
	}

	transport := &http.Transport{}
	if cfg.IgnoreUnsafeCert {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式要求
	}
	a := &Agent{
		cfg: cfg,
		cli: &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}

	interval := time.Duration(2) * time.Second
	if cfg.Interval > 0 {
		interval = time.Duration(cfg.Interval) * time.Second
	}
	first := true
	log.Printf("[agent] 启动: endpoint=%s version=%s interval=%s", a.cfg.Endpoint, cfg.Version, interval)

	for {
		report := a.collect()
		resp, err := a.report(ctx, report)
		if err != nil {
			if first {
				log.Printf("[agent] 首次上报失败: %v（将自动重试）", err)
			}
		} else if resp.Interval > 0 && cfg.Interval == 0 {
			// 未显式指定间隔时，跟随服务端建议
			interval = time.Duration(resp.Interval) * time.Second
		}
		first = false

		select {
		case <-ctx.Done():
			log.Println("[agent] 收到退出信号")
			return nil
		case <-time.After(interval):
		}
	}
}

// collect 采集一轮本机指标。
func (a *Agent) collect() *model.Report {
	r := &model.Report{}
	now := time.Now()

	// CPU 使用率
	cpuPct, err := cpu.Percent(0, false)
	if err == nil && len(cpuPct) > 0 {
		r.CPUUsage = round1(cpuPct[0])
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		r.MemTotal = vm.Total
		r.MemUsed = vm.Total - vm.Available
	}
	if sm, err := mem.SwapMemory(); err == nil {
		r.SwapTotal, r.SwapUsed = sm.Total, sm.Used
	}
	if lu, err := load.Avg(); err == nil {
		r.Load1, r.Load5, r.Load15 = round2(lu.Load1), round2(lu.Load5), round2(lu.Load15)
	}

	// 磁盘：优先根分区
	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			if p.Mountpoint == "/" || (r.DiskTotal == 0 && isRootLike(p.Mountpoint)) {
				if u, err := disk.Usage(p.Mountpoint); err == nil {
					r.DiskTotal, r.DiskUsed = u.Total, u.Used
					if p.Mountpoint == "/" {
						break
					}
				}
			}
		}
	}

	// 网络：按 include/exclude 过滤网卡后聚合
	if counters, err := gnet.IOCounters(true); err == nil {
		include := splitSet(a.cfg.IncludeNICs)
		exclude := splitSet(a.cfg.ExcludeNICs)
		var totalIn, totalOut uint64
		for _, c := range counters {
			if len(include) > 0 && !include[c.Name] {
				continue
			}
			if exclude[c.Name] || isVirtualNIC(c.Name) {
				continue
			}
			totalIn += c.BytesRecv
			totalOut += c.BytesSent
		}
		a.mu.Lock()
		if !a.lastTime.IsZero() {
			dt := now.Sub(a.lastTime).Seconds()
			if dt > 0 && totalIn >= a.lastIn {
				r.NetIn = uint64(float64(totalIn-a.lastIn) / dt)
			}
			if dt > 0 && totalOut >= a.lastOut {
				r.NetOut = uint64(float64(totalOut-a.lastOut) / dt)
			}
		}
		a.lastIn, a.lastOut, a.lastTime = totalIn, totalOut, now
		a.mu.Unlock()
		r.NetTotalIn, r.NetTotalOut = totalIn, totalOut
	}
	if conns, err := gnet.Connections("tcp"); err == nil {
		r.TCPConns = countEstablished(conns)
	}
	if uconns, err := gnet.Connections("udp"); err == nil {
		r.UDPConns = len(uconns)
	}

	// 进程数
	if procs, err := process.Pids(); err == nil {
		r.ProcCount = len(procs)
	}

	// 系统信息
	if info, err := host.Info(); err == nil {
		r.Uptime = info.Uptime
		r.AgentInfo.OS = info.OS
		r.AgentInfo.Platform = info.Platform
		r.AgentInfo.KernelVer = info.KernelArch
		if info.PlatformVersion != "" {
			r.AgentInfo.Platform = info.Platform + " " + info.PlatformVersion
		}
	}
	r.Arch = runtime.GOARCH
	if ci, err := cpu.Info(); err == nil && len(ci) > 0 {
		r.CPUModel = strings.TrimSpace(ci[0].ModelName)
		if n, err := cpu.Counts(true); err == nil {
			r.CPUCores = n
		}
	}
	r.Virt = detectVirt()
	r.Version = a.cfg.Version
	return r
}

// splitSet 将逗号分隔字符串转为集合。
func splitSet(s string) map[string]bool {
	out := map[string]bool{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// isVirtualNIC 过滤虚拟/回环网卡。
func isVirtualNIC(name string) bool {
	for _, p := range []string{"lo", "docker", "veth", "br-", "vmnet", "utun", "tun", "tap", "virbr", "vbr", "wg"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isRootLike(mp string) bool {
	return mp == "/" || mp == "C:\\" || mp == "C:/"
}

func countEstablished(conns []gnet.ConnectionStat) int {
	n := 0
	for _, c := range conns {
		if c.Status == "ESTABLISHED" {
			n++
		}
	}
	return n
}

func detectVirt() string {
	if ctxHost, err := host.Info(); err == nil && ctxHost.VirtualizationSystem != "" {
		return ctxHost.VirtualizationSystem
	}
	return ""
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// report 上报到服务端。
func (a *Agent) report(ctx context.Context, r *model.Report) (*model.ReportResponse, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Endpoint+"/api/agents/report", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("User-Agent", "meerkat-agent/"+a.cfg.Version)

	resp, err := a.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("令牌被拒绝(401)，请检查 token")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务端返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out model.ReportResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
