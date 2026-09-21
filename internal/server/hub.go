package server

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// Hub 维护所有服务器的实时状态（内存环形缓冲）并负责 WebSocket 广播。
type Hub struct {
	mu       sync.RWMutex
	latest   map[string]*model.Report  // serverUUID -> 最新上报
	lastSeen map[string]int64          // serverUUID -> 最近上报时间戳
	recent   map[string][]model.Report // 最近 N 条，供前端补帧
	seq      map[string]*reportRing    // 分钟聚合器

	clients map[*wsClient]struct{}
	addCh   chan *wsClient
	delCh   chan *wsClient
	bcCh    chan *broadcastMsg

	keepCount int // 内存保留的实时样本数
}

// reportRing 负责“每分钟一条”的聚合落库。
type reportRing struct {
	bucketStart int64
	sumCPU      float64
	sumMemPct   float64
	sumDiskPct  float64
	sumNetIn    float64
	sumNetOut   float64
	sumLoad     float64
	sumTCP      float64
	sumUptime   float64
	n           int
}

type broadcastMsg struct {
	payload []byte
}

const recentKeep = 120 // 2 秒间隔约等于最近 4 分钟样本

// NewHub 创建广播中心并启动内部协程。
func NewHub() *Hub {
	h := &Hub{
		latest:    make(map[string]*model.Report),
		lastSeen:  make(map[string]int64),
		recent:    make(map[string][]model.Report),
		seq:       make(map[string]*reportRing),
		clients:   make(map[*wsClient]struct{}),
		addCh:     make(chan *wsClient, 16),
		delCh:     make(chan *wsClient, 16),
		bcCh:      make(chan *broadcastMsg, 128),
		keepCount: recentKeep,
	}
	go h.run()
	return h
}

func (h *Hub) run() {
	for {
		select {
		case c := <-h.addCh:
			h.clients[c] = struct{}{}
		case c := <-h.delCh:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}
		case m := <-h.bcCh:
			for c := range h.clients {
				select {
				case c.send <- m.payload:
				default: // 慢客户端直接丢弃，避免阻塞
				}
			}
		}
	}
}

// Ingest 接收一次 Agent 上报：更新实时状态、推进分钟聚合，并广播。
// flush 回调由上层注入（写 SQLite）。
func (h *Hub) Ingest(uuid string, r *model.Report, now int64, flush func(uuid string, p model.MinutePoint) error) {
	h.mu.Lock()
	h.latest[uuid] = r
	h.lastSeen[uuid] = now

	// 维护最近样本环形缓冲
	buf := append(h.recent[uuid], *r)
	if len(buf) > h.keepCount {
		buf = buf[len(buf)-h.keepCount:]
	}
	h.recent[uuid] = buf

	// 分钟聚合
	bucket := now - now%60
	rg := h.seq[uuid]
	if rg == nil || rg.bucketStart != bucket {
		if rg != nil && rg.n > 0 {
			flushPoint(uuid, rg, flush)
		}
		rg = &reportRing{bucketStart: bucket}
		h.seq[uuid] = rg
	}
	memPct, diskPct := percent(r.MemUsed, r.MemTotal), percent(r.DiskUsed, r.DiskTotal)
	rg.sumCPU += r.CPUUsage
	rg.sumMemPct += memPct
	rg.sumDiskPct += diskPct
	rg.sumNetIn += float64(r.NetIn)
	rg.sumNetOut += float64(r.NetOut)
	rg.sumLoad += r.Load1
	rg.sumTCP += float64(r.TCPConns)
	rg.sumUptime += float64(r.Uptime)
	rg.n++
	h.mu.Unlock()

	if payload, err := json.Marshal(map[string]any{
		"type":  "update",
		"uuid":  uuid,
		"time":  now,
		"order": "single",
		"data":  r,
	}); err == nil {
		select {
		case h.bcCh <- &broadcastMsg{payload: payload}:
		default:
		}
	}
}

func flushPoint(uuid string, rg *reportRing, flush func(string, model.MinutePoint) error) {
	n := float64(rg.n)
	p := model.MinutePoint{
		Time:    rg.bucketStart,
		CPU:     rg.sumCPU / n,
		MemPct:  rg.sumMemPct / n,
		DiskPct: rg.sumDiskPct / n,
		NetIn:   rg.sumNetIn / n,
		NetOut:  rg.sumNetOut / n,
		Load1:   rg.sumLoad / n,
		TCP:     int(rg.sumTCP / n),
		Uptime:  uint64(rg.sumUptime / n),
	}
	if err := flush(uuid, p); err != nil {
		log.Printf("[hub] 分钟数据落库失败: %v", err)
	}
}

