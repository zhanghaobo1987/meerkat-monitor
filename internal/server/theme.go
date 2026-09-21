package server

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// themeShortRe 合法主题目录名。
var themeShortRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// themesDir 返回主题根目录（数据库同级的 themes/）。
func (s *Server) themesDir() string {
	return filepath.Join(filepath.Dir(s.dbPath), "themes")
}

// readThemeManifest 读取一个主题目录的 komari-theme.json。
func readThemeManifest(dir string) (*model.ThemeInfo, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "komari-theme.json"))
	if err != nil {
		return nil, err
	}
	var ti model.ThemeInfo
	if err := json.Unmarshal(raw, &ti); err != nil {
		return nil, err
	}
	if ti.Short == "" {
		ti.Short = filepath.Base(dir)
	}
	// 校验主题有 dist/index.html
	if _, err := os.Stat(filepath.Join(dir, "dist", "index.html")); err != nil {
		return nil, fmt.Errorf("主题缺少 dist/index.html")
	}
	ti.Dir = filepath.Base(dir)
	return &ti, nil
}

// ListThemes 扫描主题目录返回全部已安装主题。
func (s *Server) ListThemes() []model.ThemeInfo {
	active := s.store.GetActiveTheme()
	entries, err := os.ReadDir(s.themesDir())
	if err != nil {
		return nil
	}
	var out []model.ThemeInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ti, err := readThemeManifest(filepath.Join(s.themesDir(), e.Name()))
		if err != nil {
			continue
		}
		ti.Active = ti.Short == active || ti.Dir == active
		out = append(out, *ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Short < out[j].Short })
	return out
}

// safeZipPath 校验 ZIP 条目路径，防目录穿越与 zip-slip。
func safeZipPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.Contains(name, "..") {
		return "", errors.New("非法路径: " + name)
	}
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) {
		return "", errors.New("非法绝对路径: " + name)
	}
	return clean, nil
}

// InstallThemeFromZIP 从 ZIP 数据安装主题。
// 兼容两种打包结构：
//  1. 根目录直接含 komari-theme.json + dist/（Komari 标准）
//  2. 外层一个目录包裹（GitHub 下载的源码包）
func (s *Server) InstallThemeFromZIP(data []byte) (*model.ThemeInfo, error) {
	tmpDir, err := os.MkdirTemp("", "meerkat-theme-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	zr, err := zip.NewReader(newByteReaderAt(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("无效的 ZIP 文件: %w", err)
	}
	// 全部解压到临时目录（限制单文件 64MB、总 512MB、文件数 20000）
	var totalFiles int
	var totalSize uint64
	for _, f := range zr.File {
		if totalFiles > 20000 {
			return nil, errors.New("ZIP 文件数超出限制")
		}
		rel, err := safeZipPath(f.Name)
		if err != nil {
			return nil, err
		}
		if rel == "." {
			continue
		}
		dst := filepath.Join(tmpDir, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		totalFiles++
		if f.UncompressedSize64 > 64<<20 {
			return nil, errors.New("单文件超出 64MB 限制: " + rel)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return nil, err
		}
		n, err := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return nil, err
		}
		totalSize += uint64(n)
		if totalSize > 512<<20 {
			return nil, errors.New("解压总大小超出 512MB 限制")
		}
	}

	// 定位清单所在目录（根或唯一一级子目录）
	manifestDir := tmpDir
	if _, err := os.Stat(filepath.Join(manifestDir, "komari-theme.json")); err != nil {
		subs, _ := os.ReadDir(tmpDir)
		found := false
		for _, e := range subs {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(tmpDir, e.Name(), "komari-theme.json")); err == nil {
					manifestDir = filepath.Join(tmpDir, e.Name())
					found = true
					break
				}
			}
		}
		if !found {
			return nil, errors.New("ZIP 中未找到 komari-theme.json（不兼容 Komari 主题格式）")
		}
	}

	ti, err := readThemeManifest(manifestDir)
	if err != nil {
		return nil, fmt.Errorf("主题清单无效: %w", err)
	}
	if !themeShortRe.MatchString(ti.Short) {
		return nil, errors.New("主题 short 名称非法: " + ti.Short)
	}

	// 安装到 themes/<short>/（覆盖旧版本）
	dst := filepath.Join(s.themesDir(), ti.Short)
	if err := os.RemoveAll(dst); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.themesDir(), 0o755); err != nil {
		return nil, err
	}
	if err := copyDir(manifestDir, dst); err != nil {
		return nil, err
	}
	ti.Dir = ti.Short
	return ti, nil
}

// DeleteTheme 删除一个主题（激活中的主题不可删）。
func (s *Server) DeleteTheme(short string) error {
	if !themeShortRe.MatchString(short) {
		return errors.New("非法主题名")
	}
	if s.store.GetActiveTheme() == short {
		return errors.New("主题正在使用中，请先切换到内置主题")
	}
	dst := filepath.Join(s.themesDir(), short)
	if _, err := os.Stat(dst); err != nil {
		return errors.New("主题不存在")
	}
	return os.RemoveAll(dst)
}

// byteReaderAt 让 []byte 满足 zip.Reader 需要的 ReaderAt。
type byteReaderAt struct{ b []byte }

func newByteReaderAt(b []byte) *byteReaderAt { return &byteReaderAt{b: b} }
func (r *byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	return n, nil
}

// copyDir 递归复制目录。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // 跳过符号链接等
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// serveThemeStatic 从激活主题的 dist/ 目录提供静态文件（前台页面）。
// 若请求文件不存在则回退 index.html（SPA 路由）。
func (s *Server) serveThemeStatic(w http.ResponseWriter, r *http.Request, dist fs.FS, themeRoot http.Dir) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	// 先尝试激活主题目录
	if f, err := themeRoot.Open("/" + p); err == nil {
		f.Close()
		http.ServeFile(w, r, filepath.Join(string(themeRoot), filepath.FromSlash(p)))
		return
	}
	// 回退 index.html（SPA）
	indexPath := filepath.Join(string(themeRoot), "index.html")
	if _, err := os.Stat(indexPath); err == nil {
		http.ServeFile(w, r, indexPath)
		return
	}
	// 主题没有该文件且无 index，回退内置面板
	if p == "index.html" || p == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 通过 http.ServeFileFS 提供
		http.ServeFileFS(w, r, dist, "index.html")
		return
	}
	http.NotFound(w, r)
}
