// Package server 实现 meerkat 服务端：存储、API、实时推送、通知与主题。
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
	group_name TEXT NOT NULL DEFAULT '',
	region     TEXT NOT NULL DEFAULT '',
	sort_order INTEGER NOT NULL DEFAULT 0,
	hidden     INTEGER NOT NULL DEFAULT 0,
	token_hash TEXT NOT NULL,
	token     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	os         TEXT NOT NULL DEFAULT '',
	arch       TEXT NOT NULL DEFAULT '',
	platform   TEXT NOT NULL DEFAULT '',
	kernel_ver TEXT NOT NULL DEFAULT '',
	cpu_model  TEXT NOT NULL DEFAULT '',
	cpu_cores  INTEGER NOT NULL DEFAULT 0,
	gpu_model  TEXT NOT NULL DEFAULT '',
	virt       TEXT NOT NULL DEFAULT '',
	agent_ver  TEXT NOT NULL DEFAULT '',
	ipv4       TEXT NOT NULL DEFAULT '',
	ipv6       TEXT NOT NULL DEFAULT '',
	price            REAL    NOT NULL DEFAULT 0,
	currency         TEXT    NOT NULL DEFAULT '$',
	billing_cycle    TEXT    NOT NULL DEFAULT 'none',
	expired_at       INTEGER NOT NULL DEFAULT 0,
	traffic_limit    INTEGER NOT NULL DEFAULT 0,
	traffic_type     TEXT    NOT NULL DEFAULT 'sum',
	offline_notify   INTEGER NOT NULL DEFAULT 1,
	offline_grace    INTEGER NOT NULL DEFAULT 60,
	last_offline_notify INTEGER NOT NULL DEFAULT 0,
	monthly_base_in  INTEGER NOT NULL DEFAULT 0,
	monthly_base_out INTEGER NOT NULL DEFAULT 0,
	monthly_reset    TEXT    NOT NULL DEFAULT ''
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
CREATE TABLE IF NOT EXISTS load_rules (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	server_uuid  TEXT NOT NULL DEFAULT '',
	metric       TEXT NOT NULL DEFAULT 'cpu',
	threshold    REAL NOT NULL DEFAULT 80,
	ratio        REAL NOT NULL DEFAULT 0.8,
	interval_min INTEGER NOT NULL DEFAULT 2,
	last_notify  INTEGER NOT NULL DEFAULT 0
);
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return err
	}
	// 旧版本升级：CREATE IF NOT EXISTS 不会为已存在表补列，逐列尝试 ALTER。
	alterCols := map[string]string{
		"group_name":          "TEXT NOT NULL DEFAULT ''",
		"region":              "TEXT NOT NULL DEFAULT ''",
		"hidden":              "INTEGER NOT NULL DEFAULT 0",
		"ipv4":                "TEXT NOT NULL DEFAULT ''",
		"ipv6":                "TEXT NOT NULL DEFAULT ''",
		"price":               "REAL NOT NULL DEFAULT 0",
		"currency":            "TEXT NOT NULL DEFAULT '$'",
		"billing_cycle":       "TEXT NOT NULL DEFAULT 'none'",
		"expired_at":          "INTEGER NOT NULL DEFAULT 0",
		"traffic_limit":       "INTEGER NOT NULL DEFAULT 0",
		"traffic_type":        "TEXT NOT NULL DEFAULT 'sum'",
		"offline_notify":      "INTEGER NOT NULL DEFAULT 1",
		"offline_grace":       "INTEGER NOT NULL DEFAULT 60",
		"last_offline_notify": "INTEGER NOT NULL DEFAULT 0",
		"monthly_base_in":     "INTEGER NOT NULL DEFAULT 0",
		"monthly_base_out":    "INTEGER NOT NULL DEFAULT 0",
		"monthly_reset":       "TEXT NOT NULL DEFAULT ''",
		"token":               "TEXT NOT NULL DEFAULT ''",
	}
	for col, def := range alterCols {
		if _, err := s.db.Exec("ALTER TABLE servers ADD COLUMN " + col + " " + def); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("migrate add %s: %w", col, err)
			}
		}
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// ---- 工具 ----

// NewToken 生成 32 字符十六进制随机令牌。
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUUID 生成不带连字符的 16 字符随机 ID。
func NewUUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HashToken 计算 token 摘要用于落库。
// 用 SHA-256 而非 bcrypt：token 是高熵随机值，无需抗暴力破解。
func HashToken(token string) string { return sha256Hex(token) }

