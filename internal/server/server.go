package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// installPlaceholder 是 install.sh 中用于注入面板地址的占位符。
const installPlaceholder = `__MEERKAT_PANEL_HOST__`

//go:embed web_dist
var webFS embed.FS

// Server 是 meerkat 服务端实例。
type Server struct {
	addr    string
	dbPath  string
	version string

	store *Store
	hub   *Hub
	api   *API
	http  *http.Server
}

// New 构造服务端。
func New(addr, dbPath, version string) (*Server, error) {
	store, err := Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	hub := NewHub()
	api := NewAPI(store, hub, version)
	return &Server{addr: addr, dbPath: dbPath, version: version, store: store, hub: hub, api: api}, nil
}

// InitAdmin 初始化管理员账号，返回首次生成的密码（已存在则返回空）。
func (s *Server) InitAdmin() (string, error) { return s.store.InitAdmin() }

// Run 启动 HTTP 服务并阻塞，直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Agent
	mux.HandleFunc("/api/agents/report", s.api.handleAgentReport)

	// 访客
	mux.HandleFunc("/api/public/servers", s.api.handlePublicServers)
	mux.HandleFunc("/api/public/site", s.api.handlePublicSite)
	mux.HandleFunc("/api/recent/", s.api.handleRecent)
	mux.HandleFunc("/api/history", s.api.handleHistory)
	mux.HandleFunc("/ws", s.hub.ServeWS)

	// 管理
	mux.HandleFunc("/api/admin/login", s.api.handleLogin)
	mux.HandleFunc("/api/admin/logout", s.api.handleLogout)
	mux.HandleFunc("/api/admin/servers", s.api.requireAdmin(s.api.handleAdminServers))
	mux.HandleFunc("/api/admin/servers/", s.api.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reset-token") {
			s.api.handleAdminResetToken(w, r)
			return
		}
		s.api.handleAdminServerOne(w, r)
	}))
	mux.HandleFunc("/api/admin/settings", s.api.requireAdmin(s.api.handleAdminSettings))
	mux.HandleFunc("/api/admin/password", s.api.requireAdmin(s.api.handleAdminPassword))

	// Agent 二进制直传分发（离线安装：文件放在数据目录 agents/ 下）
	agentsDir := filepath.Join(filepath.Dir(s.dbPath), "agents")
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		s.serveAgentBinary(w, r, agentsDir)
	})

	// 静态面板
	dist, err := fs.Sub(webFS, "web_dist")
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/" && !strings.HasPrefix(p, "/admin") && p != "/install.sh" && !strings.HasPrefix(p, "/assets/") {
			http.NotFound(w, r)
			return
		}
		if p == "/install.sh" {
			s.serveInstallScript(w, r, dist)
			return
		}
		if p == "/admin" || p == "/admin/" {
			r.URL.Path = "/admin.html"
		}
		fileServer.ServeHTTP(w, r)
	})

	s.http = &http.Server{
		Addr:              s.addr,
		Handler:           withCommonHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 历史数据清理协程
	go s.pruneLoop(ctx)

	log.Printf("[server] 监听 %s", s.addr)
	err = s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown 优雅停机。
func (s *Server) Shutdown() {
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(ctx)
	}
	_ = s.store.Close()
}

func (s *Server) pruneLoop(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	st, err := s.store.GetSettings()
	if err == nil {
		if n, err := s.store.PruneHistory(st.DataKeepDays); err == nil && n > 0 {
			log.Printf("[server] 清理过期历史数据 %d 条", n)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := s.store.GetSettings()
			if err != nil {
				continue
			}
			if n, err := s.store.PruneHistory(st.DataKeepDays); err == nil && n > 0 {
				log.Printf("[server] 清理过期历史数据 %d 条", n)
			}
		}
	}
}

// serveInstallScript 分发安装脚本，自动将面板地址注入到脚本中。
// 这样用户只需 curl ... | bash -s -- -t <令牌>，无需手动传 -e。
func (s *Server) serveInstallScript(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	raw, err := fs.ReadFile(dist, "install.sh")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 根据请求构造面板地址（优先 X-Forwarded-Proto/Host，兼容反代）
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		scheme = xfp
	}
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = xfh
	}
	panelURL := scheme + "://" + host
	out := strings.Replace(string(raw), installPlaceholder, panelURL, 1)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	_, _ = w.Write([]byte(out))
}

// downloadNameRe 限制可下载文件名，防目录穿越。
var downloadNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// serveAgentBinary 从数据目录 agents/ 下分发 Agent 二进制。
// 文件命名约定: meerkat_<goos>_<goarch>，例如 meerkat_linux_amd64。
// 未放置任何文件时返回 404，安装脚本会自动回退 GitHub Release。
func (s *Server) serveAgentBinary(w http.ResponseWriter, r *http.Request, dir string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/download/")
	if !downloadNameRe.MatchString(name) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, name)
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
