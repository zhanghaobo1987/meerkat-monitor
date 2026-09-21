package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// ---- 会话认证 ----

type sessionMgr struct {
	mu       sync.Mutex
	sessions map[string]time.Time // cookie -> 过期时间
}

const sessionTTL = 7 * 24 * time.Hour

func newSessionMgr() *sessionMgr {
	return &sessionMgr{sessions: make(map[string]time.Time)}
}

func (m *sessionMgr) create() string {
	tok := NewToken()
	m.mu.Lock()
	m.sessions[tok] = time.Now().Add(sessionTTL)
	m.mu.Unlock()
	return tok
}

func (m *sessionMgr) valid(tok string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(m.sessions, tok)
		return false
	}
	return true
}

func (m *sessionMgr) destroy(tok string) {
	m.mu.Lock()
	delete(m.sessions, tok)
	m.mu.Unlock()
}

// ---- API 容器 ----

// API 持有服务端全部业务依赖。
type API struct {
	store    *Store
	hub      *Hub
	notifier *Notifier
	session  *sessionMgr
	version  string
	startAt  time.Time
	srvRef   *Server // 反向引用（主题操作需要文件系统路径）
}

// NewAPI 构造 API。
func NewAPI(store *Store, hub *Hub, notifier *Notifier, version string) *API {
	return &API{store: store, hub: hub, notifier: notifier, session: newSessionMgr(), version: version, startAt: time.Now()}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---- Agent 接口 ----

// handleAgentReport 处理 Agent 周期上报。
func (a *API) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tok := bearer(r)
	uuid, err := a.store.ServerUUIDByToken(tok)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "无效的接入令牌")
		return
	}
	var rep model.Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&rep); err != nil {
		errJSON(w, http.StatusBadRequest, "上报数据格式错误")
		return
	}
	settings, _ := a.store.GetSettings()

	// 回填 Agent 出口 IP（服务端视角，比 Agent 自测更可靠）
	ip := clientIP(r)
	if ip != "" {
		if isIPv4(ip) {
			rep.IPv4 = ip
		} else {
			rep.IPv6 = ip
		}
	}

	// 静态信息有变化时更新档案
	if rep.AgentInfo.OS != "" || rep.AgentInfo.CPUModel != "" {
		if cur, err := a.store.GetServerAdmin(uuid); err == nil {
			ai := cur.AgentInfo
			if ai.OS != rep.AgentInfo.OS || ai.CPUModel != rep.AgentInfo.CPUModel || ai.CPUCores != rep.AgentInfo.CPUCores ||
				ai.Platform != rep.AgentInfo.Platform || ai.KernelVer != rep.AgentInfo.KernelVer || ai.Version != rep.AgentInfo.Version ||
				ai.Arch != rep.AgentInfo.Arch || ai.Virt != rep.AgentInfo.Virt || ai.GPUModel != rep.AgentInfo.GPUModel {
				a.store.UpdateAgentStatic(uuid, &rep)
			}
		}
	}
	// IP 变化时更新
	if rep.IPv4 != "" || rep.IPv6 != "" {
		if cur, err := a.store.GetServerAdmin(uuid); err == nil {
			var old4, old6 string
			if cur.Report != nil {
				old4, old6 = cur.Report.IPv4, cur.Report.IPv6
			}
			if old4 != rep.IPv4 || old6 != rep.IPv6 {
				a.store.UpdateServerIP(uuid, rep.IPv4, rep.IPv6)
			}
		}
	}

	// 刷新月度流量基准（跨月自动重置）
	if srv, err := a.store.GetServerAdmin(uuid); err == nil {
		a.store.MonthlyTrafficRecord(uuid, rep.NetTotalIn, rep.NetTotalOut)
		// 更新内存中的流量统计（供公开 API 展示）
		used := monthlyTrafficUsed(*srv, &rep)
		srv.Billing.TrafficUsed = used
		if srv.Billing.TrafficLimit > 0 {
			srv.Billing.TrafficRemaining = srv.Billing.TrafficLimit - used
			if srv.Billing.TrafficRemaining < 0 {
				srv.Billing.TrafficRemaining = 0
			}
		} else {
			srv.Billing.TrafficRemaining = -1
		}
	}
	now := time.Now().Unix()
	a.hub.Ingest(uuid, &rep, now, func(sid string, p model.MinutePoint) error {
		return a.store.SaveMinute(sid, p)
	})
	writeJSON(w, http.StatusOK, model.ReportResponse{OK: true, Interval: settings.ReportInterval})
}

func bearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return r.URL.Query().Get("token")
}