// serverCols 是 servers 表读取列的统一顺序。
const serverCols = `uuid,name,note,tag,group_name,region,sort_order,hidden,created_at,token,
	os,arch,platform,kernel_ver,cpu_model,cpu_cores,gpu_model,virt,agent_ver,ipv4,ipv6,
	price,currency,billing_cycle,expired_at,traffic_limit,traffic_type,
	offline_notify,offline_grace,last_offline_notify,monthly_base_in,monthly_base_out,monthly_reset`

func scanServer(scan func(dest ...any) error) (*model.ServerAdmin, error) {
	var v model.ServerAdmin
	var hidden int
	var ipv4, ipv6, monthlyReset string
	err := scan(&v.UUID, &v.Name, &v.Note, &v.Tag, &v.Group, &v.Region, &v.SortOrder, &hidden, &v.CreatedAt, &v.Token,
		&v.AgentInfo.OS, &v.AgentInfo.Arch, &v.AgentInfo.Platform, &v.AgentInfo.KernelVer,
		&v.AgentInfo.CPUModel, &v.AgentInfo.CPUCores, &v.AgentInfo.GPUModel, &v.AgentInfo.Virt, &v.AgentInfo.Version,
		&ipv4, &ipv6,
		&v.Billing.Price, &v.Billing.Currency, &v.Billing.BillingCycle, &v.Billing.ExpiredAt,
		&v.Billing.TrafficLimit, &v.Billing.TrafficLimitType,
		&v.OfflineNotifyEnabled, &v.OfflineGraceSeconds, &v.LastOfflineNotify,
		&v.MonthlyBaseIn, &v.MonthlyBaseOut, &monthlyReset)
	if err != nil {
		return nil, err
	}
	v.Hidden = hidden != 0
	v.MonthlyReset = monthlyReset
	if ipv4 != "" || ipv6 != "" {
		if v.Report == nil {
			v.Report = &model.Report{}
		}
		v.Report.IPv4, v.Report.IPv6 = ipv4, ipv6
	}
	return &v, nil
}

// ---- 服务器管理 ----

// CreateServer 新建被监控服务器，返回完整记录（含明文 token，仅此一次）。
func (s *Store) CreateServer(name, note, tag, group, region string) (*model.ServerAdmin, error) {
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
		`INSERT INTO servers(uuid,name,note,tag,group_name,region,sort_order,token_hash,token,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		uuid, name, note, tag, group, region, maxOrder+1, HashToken(token), token, now)
	if err != nil {
		return nil, err
	}
	return &model.ServerAdmin{
		ServerPublic: model.ServerPublic{
			ServerStatic: model.ServerStatic{UUID: uuid, Name: name, Note: note, Tag: tag, Group: group, Region: region, SortOrder: maxOrder + 1, CreatedAt: now},
		},
		Token: token,
	}, nil
}

// ServerPatch 是可修改的服务器字段集合。
type ServerPatch struct {
	Name                 *string
	Note                 *string
	Tag                  *string
	Group                *string
	Region               *string
	SortOrder            *int
	Hidden               *bool
	Billing              *model.Billing
	OfflineNotifyEnabled *bool
	OfflineGraceSeconds  *int
}

// UpdateServer 按补丁更新服务器字段。
func (s *Store) UpdateServer(uuid string, p ServerPatch) error {
	sets := []string{}
	args := []any{}
	add := func(col string, val any) { sets = append(sets, col+"=?"); args = append(args, val) }
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return errors.New("服务器名称不能为空")
		}
		add("name", strings.TrimSpace(*p.Name))
	}
	if p.Note != nil {
		add("note", *p.Note)
	}
	if p.Tag != nil {
		add("tag", *p.Tag)
	}
	if p.Group != nil {
		add("group_name", *p.Group)
	}
	if p.Region != nil {
		add("region", *p.Region)
	}
	if p.SortOrder != nil {
		add("sort_order", *p.SortOrder)
	}
	if p.Hidden != nil {
		if *p.Hidden {
			add("hidden", 1)
		} else {
			add("hidden", 0)
		}
	}
	if p.Billing != nil {
		add("price", p.Billing.Price)
		add("currency", p.Billing.Currency)
		add("billing_cycle", p.Billing.BillingCycle)
		add("expired_at", p.Billing.ExpiredAt)
		add("traffic_limit", p.Billing.TrafficLimit)
		add("traffic_type", p.Billing.TrafficLimitType)
	}
	if p.OfflineNotifyEnabled != nil {
		if *p.OfflineNotifyEnabled {
			add("offline_notify", 1)
		} else {
			add("offline_notify", 0)
		}
	}
	if p.OfflineGraceSeconds != nil {
		add("offline_grace", *p.OfflineGraceSeconds)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, uuid)

	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE servers SET `+strings.Join(sets, ",")+` WHERE uuid=?`, args...)
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
	if _, err := s.db.Exec(`DELETE FROM stats_minute WHERE server_id=?`, uuid); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM load_rules WHERE server_uuid=?`, uuid)
	return err
}

// ResetToken 重新生成接入令牌，返回新明文 token。
func (s *Store) ResetToken(uuid string) (string, error) {
	token := NewToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE servers SET token_hash=?,token=? WHERE uuid=?`, HashToken(token), token, uuid)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", errors.New("服务器不存在")
	}
	return token, nil
}