// percent 安全计算百分比。
func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// Online 判断服务器是否在线（60 秒内有上报视为在线）。
func (h *Hub) Online(uuid string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return time.Now().Unix()-h.lastSeen[uuid] < 60
}

// Augment 为静态列表补充在线状态与最新指标。
func (h *Hub) Augment(statics []model.ServerPublic) []model.ServerPublic {
	now := time.Now().Unix()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]model.ServerPublic, 0, len(statics))
	for _, st := range statics {
		st.Online = now-h.lastSeen[st.UUID] < 60
		st.LastSeen = h.lastSeen[st.UUID]
		if r, ok := h.latest[st.UUID]; ok {
			cp := *r
			st.Report = &cp
		}
		out = append(out, st)
	}
	return out
}

// AugmentFull 同 Augment，但作用于管理视角列表（保留 token 与通知配置）。
func (h *Hub) AugmentFull(admins []model.ServerAdmin) []model.ServerAdmin {
	now := time.Now().Unix()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]model.ServerAdmin, 0, len(admins))
	for _, st := range admins {
		st.Online = now-h.lastSeen[st.UUID] < 60
		st.LastSeen = h.lastSeen[st.UUID]
		if r, ok := h.latest[st.UUID]; ok {
			cp := *r
			st.Report = &cp
		}
		out = append(out, st)
	}
	return out
}

// Recent 返回某台服务器最近的实时样本。
func (h *Hub) Recent(uuid string) []model.Report {
	h.mu.RLock()
	defer h.mu.RUnlock()
	buf := h.recent[uuid]
	out := make([]model.Report, len(buf))
	copy(out, buf)
	return out
}

// Latest 返回某台服务器的最新上报快照。
func (h *Hub) Latest(uuid string) (model.Report, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.latest[uuid]
	if !ok {
		return model.Report{}, false
	}
	return *r, true
}

// OnlineUUIDs 返回当前在线的服务器 UUID 列表。
func (h *Hub) OnlineUUIDs() []string {
	now := time.Now().Unix()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := []string{}
	for uuid, seen := range h.lastSeen {
		if now-seen < 60 {
			out = append(out, uuid)
		}
	}
	return out
}

// LatestAll 返回全部最新上报快照副本。
func (h *Hub) LatestAll() map[string]model.Report {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]model.Report, len(h.latest))
	for k, v := range h.latest {
		out[k] = *v
	}
	return out
}

// ---- Komari 兼容转换 ----