// ---- 访客接口（meerkat 原生）----

// handlePublicServers 返回所有服务器的公开信息与最新指标（含账单与流量）。
func (a *API) handlePublicServers(w http.ResponseWriter, r *http.Request) {
	statics, err := a.store.PublicServers(false)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	augmented := a.hub.Augment(statics)
	augmented = a.fillTraffic(augmented)
	writeJSON(w, http.StatusOK, augmented)
}

// fillTraffic 为公开列表计算流量用量（依据月度基准）。
func (a *API) fillTraffic(list []model.ServerPublic) []model.ServerPublic {
	for i := range list {
		rep, ok := a.hub.Latest(list[i].UUID)
		if !ok {
			continue
		}
		if srv, err := a.store.GetServerAdmin(list[i].UUID); err == nil {
			used := monthlyTrafficUsed(*srv, &rep)
			list[i].Billing.TrafficUsed = used
			if srv.Billing.TrafficLimit > 0 {
				list[i].Billing.TrafficRemaining = srv.Billing.TrafficLimit - used
				if list[i].Billing.TrafficRemaining < 0 {
					list[i].Billing.TrafficRemaining = 0
				}
			} else {
				list[i].Billing.TrafficRemaining = -1
			}
		}
	}
	return list
}

// handlePublicSite 返回站点基础信息。
func (a *API) handlePublicSite(w http.ResponseWriter, r *http.Request) {
	st, err := a.store.GetSettings()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_name": st.SiteName,
		"version":   a.version,
		"started":   a.startAt.Unix(),
		"theme":     a.store.GetActiveTheme(),
	})
}

// handleRecent 返回某服务器最近实时样本（页面刷新补帧）。
func (a *API) handleRecent(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/api/recent/")
	if uuid == "" {
		errJSON(w, http.StatusBadRequest, "missing uuid")
		return
	}
	writeJSON(w, http.StatusOK, a.hub.Recent(uuid))
}

// handleHistory 返回某服务器的历史分钟数据。
func (a *API) handleHistory(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		errJSON(w, http.StatusBadRequest, "missing uuid")
		return
	}
	hours := 24
	if v := atoiDefault(r.URL.Query().Get("hours"), 24); v > 0 && v <= 24*30 {
		hours = v
	}
	data, err := a.store.History(uuid, hours)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	if data == nil {
		data = []model.MinutePoint{}
	}
	writeJSON(w, http.StatusOK, data)
}

func atoiDefault(s string, def int) int {
	n := 0
	ok := false
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		ok = true
	}
	if !ok {
		return def
	}
	return n
}

// ---- Komari 兼容接口（供 Komari 生态主题如 LuminaPlus 调用）----

// handleKomariNodes 对应 Komari GET /api/nodes。
func (a *API) handleKomariNodes(w http.ResponseWriter, r *http.Request) {
	statics, err := a.store.PublicServers(false)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	augmented := a.fillTraffic(a.hub.Augment(statics))
	out := make([]model.KomariClient, 0, len(augmented))
	for _, p := range augmented {
		out = append(out, ToKomariClient(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleKomariPublic 对应 Komari GET /api/public（公开设置）。
func (a *API) handleKomariPublic(w http.ResponseWriter, r *http.Request) {
	st, _ := a.store.GetSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"site_name":     st.SiteName,
		"version":       a.version,
		"theme":         a.store.GetActiveTheme(),
		"announcements": []string{},
	})
}

// handleKomariVersion 对应 Komari GET /api/version。
func (a *API) handleKomariVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": a.version,
		"hash":    "",
	})
}