// ListServers 返回全部服务器完整档案（按排序）。
func (s *Store) ListServers() ([]model.ServerAdmin, error) {
	rows, err := s.db.Query(`SELECT ` + serverCols + ` FROM servers ORDER BY sort_order,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ServerAdmin{}
	for rows.Next() {
		v, err := scanServer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// PublicServers 返回访客视角列表（不含 token；默认过滤隐藏节点）。
func (s *Store) PublicServers(includeHidden bool) ([]model.ServerPublic, error) {
	admins, err := s.ListServers()
	if err != nil {
		return nil, err
	}
	out := make([]model.ServerPublic, 0, len(admins))
	for _, a := range admins {
		if a.Hidden && !includeHidden {
			continue
		}
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
	err := s.db.QueryRow(`SELECT uuid FROM servers WHERE token=?`, token).Scan(&uuid)
	if err == nil {
		return uuid, nil
	}
	err = s.db.QueryRow(`SELECT uuid FROM servers WHERE token_hash=?`, HashToken(token)).Scan(&uuid)
	return uuid, err
}

// UpdateAgentStatic 更新 Agent 首次上报带来的静态信息。
func (s *Store) UpdateAgentStatic(uuid string, r *model.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE servers SET os=?,arch=?,platform=?,kernel_ver=?,cpu_model=?,cpu_cores=?,gpu_model=?,virt=?,agent_ver=? WHERE uuid=?`,
		r.AgentInfo.OS, r.AgentInfo.Arch, r.AgentInfo.Platform, r.AgentInfo.KernelVer, r.AgentInfo.CPUModel, r.AgentInfo.CPUCores, r.AgentInfo.GPUModel, r.AgentInfo.Virt, r.AgentInfo.Version, uuid)
	if err != nil {
		log.Printf("[store] 更新服务器静态信息失败: %v", err)
	}
}

// UpdateServerIP 更新 Agent 出口 IP。
func (s *Store) UpdateServerIP(uuid, ipv4, ipv6 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE servers SET ipv4=?,ipv6=? WHERE uuid=?`, ipv4, ipv6, uuid)
	if err != nil {
		log.Printf("[store] 更新服务器 IP 失败: %v", err)
	}
}

// GetServerAdmin 读取一台服务器的完整档案（管理视角）。
func (s *Store) GetServerAdmin(uuid string) (*model.ServerAdmin, error) {
	row := s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE uuid=?`, uuid)
	return scanServer(row.Scan)
}

// MarkOfflineNotified 记录离线通知发送时间。
func (s *Store) MarkOfflineNotified(uuid string, ts int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE servers SET last_offline_notify=? WHERE uuid=?`, ts, uuid)
}

// MonthlyTrafficRecord 依据当前累计值刷新月度流量基准（跨月自动重置），返回基准值。
func (s *Store) MonthlyTrafficRecord(uuid string, totalIn, totalOut uint64) (baseIn, baseOut uint64, monthKey string) {
	month := time.Now().UTC().Format("2006-01")
	var bIn, bOut uint64
	var oldMonth string
	err := s.db.QueryRow(`SELECT monthly_base_in,monthly_base_out,monthly_reset FROM servers WHERE uuid=?`, uuid).
		Scan(&bIn, &bOut, &oldMonth)
	if err != nil {
		return 0, 0, month
	}
	if oldMonth == month {
		return bIn, bOut, month
	}
	// 跨月（或首次记录）：以当前累计值为新基准
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE servers SET monthly_base_in=?,monthly_base_out=?,monthly_reset=? WHERE uuid=?`,
		totalIn, totalOut, month, uuid)
	return totalIn, totalOut, month
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

