// Package server 实现 meerkat 服务端：存储、API、实时推送。
package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// Store 基于 SQLite 的持久化存储层。所有方法并发安全。
type Store struct {
	db   *sql.DB
	mu   sync.Mutex // SQLite 单写者，用互斥锁串行化写事务
	path string
}

// Open 打开（或创建）数据库并完成建表迁移。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 最佳实践：单连接避免锁竞争
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS servers (
	uuid       TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	tag        TEXT NOT NULL DEFAULT '',
	sort_order INTEGER NOT NULL DEFAULT 0,
	token_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	os         TEXT NOT NULL DEFAULT '',
	arch       TEXT NOT NULL DEFAULT '',
	platform   TEXT NOT NULL DEFAULT '',
	kernel_ver TEXT NOT NULL DEFAULT '',
	cpu_model  TEXT NOT NULL DEFAULT '',
	cpu_cores  INTEGER NOT NULL DEFAULT 0,
	gpu_model  TEXT NOT NULL DEFAULT '',
	virt       TEXT NOT NULL DEFAULT '',
	agent_ver  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS admin (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	username TEXT NOT NULL,
	password_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stats_minute (
	server_id TEXT NOT NULL,
	time      INTEGER NOT NULL,
	cpu       REAL NOT NULL,
	mem_pct   REAL NOT NULL,
	disk_pct  REAL NOT NULL,
	net_in    REAL NOT NULL,
	net_out   REAL NOT NULL,
	load1     REAL NOT NULL,
	tcp       INTEGER NOT NULL,
	uptime    INTEGER NOT NULL,
	PRIMARY KEY (server_id, time)
);
CREATE INDEX IF NOT EXISTS idx_stats_time ON stats_minute(time);
`
	_, err := s.db.Exec(schema)
	return err
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// ---- 工具 ----

// NewToken 生成 32 字节十六进制随机令牌。
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUUID 生成不带连字符的 16 字节 UUID。
func NewUUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HashToken 计算 token 摘要用于落库（token 本身只展示一次/按需查看）。
// 这里用 SHA-256 而非 bcrypt：token 是高熵随机值，无需抗暴力破解。
func HashToken(token string) string {
	// 延迟导入避免额外依赖：x/crypto/sha256 无，标准库即可
	return stdSha256Hex(token)
}

// ---- 服务器管理 ----

// CreateServer 新建被监控服务器，返回完整记录（含明文 token，仅此一次）。
func (s *Store) CreateServer(name, note, tag string) (*model.ServerAdmin, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("服务器名称不能为空")
	}
	uuid := NewUUID()
	token := NewToken()
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	var maxOrder int
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(sort_order),0) FROM servers`).Scan(&maxOrder)
	_, err := s.db.Exec(
		`INSERT INTO servers(uuid,name,note,tag,sort_order,token_hash,created_at) VALUES(?,?,?,?,?,?,?)`,
		uuid, name, note, tag, maxOrder+1, HashToken(token), now)
	if err != nil {
		return nil, err
	}
	return &model.ServerAdmin{
		ServerPublic: model.ServerPublic{
			ServerStatic: model.ServerStatic{UUID: uuid, Name: name, Note: note, Tag: tag, SortOrder: maxOrder + 1, CreatedAt: now},
		},
		Token: token,
	}, nil
}

// UpdateServer 修改名称/备注/标签/排序。
func (s *Store) UpdateServer(uuid, name, note, tag string, sortOrder int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE servers SET name=?,note=?,tag=?,sort_order=? WHERE uuid=?`,
		strings.TrimSpace(name), note, tag, sortOrder, uuid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("服务器不存在")
	}
	return nil
}

// DeleteServer 删除服务器及其历史数据。
func (s *Store) DeleteServer(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM servers WHERE uuid=?`, uuid); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM stats_minute WHERE server_id=?`, uuid)
	return err
}

// ResetToken 重新生成接入令牌，返回新明文 token。
func (s *Store) ResetToken(uuid string) (string, error) {
	token := NewToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE servers SET token_hash=? WHERE uuid=?`, HashToken(token), uuid)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", errors.New("服务器不存在")
	}
	return token, nil
}

