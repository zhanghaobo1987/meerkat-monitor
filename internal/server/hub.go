package server

import (
	"encoding/json"
	"log"
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

// AugmentFull 同 Augment，但作用于管理视角列表（保留 token）。
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

// ServeWS 升级 HTTP 连接并开始推送。
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