// AllRecentHistory 返回全部服务器最近 N 分钟的分钟数据（仪表盘/告警统计用）。
func (s *Store) AllRecentHistory(minutes int) (map[string][]model.MinutePoint, error) {
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
	rows, err := s.db.Query(`SELECT server_id,time,cpu,mem_pct,disk_pct,net_in,net_out,load1,tcp,uptime FROM stats_minute WHERE time>=? ORDER BY time`,
		since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.MinutePoint{}
	for rows.Next() {
		var sid string
		var p model.MinutePoint
		if err := rows.Scan(&sid, &p.Time, &p.CPU, &p.MemPct, &p.DiskPct, &p.NetIn, &p.NetOut, &p.Load1, &p.TCP, &p.Uptime); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], p)
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

// ---- 负载通知规则 ----

// ListLoadRules 返回全部负载告警规则。
func (s *Store) ListLoadRules() ([]model.LoadRule, error) {
	rows, err := s.db.Query(`SELECT id,name,server_uuid,metric,threshold,ratio,interval_min,last_notify FROM load_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LoadRule
	for rows.Next() {
		var r model.LoadRule
		if err := rows.Scan(&r.ID, &r.Name, &r.ServerUUID, &r.Metric, &r.Threshold, &r.Ratio, &r.IntervalMin, &r.LastNotify); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateLoadRule 新建负载告警规则。
func (s *Store) CreateLoadRule(r model.LoadRule) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO load_rules(name,server_uuid,metric,threshold,ratio,interval_min) VALUES(?,?,?,?,?,?)`,
		r.Name, r.ServerUUID, r.Metric, r.Threshold, r.Ratio, r.IntervalMin)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateLoadRule 更新负载告警规则。
func (s *Store) UpdateLoadRule(r model.LoadRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE load_rules SET name=?,server_uuid=?,metric=?,threshold=?,ratio=?,interval_min=? WHERE id=?`,
		r.Name, r.ServerUUID, r.Metric, r.Threshold, r.Ratio, r.IntervalMin, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("规则不存在")
	}
	return nil
}

// DeleteLoadRule 删除负载告警规则。
func (s *Store) DeleteLoadRule(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM load_rules WHERE id=?`, id)
	return err
}

// MarkLoadRuleNotified 记录规则最近一次告警时间。
func (s *Store) MarkLoadRuleNotified(id int64, ts int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE load_rules SET last_notify=? WHERE id=?`, ts, id)
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

// ---- 通用 KV 设置 ----

// GetSetting 读取一个 KV 设置。
func (s *Store) GetSetting(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v, err == nil
}

// SetSetting 覆盖保存一个 KV 设置。
func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

const defaultSettings = `{"site_name":"Meerkat","data_keep_days":30,"report_interval":2}`

// GetSettings 读取站点设置（不存在则返回默认值）。
func (s *Store) GetSettings() (model.SiteSettings, error) {
	raw, ok := s.GetSetting("site")
	if !ok {
		raw = defaultSettings
	}
	var st model.SiteSettings
	err := jsonUnmarshal(raw, &st)
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
	raw, err := jsonMarshal(st)
	if err != nil {
		return err
	}
	return s.SetSetting("site", string(raw))
}

// GetNotifySettings 读取通知设置（含默认值兜底）。
func (s *Store) GetNotifySettings() model.NotifySettings {
	ns := model.DefaultNotifySettings()
	if raw, ok := s.GetSetting("notify"); ok {
		_ = jsonUnmarshal(raw, &ns)
	}
	return ns
}

// SaveNotifySettings 保存通知设置。
func (s *Store) SaveNotifySettings(ns model.NotifySettings) error {
	raw, err := jsonMarshal(ns)
	if err != nil {
		return err
	}
	return s.SetSetting("notify", string(raw))
}

// GetActiveTheme 返回当前激活主题（空 = 内置主题）。
func (s *Store) GetActiveTheme() string {
	v, _ := s.GetSetting("active_theme")
	return v
}

// SetActiveTheme 设置激活主题。
func (s *Store) SetActiveTheme(name string) error {
	return s.SetSetting("active_theme", name)
}