// ListServers 返回全部服务器完整档案（按排序，含静态信息与 token）。
func (s *Store) ListServers() ([]model.ServerAdmin, error) {
	rows, err := s.db.Query(`SELECT uuid,name,note,tag,sort_order,created_at,token_hash,
		os,arch,platform,kernel_ver,cpu_model,cpu_cores,gpu_model,virt,agent_ver
		FROM servers ORDER BY sort_order,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ServerAdmin{}
	for rows.Next() {
		var v model.ServerAdmin
		var tokenHash, os, arch, platform, kernel, cpum, gpu, virt, aver string
		var cores int
		if err := rows.Scan(&v.UUID, &v.Name, &v.Note, &v.Tag, &v.SortOrder, &v.CreatedAt, &tokenHash,
			&os, &arch, &platform, &kernel, &cpum, &cores, &gpu, &virt, &aver); err != nil {
			return nil, err
		}
		v.AgentInfo = model.AgentInfo{OS: os, Arch: arch, Platform: platform, KernelVer: kernel,
			CPUModel: cpum, CPUCores: cores, GPUModel: gpu, Virt: virt, Version: aver}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PublicServers 返回访客视角列表（不含 token）。
func (s *Store) PublicServers() ([]model.ServerPublic, error) {
	admins, err := s.ListServers()
	if err != nil {
		return nil, err
	}
	out := make([]model.ServerPublic, 0, len(admins))
	for _, a := range admins {
		out = append(out, a.ServerPublic)
	}
	return out, nil
}

// ServerUUIDByToken 通过明文 token 校验 Agent 身份，返回服务器 UUID。
func (s *Store) ServerUUIDByToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("missing token")
	}
	var uuid string
	err := s.db.QueryRow(`SELECT uuid FROM servers WHERE token_hash=?`, HashToken(token)).Scan(&uuid)
	return uuid, err
}

// UpdateAgentStatic 更新 Agent 首次上报带来的静态信息。
func (s *Store) UpdateAgentStatic(uuid string, r *model.Report) {
	_, err := s.db.Exec(`UPDATE servers SET os=?,arch=?,platform=?,kernel_ver=?,cpu_model=?,cpu_cores=?,gpu_model=?,virt=?,agent_ver=? WHERE uuid=?`,
		r.OS, r.Arch, r.Platform, r.KernelVer, r.CPUModel, r.CPUCores, r.GPUModel, r.Virt, r.Version, uuid)
	if err != nil {
		log.Printf("[store] 更新服务器静态信息失败: %v", err)
	}
}

// GetServerAdmin 读取一台服务器的完整档案（管理视角）。
func (s *Store) GetServerAdmin(uuid string) (*model.ServerAdmin, error) {
	var v model.ServerAdmin
	var os, arch, platform, kernel, cpum, gpu, virt, aver string
	var cores int
	err := s.db.QueryRow(`SELECT uuid,name,note,tag,sort_order,created_at,os,arch,platform,kernel_ver,cpu_model,cpu_cores,gpu_model,virt,agent_ver FROM servers WHERE uuid=?`, uuid).
		Scan(&v.UUID, &v.Name, &v.Note, &v.Tag, &v.SortOrder, &v.CreatedAt, &os, &arch, &platform, &kernel, &cpum, &cores, &gpu, &virt, &aver)
	if err != nil {
		return nil, err
	}
	v.AgentInfo = model.AgentInfo{OS: os, Arch: arch, Platform: platform, KernelVer: kernel,
		CPUModel: cpum, CPUCores: cores, GPUModel: gpu, Virt: virt, Version: aver}
	return &v, nil
}

// ---- 历史数据 ----

// SaveMinute 写入（或覆盖）一条分钟级聚合数据。
func (s *Store) SaveMinute(serverID string, p model.MinutePoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO stats_minute(server_id,time,cpu,mem_pct,disk_pct,net_in,net_out,load1,tcp,uptime) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		serverID, p.Time, p.CPU, p.MemPct, p.DiskPct, p.NetIn, p.NetOut, p.Load1, p.TCP, p.Uptime)
	return err
}

// History 读取某服务器最近 hours 小时的分钟级历史。
func (s *Store) History(serverID string, hours int) ([]model.MinutePoint, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT time,cpu,mem_pct,disk_pct,net_in,net_out,load1,tcp,uptime FROM stats_minute WHERE server_id=? AND time>=? ORDER BY time`,
		serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.MinutePoint
	for rows.Next() {
		var p model.MinutePoint
		if err := rows.Scan(&p.Time, &p.CPU, &p.MemPct, &p.DiskPct, &p.NetIn, &p.NetOut, &p.Load1, &p.TCP, &p.Uptime); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneHistory 删除保留期之外的历史数据，返回删除行数。
func (s *Store) PruneHistory(keepDays int) (int64, error) {
	if keepDays <= 0 {
		keepDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).Unix()
	res, err := s.db.Exec(`DELETE FROM stats_minute WHERE time < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- 管理员 ----

// InitAdmin 若不存在管理员则创建，返回生成的随机密码。
func (s *Store) InitAdmin() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM admin`).Scan(&exists)
	if exists > 0 {
		return "", nil
	}
	pw := NewToken()[:12]
	hash, err := bcryptHash(pw)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO admin(id,username,password_hash) VALUES(1,'admin',?)`, hash)
	if err != nil {
		return "", err
	}
	return pw, nil
}

// VerifyAdmin 校验用户名密码。
func (s *Store) VerifyAdmin(username, password string) bool {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM admin WHERE username=?`, username).Scan(&hash)
	if err != nil {
		return false
	}
	return bcryptCompare(password, hash)
}

// ChangeAdminPassword 修改管理员密码（需验证旧密码）。
func (s *Store) ChangeAdminPassword(oldPw, newPw string) error {
	var hash string
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRow(`SELECT password_hash FROM admin WHERE id=1`).Scan(&hash); err != nil {
		return err
	}
	if !bcryptCompare(oldPw, hash) {
		return errors.New("旧密码错误")
	}
	if len(newPw) < 6 {
		return errors.New("新密码至少 6 位")
	}
	newHash, err := bcryptHash(newPw)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE admin SET password_hash=? WHERE id=1`, newHash)
	return err
}

// ---- 站点设置 ----

const defaultSettings = `{"site_name":"Meerkat","data_keep_days":30,"report_interval":2}`

// GetSettings 读取站点设置（不存在则返回默认值）。
func (s *Store) GetSettings() (model.SiteSettings, error) {
	var st model.SiteSettings
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key='site'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return parseSettings(defaultSettings)
	}
	if err != nil {
		return st, err
	}
	return parseSettings(raw)
}

// SaveSettings 覆盖保存站点设置。
func (s *Store) SaveSettings(st model.SiteSettings) error {
	if st.DataKeepDays <= 0 {
		st.DataKeepDays = 30
	}
	if st.ReportInterval < 1 {
		st.ReportInterval = 2
	}
	if strings.TrimSpace(st.SiteName) == "" {
		st.SiteName = "Meerkat"
	}
	raw, err := jsonSettings(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO settings(key,value) VALUES('site',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, raw)
	return err
}

func parseSettings(raw string) (model.SiteSettings, error) {
	var st model.SiteSettings
	err := unmarshalJSON(raw, &st)
	if st.SiteName == "" {
		st.SiteName = "Meerkat"
	}
	if st.DataKeepDays <= 0 {
		st.DataKeepDays = 30
	}
	if st.ReportInterval < 1 {
		st.ReportInterval = 2
	}
	return st, err
}

// ---- 序列化辅助（集中在此，避免到处引 encoding/json）----

func unmarshalJSON(raw string, v any) error { return jsonUnmarshal(raw, v) }

func jsonSettings(st model.SiteSettings) (string, error) {
	b, err := jsonMarshal(st)
	return string(b), err
}

// stdSha256Hex 为避免包命名冲突的间接封装
func stdSha256Hex(s string) string { return sha256Hex(s) }

var _ = fmt.Sprintf // keep import if unused in future edits
