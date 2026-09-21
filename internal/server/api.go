package server

import (
	"encoding/json"
	"net/http"
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
	store   *Store
	hub     *Hub
	session *sessionMgr
	version string
	startAt time.Time
}

// NewAPI 构造 API。
func NewAPI(store *Store, hub *Hub, version string) *API {
	return &API{store: store, hub: hub, session: newSessionMgr(), version: version, startAt: time.Now()}
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

	// 静态信息有变化时更新档案
	if rep.OS != "" || rep.CPUModel != "" {
		if cur, err := a.store.GetServerAdmin(uuid); err == nil {
			ai := cur.AgentInfo
			if ai.OS != rep.OS || ai.CPUModel != rep.CPUModel || ai.CPUCores != rep.CPUCores ||
				ai.Platform != rep.Platform || ai.KernelVer != rep.KernelVer || ai.Version != rep.Version ||
				ai.Arch != rep.Arch || ai.Virt != rep.Virt || ai.GPUModel != rep.GPUModel {
				a.store.UpdateAgentStatic(uuid, &rep)
			}
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
	// 兼容 URL 参数（安装脚本场景）
	return r.URL.Query().Get("token")
}

// ---- 访客接口 ----

// handlePublicServers 返回所有服务器的公开信息与最新指标。
func (a *API) handlePublicServers(w http.ResponseWriter, r *http.Request) {
	statics, err := a.store.PublicServers()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "读取失败")
		return
	}
	writeJSON(w, http.StatusOK, a.hub.Augment(statics))
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
	})
}

// handleRecent 返回某服务器最近实时样本（用于页面刷新补帧）。
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
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Note string `json:"note"`
			Tag  string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		srv, err := a.store.CreateServer(req.Name, req.Note, req.Tag)
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
	uuid := strings.TrimPrefix(r.URL.Path, "/api/admin/servers/")
	uuid = strings.TrimSuffix(uuid, "/reset-token")
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name      string `json:"name"`
			Note      string `json:"note"`
			Tag       string `json:"tag"`
			SortOrder int    `json:"sort_order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "参数错误")
			return
		}
		if err := a.store.UpdateServer(uuid, req.Name, req.Note, req.Tag, req.SortOrder); err != nil {
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
