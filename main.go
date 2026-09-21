// meerkat —— 轻量级自托管服务器监控。
// 单二进制双角色：server（面板）与 agent（采集端）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/meerkat-monitor/meerkat/internal/agent"
	"github.com/meerkat-monitor/meerkat/internal/server"
)

var version = "dev"

const usage = `meerkat - 轻量级自托管服务器监控

用法:
  meerkat server  [选项]        启动监控面板服务端
  meerkat agent   [选项]        启动采集 Agent
  meerkat version               查看版本

服务端选项:
  -listen string    监听地址 (默认 ":8080")
  -db string        SQLite 数据文件路径 (默认 "./data/meerkat.db")

Agent 选项:
  -endpoint string  服务端地址，如 https://monitor.example.com
  -token string     服务器接入令牌（在管理后台创建服务器时生成）
  -tag string       可选标签
`

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("meerkat", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Printf("未知命令: %s\n\n", os.Args[1])
		fmt.Print(usage)
		os.Exit(1)
	}
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "监听地址")
	dbPath := fs.String("db", "./data/meerkat.db", "SQLite 数据文件路径")
	_ = fs.Parse(args)

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	srv, err := server.New(*listen, *dbPath, version)
	if err != nil {
		log.Fatalf("初始化服务端失败: %v", err)
	}

	// 首次启动生成随机管理员密码
	pw, err := srv.InitAdmin()
	if err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	}
	if pw != "" {
		fmt.Println("==========================================================")
		fmt.Println("  首次启动已创建管理员账号")
		fmt.Println("  用户名: admin")
		fmt.Printf("  密码:   %s\n", pw)
		fmt.Println("  请登录 /admin 后立即修改密码！")
		fmt.Println("==========================================================")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("服务端退出: %v", err)
		}
	case <-ctx.Done():
		log.Println("收到退出信号，正在关闭…")
		srv.Shutdown()
	}
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "服务端地址，如 https://monitor.example.com")
	token := fs.String("token", "", "服务器接入令牌")
	_ = fs.Parse(args)

	cfg := agent.Config{
		Endpoint: *endpoint,
		Token:    *token,
		Version:  version,
		// 允许环境变量覆盖，便于 Docker/systemd 部署
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = os.Getenv("MEERKAT_ENDPOINT")
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("MEERKAT_TOKEN")
	}
	if cfg.Endpoint == "" || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -endpoint 与 -token（或环境变量 MEERKAT_ENDPOINT / MEERKAT_TOKEN）")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, cfg); err != nil {
		log.Fatalf("Agent 退出: %v", err)
	}
}