// ToKomariReport 将 meerkat 上报转换为 Komari v2.Report 形状。
func ToKomariReport(r *model.Report) model.KomariReport {
	usage := r.CPUUsage
	if usage == 0 {
		usage = 0.01 // 与 Komari 服务端行为一致，避免主题渲染 0
	}
	return model.KomariReport{
		CPU: model.KomariCPU{
			Name:  r.AgentInfo.CPUModel,
			Cores: r.AgentInfo.CPUCores,
			Arch:  r.AgentInfo.Arch,
			Usage: usage,
		},
		Ram:  model.KomariAmount{Total: int64(r.MemTotal), Used: int64(r.MemUsed)},
		Swap: model.KomariAmount{Total: int64(r.SwapTotal), Used: int64(r.SwapUsed)},
		Load: model.KomariLoad{Load1: r.Load1, Load5: r.Load5, Load15: r.Load15},
		Disk: model.KomariAmount{Total: int64(r.DiskTotal), Used: int64(r.DiskUsed)},
		Network: model.KomariNetwork{
			Up: int64(r.NetOut), Down: int64(r.NetIn),
			TotalUp: int64(r.NetTotalOut), TotalDown: int64(r.NetTotalIn),
		},
		Connections: model.KomariConnections{TCP: r.TCPConns, UDP: r.UDPConns},
		Uptime:      int64(r.Uptime),
		Process:     r.ProcCount,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// komariCycleMonths 将周期字符串映射为 Komari 的数字周期。
func komariCycleMonths(cycle string) int {
	switch cycle {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "semiannual":
		return 6
	case "yearly":
		return 12
	}
	return 0
}

// ToKomariClient 将 meerkat 服务器档案转换为 Komari Client 形状。
func ToKomariClient(p model.ServerPublic) model.KomariClient {
	var expired *time.Time
	if p.Billing.ExpiredAt > 0 {
		t := time.Unix(p.Billing.ExpiredAt, 0).UTC()
		expired = &t
	}
	created := time.Unix(p.CreatedAt, 0).UTC()
	updated := created
	if p.LastSeen > 0 {
		updated = time.Unix(p.LastSeen, 0).UTC()
	}
	kc := model.KomariClient{
		UUID:             p.UUID,
		Name:             p.Name,
		Region:           p.Region,
		Group:            p.Group,
		Tags:             p.Tag,
		CpuName:          p.AgentInfo.CPUModel,
		Virtualization:   p.AgentInfo.Virt,
		Arch:             p.AgentInfo.Arch,
		CpuCores:         p.AgentInfo.CPUCores,
		OS:               platformOf(p),
		KernelVersion:    p.AgentInfo.KernelVer,
		GpuName:          p.AgentInfo.GPUModel,
		MemTotal:         int64(memTotalOf(p)),
		SwapTotal:        int64(swapTotalOf(p)),
		DiskTotal:        int64(diskTotalOf(p)),
		Weight:           p.SortOrder,
		Price:            p.Billing.Price,
		BillingCycle:     komariCycleMonths(p.Billing.BillingCycle),
		Currency:         p.Billing.Currency,
		ExpiredAt:        expired,
		TrafficLimit:     p.Billing.TrafficLimit,
		TrafficLimitType: p.Billing.TrafficLimitType,
		CreatedAt:        created,
		UpdatedAt:        updated,
	}
	kc.Account = model.KomariAccount{
		ExpiredAt:        expired,
		Price:            p.Billing.Price,
		BillingCycle:     komariCycleMonths(p.Billing.BillingCycle),
		Currency:         p.Billing.Currency,
		TrafficLimit:     p.Billing.TrafficLimit,
		TrafficLimitType: p.Billing.TrafficLimitType,
		TrafficUsed:      p.Billing.TrafficUsed,
		TrafficRemaining: p.Billing.TrafficRemaining,
	}
	return kc
}

func platformOf(p model.ServerPublic) string {
	if p.AgentInfo.OS == "" {
		return ""
	}
	if p.AgentInfo.Platform != "" {
		return p.AgentInfo.Platform
	}
	return p.AgentInfo.OS
}
func memTotalOf(p model.ServerPublic) uint64 {
	if p.Report != nil {
		return p.Report.MemTotal
	}
	return 0
}
func swapTotalOf(p model.ServerPublic) uint64 {
	if p.Report != nil {
		return p.Report.SwapTotal
	}
	return 0
}
func diskTotalOf(p model.ServerPublic) uint64 {
	if p.Report != nil {
		return p.Report.DiskTotal
	}
	return 0
}

// ---- WebSocket ----

// wsClient 一条 WebSocket 连接。
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true }, // 公开面板数据，允许任意来源
}

// ServeWS 升级 HTTP 连接并开始推送（meerkat 内置面板协议：服务端主动推 update）。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsClient{conn: conn, send: make(chan []byte, 64)}
	h.addCh <- c

	go h.wsWriter(c)
	h.wsReader(c)
}

func (h *Hub) wsReader(c *wsClient) {
	defer func() {
		h.delCh <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil })
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (h *Hub) wsWriter(c *wsClient) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case payload, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ServeKomariWS 实现 Komari 的 /api/clients WebSocket 拉取协议：
// 浏览器连接后周期发送 "get"（全部）或 "get <uuid>"，
// 服务端返回 {"status":"success","data":{"online":[...],"data":{uuid:Report}}}。
func (s *Server) ServeKomariWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(256)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		message := string(data)
		uuid := ""
		if message != "get" {
			if len(message) > 4 && message[:4] == "get " {
				uuid = trimSpace(message[4:])
			} else {
				_ = conn.WriteJSON(map[string]any{"status": "error", "error": "Invalid message"})
				continue
			}
		}

		online := []string{}
		dataMap := map[string]model.KomariReport{}
		h := s.hub
		for _, id := range h.OnlineUUIDs() {
			if uuid != "" && id != uuid {
				continue
			}
			online = append(online, id)
		}
		for id, rep := range h.LatestAll() {
			if uuid != "" && id != uuid {
				continue
			}
			dataMap[id] = ToKomariReport(&rep)
		}
		_ = conn.WriteJSON(map[string]any{
			"status": "success",
			"data": map[string]any{
				"online": online,
				"data":   dataMap,
			},
		})
	}
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// clientIP 从请求提取客户端出口 IP（优先反代头）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个（最原始客户端）
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return trimSpace(xff[:i])
			}
		}
		return trimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return trimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isIPv4 判断字符串是否为 IPv4 地址。
func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}
