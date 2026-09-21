// Package agent 实现 meerkat 采集端：本地指标采集与周期上报。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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

// Config Agent 运行配置。
type Config struct {
	Endpoint string // 服务端根地址
	Token    string // 接入令牌
	Version  string
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

// Run 阻塞运行 Agent，直到 ctx 取消。
func Run(ctx context.Context, cfg Config) error {
	a := &Agent{
		cfg: cfg,
		cli: &http.Client{Timeout: 10 * time.Second},
	}
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		cfg.Endpoint = "https://" + cfg.Endpoint
	}
	a.cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")

	interval := time.Duration(2) * time.Second
	first := true
	log.Printf("[agent] 启动: endpoint=%s version=%s", a.cfg.Endpoint, cfg.Version)

	for {
		report := a.collect()
		resp, err := a.report(ctx, report)
		if err != nil {
			if first {
				log.Printf("[agent] 首次上报失败: %v（将自动重试）", err)
			}
		} else if resp.Interval > 0 {
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

	// CPU 使用率：与上一轮采样差分
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

	// 网络累计与速率
	if io, err := gnet.IOCounters(false); err == nil && len(io) > 0 {
		totalIn, totalOut := io[0].BytesRecv, io[0].BytesSent
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
		r.OS = info.OS
		r.Platform = info.Platform
		r.KernelVer = info.KernelArch
		if info.PlatformVersion != "" {
			r.Platform = info.Platform + " " + info.PlatformVersion
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