// handleKomariRecent 对应 Komari GET /api/recent/:uuid。
func (a *API) handleKomariRecent(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/api/recent/")
	if uuid == "" {
		errJSON(w, http.StatusBadRequest, "uuid is required")
		return
	}
	reports := a.hub.Recent(uuid)
	out := make([]model.KomariRecord, 0, len(reports))
	for _, rep := range reports {
		out = append(out, model.KomariRecord{
			Time:           time.Now().UTC(),
			Cpu:            float32(rep.CPUUsage),
			Ram:            int64(rep.MemUsed),
			RamTotal:       int64(rep.MemTotal),
			Swap:           int64(rep.SwapUsed),
			SwapTotal:      int64(rep.SwapTotal),
			Load:           float32(rep.Load1),
			Disk:           int64(rep.DiskUsed),
			DiskTotal:      int64(rep.DiskTotal),
			NetIn:          int64(rep.NetIn),
			NetOut:         int64(rep.NetOut),
			NetTotalUp:     int64(rep.NetTotalOut),
			NetTotalDown:   int64(rep.NetTotalIn),
			Process:        rep.ProcCount,
			Connections:    rep.TCPConns,
			ConnectionsUdp: rep.UDPConns,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleKomariRecordsLoad 对应 Komari GET /api/records/load?uuid=&load_type=&hours=。
func (a *API) handleKomariRecordsLoad(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		errJSON(w, http.StatusBadRequest, "uuid is required")
		return
	}
	hours := atoiDefault(r.URL.Query().Get("hours"), 4)
	if hours <= 0 || hours > 24*30 {
		hours = 4
	}
	hist, err := a.store.History(uuid, hours)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	out := make([]model.KomariRecord, 0, len(hist))
	for _, p := range hist {
		srv, _ := a.store.GetServerAdmin(uuid)
		var ramUsed, diskUsed int64
		if srv != nil && srv.Report != nil {
			ramUsed = int64(srv.Report.MemTotal) * int64(p.MemPct) / 100
			diskUsed = int64(srv.Report.DiskTotal) * int64(p.DiskPct) / 100
		}
		out = append(out, model.KomariRecord{
			Time:        time.Unix(p.Time, 0).UTC(),
			Cpu:         float32(p.CPU),
			Ram:         ramUsed,
			RamTotal:    int64(0),
			Load:        float32(p.Load1),
			Disk:        diskUsed,
			NetIn:       int64(p.NetIn),
			NetOut:      int64(p.NetOut),
			Connections: p.TCP,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": out,
		"count":   len(out),
	})
}

// ---- 管理接口 ----

// requireAdmin 包装管理接口的登录校验。
func (a *API) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie("mk_session")
		if err != nil || !a.session.valid(ck.Value) {
			errJSON(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	}
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "参数错误")
		return
	}
	if !a.store.VerifyAdmin(req.Username, req.Password) {
		time.Sleep(time.Second) // 轻量防爆破
		errJSON(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok := a.session.create()
	http.SetCookie(w, &http.Cookie{
		Name: "mk_session", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	// 登录通知（异步）
	go a.notifier.NotifyLogin(req.Username, clientIP(r), r.UserAgent())
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie("mk_session"); err == nil {
		a.session.destroy(ck.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "mk_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleAdminServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		statics, err := a.store.ListServers()
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "读取失败")
			return
		}
		out := a.hub.AugmentFull(statics)
		// 补充流量统计
		for i := range out {
			if rep, ok := a.hub.Latest(out[i].UUID); ok {
				used := monthlyTrafficUsed(out[i], &rep)
				out[i].Billing.TrafficUsed = used
				if out[i].Billing.TrafficLimit > 0 {
					out[i].Billing.TrafficRemaining = out[i].Billing.TrafficLimit - used
					if out[i].Billing.TrafficRemaining < 0 {
						out[i].Billing.TrafficRemaining = 0
					}
				} else {
					out[i].Billing.TrafficRemaining = -1
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req struct {
			Name   string `json:"name"`
			Note   string `json:"note"`
			Tag    string `json:"tag"`
			Group  string `json:"group"`
			Region string `json:"region"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		srv, err := a.store.CreateServer(req.Name, req.Note, req.Tag, req.Group, req.Region)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srv)
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAdminServerOne(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admin/servers/"), "/")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name                 *string        `json:"name"`
			Note                 *string        `json:"note"`
			Tag                  *string        `json:"tag"`
			Group                *string        `json:"group"`
			Region               *string        `json:"region"`
			SortOrder            *int           `json:"sort_order"`
			Hidden               *bool          `json:"hidden"`
			Billing              *model.Billing `json:"billing"`
			OfflineNotifyEnabled *bool          `json:"offline_notify_enabled"`
			OfflineGraceSeconds  *int           `json:"offline_grace_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		patch := ServerPatch{
			Name: req.Name, Note: req.Note, Tag: req.Tag, Group: req.Group, Region: req.Region,
			SortOrder: req.SortOrder, Hidden: req.Hidden, Billing: req.Billing,
			OfflineNotifyEnabled: req.OfflineNotifyEnabled, OfflineGraceSeconds: req.OfflineGraceSeconds,
		}
		if err := a.store.UpdateServer(uuid, patch); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := a.store.DeleteServer(uuid); err != nil {
			errJSON(w, http.StatusInternalServerError, "删除失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAdminResetToken(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admin/servers/"), "/reset-token")
	tok, err := a.store.ResetToken(uuid)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (a *API) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st, err := a.store.GetSettings()
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "读取失败")
			return
		}
		writeJSON(w, http.StatusOK, st)
	case http.MethodPut:
		var st model.SiteSettings
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		if err := a.store.SaveSettings(st); err != nil {
			errJSON(w, http.StatusInternalServerError, "保存失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "参数错误")
		return
	}
	if err := a.store.ChangeAdminPassword(req.Old, req.New); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- 管理接口：通知 ----

func (a *API) handleAdminNotify(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ns := a.store.GetNotifySettings()
		writeJSON(w, http.StatusOK, ns)
	case http.MethodPut:
		var ns model.NotifySettings
		if err := json.NewDecoder(r.Body).Decode(&ns); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		if ns.TGEndpoint == "" {
			ns.TGEndpoint = "https://api.telegram.org/bot"
		}
		if err := a.store.SaveNotifySettings(ns); err != nil {
			errJSON(w, http.StatusInternalServerError, "保存失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAdminNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ns := a.store.GetNotifySettings()
	if err := a.notifier.SendTest(ns); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleAdminLoadRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := a.store.ListLoadRules()
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "读取失败")
			return
		}
		if rules == nil {
			rules = []model.LoadRule{}
		}
		writeJSON(w, http.StatusOK, rules)
	case http.MethodPost:
		var rule model.LoadRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		if strings.TrimSpace(rule.Name) == "" {
			errJSON(w, http.StatusBadRequest, "规则名称不能为空")
			return
		}
		if rule.Metric != "cpu" && rule.Metric != "ram" {
			rule.Metric = "cpu"
		}
		if rule.Threshold <= 0 || rule.Threshold > 100 {
			rule.Threshold = 80
		}
		if rule.Ratio <= 0 || rule.Ratio > 1 {
			rule.Ratio = 0.8
		}
		if rule.IntervalMin < 1 {
			rule.IntervalMin = 2
		}
		id, err := a.store.CreateLoadRule(rule)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		rule.ID = id
		writeJSON(w, http.StatusOK, rule)
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAdminLoadRuleOne(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admin/loadrules/"), "/")
	id := int64(atoiDefault(idStr, 0))
	if id <= 0 {
		errJSON(w, http.StatusBadRequest, "无效 ID")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var rule model.LoadRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		rule.ID = id
		if err := a.store.UpdateLoadRule(rule); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := a.store.DeleteLoadRule(id); err != nil {
			errJSON(w, http.StatusInternalServerError, "删除失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---- 管理接口：主题 ----

func (a *API) handleAdminThemeList(w http.ResponseWriter, r *http.Request) {
	themes := a.srv().ListThemes()
	if themes == nil {
		themes = []model.ThemeInfo{}
	}
	// 内置主题放最前
	builtin := model.ThemeInfo{
		Name: "Meerkat (built-in)", Short: "", Version: a.version,
		Author: "meerkat-monitor", Description: "内置默认主题",
		Active: a.store.GetActiveTheme() == "",
	}
	writeJSON(w, http.StatusOK, append([]model.ThemeInfo{builtin}, themes...))
}

func (a *API) handleAdminThemeUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// 限制 100MB
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "请选择 ZIP 主题包（form 字段名 file）")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "读取上传数据失败")
		return
	}
	ti, err := a.srv().InstallThemeFromZIP(data)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ti)
}

func (a *API) handleAdminThemeSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Theme string `json:"theme"` // 空 = 内置
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "参数错误")
		return
	}
	if req.Theme != "" {
		if !themeShortRe.MatchString(req.Theme) {
			errJSON(w, http.StatusBadRequest, "非法主题名")
			return
		}
		if _, err := readThemeManifest(fmt.Sprintf("%s/%s", a.srv().themesDir(), req.Theme)); err != nil {
			errJSON(w, http.StatusBadRequest, "主题不存在或清单无效")
			return
		}
	}
	if err := a.store.SetActiveTheme(req.Theme); err != nil {
		errJSON(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) handleAdminThemeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Theme string `json:"theme"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "参数错误")
		return
	}
	if err := a.srv().DeleteTheme(req.Theme); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- 仪表盘统计 ----

// handleAdminDashboard 返回仪表盘统计数据。
func (a *API) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	servers, err := a.store.ListServers()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	augmented := a.hub.AugmentFull(servers)

	total, online := len(augmented), 0
	var dbBytes int64 = dbSizeBytes(a.store.Path())
	now := time.Now()

	type expiringItem struct {
		Name string `json:"name"`
		Days int    `json:"days"`
	}
	expiring := []expiringItem{}

	for _, s := range augmented {
		if s.Online {
			online++
		}
		if s.Billing.ExpiredAt > 0 {
			days := int(time.Unix(s.Billing.ExpiredAt, 0).Sub(now).Hours() / 24)
			if days <= 30 {
				expiring = append(expiring, expiringItem{Name: s.Name, Days: days})
			}
		}
	}

	// 24h 流量走势（全部服务器汇总）+ 排行
	hist24, _ := a.store.AllRecentHistory(24 * 60)
	type trafficPoint struct {
		Time   int64   `json:"time"`
		NetIn  float64 `json:"net_in"`
		NetOut float64 `json:"net_out"`
	}
	trafficSeries := []trafficPoint{}
	perServer := map[string]float64{}
	if len(hist24) > 0 {
		// 按时间桶聚合全部服务器
		buckets := map[int64]*trafficPoint{}
		for sid, points := range hist24 {
			for _, p := range points {
				b, ok := buckets[p.Time]
				if !ok {
					b = &trafficPoint{Time: p.Time}
					buckets[p.Time] = b
				}
				b.NetIn += p.NetIn
				b.NetOut += p.NetOut
				perServer[sid] += p.NetIn + p.NetOut
			}
		}
		for _, b := range buckets {
			trafficSeries = append(trafficSeries, *b)
		}
		// 排序
		for i := 1; i < len(trafficSeries); i++ {
			for j := i; j > 0 && trafficSeries[j].Time < trafficSeries[j-1].Time; j-- {
				trafficSeries[j], trafficSeries[j-1] = trafficSeries[j-1], trafficSeries[j]
			}
		}
	}

	// 流量 / CPU / 内存 排行
	type rankItem struct {
		UUID  string  `json:"uuid"`
		Name  string  `json:"name"`
		Value float64 `json:"value"`
		Peak  float64 `json:"peak"`
	}
	var trafficRank, cpuRank, memRank []rankItem
	nameOf := func(uuid string) string {
		for _, s := range augmented {
			if s.UUID == uuid {
				return s.Name
			}
		}
		return uuid
	}
	for sid, total := range perServer {
		peak := 0.0
		for _, p := range hist24[sid] {
			if p.NetIn+p.NetOut > peak {
				peak = p.NetIn + p.NetOut
			}
		}
		trafficRank = append(trafficRank, rankItem{UUID: sid, Name: nameOf(sid), Value: total, Peak: peak})
	}
	cpuAgg := map[string][]float64{}
	memAgg := map[string][]float64{}
	for sid, points := range hist24 {
		for _, p := range points {
			cpuAgg[sid] = append(cpuAgg[sid], p.CPU)
			memAgg[sid] = append(memAgg[sid], p.MemPct)
		}
	}
	avgOf := func(vals []float64) float64 {
		if len(vals) == 0 {
			return 0
		}
		s := 0.0
		for _, v := range vals {
			s += v
		}
		return s / float64(len(vals))
	}
	for sid, vals := range cpuAgg {
		peak := 0.0
		for _, v := range vals {
			if v > peak {
				peak = v
			}
		}
		cpuRank = append(cpuRank, rankItem{UUID: sid, Name: nameOf(sid), Value: avgOf(vals), Peak: peak})
	}
	for sid, vals := range memAgg {
		peak := 0.0
		for _, v := range vals {
			if v > peak {
				peak = v
			}
		}
		memRank = append(memRank, rankItem{UUID: sid, Name: nameOf(sid), Value: avgOf(vals), Peak: peak})
	}
	sortRank := func(items []rankItem) {
		for i := 1; i < len(items); i++ {
			for j := i; j > 0 && items[j].Value > items[j-1].Value; j-- {
				items[j], items[j-1] = items[j-1], items[j]
			}
		}
		if len(items) > 5 {
			items = items[:5]
		}
	}
	sortRank(trafficRank)
	sortRank(cpuRank)
	sortRank(memRank)
	if trafficRank == nil {
		trafficRank = []rankItem{}
	}
	if cpuRank == nil {
		cpuRank = []rankItem{}
	}
	if memRank == nil {
		memRank = []rankItem{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":          total,
		"online":         online,
		"offline":        total - online,
		"db_bytes":       dbBytes,
		"expiring":       expiring,
		"traffic_series": trafficSeries,
		"traffic_rank":   trafficRank,
		"cpu_rank":       cpuRank,
		"mem_rank":       memRank,
		"version":        a.version,
		"started":        a.startAt.Unix(),
	})
}

// srv 返回所属 Server 实例（主题操作需要访问文件系统）。
func (a *API) srv() *Server { return a.srvRef }

// osStat 包装（避免在别处直接引 os）。
var _ = os.Stat
