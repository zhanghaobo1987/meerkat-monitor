package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/meerkat-monitor/meerkat/internal/model"
)

// Notifier 是通知引擎：周期检查离线/负载/过期/流量并发送通知。
type Notifier struct {
	store *Store
	hub   *Hub
	cli   *http.Client
	stop  chan struct{}
}

// NewNotifier 创建并启动通知引擎。
func NewNotifier(store *Store, hub *Hub) *Notifier {
	n := &Notifier{
		store: store,
		hub:   hub,
		cli:   &http.Client{Timeout: 15 * time.Second},
		stop:  make(chan struct{}),
	}
	go n.run()
	return n
}

// Stop 停止通知引擎。
func (n *Notifier) Stop() { close(n.stop) }

func (n *Notifier) run() {
	// 启动先歇 30 秒，等 Agent 陆续上线，避免误报离线
	select {
	case <-time.After(30 * time.Second):
	case <-n.stop:
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		n.check()
		select {
		case <-n.stop:
			return
		case <-t.C:
		}
	}
}

// check 执行一轮全部检查。
func (n *Notifier) check() {
	ns := n.store.GetNotifySettings()
	if !ns.Enabled {
		return
	}
	n.checkOffline(ns)
	n.checkLoad(ns)
	n.checkExpiry(ns)
	n.checkTraffic(ns)
}

// checkOffline 检查服务器离线（宽限期 + 冷却 30 分钟）。
func (n *Notifier) checkOffline(ns model.NotifySettings) {
	servers, err := n.store.ListServers()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, s := range servers {
		if !s.OfflineNotifyEnabled {
			continue
		}
		online := now-s.LastSeen < 60
		if online {
			continue
		}
		// 从最后上报时间起算宽限期
		offlineFor := now - s.LastSeen
		if s.LastSeen == 0 {
			continue // 从未上报过（刚创建），不告警
		}
		if offlineFor < int64(s.OfflineGraceSeconds) {
			continue
		}
		// 冷却：30 分钟内不重复告警
		if now-s.LastOfflineNotify < 30*60 {
			continue
		}
		n.Send(ns, model.NotifyMessage{
			Event:  "offline",
			Client: s.Name,
			Detail: fmt.Sprintf("已离线 %d 秒（宽限期 %d 秒）", offlineFor, s.OfflineGraceSeconds),
			Emoji:  "🔴",
		})
		n.store.MarkOfflineNotified(s.UUID, now)
	}
}

// checkLoad 检查负载告警规则（基于最近 10 分钟分钟数据的时间占比）。
func (n *Notifier) checkLoad(ns model.NotifySettings) {
	rules, err := n.store.ListLoadRules()
	if err != nil {
		return
	}
	if len(rules) == 0 {
		return
	}
	hist, err := n.store.AllRecentHistory(10)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, rule := range rules {
		if now-rule.LastNotify < int64(rule.IntervalMin)*60 {
			continue
		}
		points := hist[rule.ServerUUID]
		if len(points) == 0 {
			continue
		}
		over := 0
		for _, p := range points {
			v := p.CPU
			if rule.Metric == "ram" {
				v = p.MemPct
			}
			if v >= rule.Threshold {
				over++
			}
		}
		ratio := float64(over) / float64(len(points))
		if ratio < rule.Ratio {
			continue
		}
		// 找服务器名
		name := rule.ServerUUID
		if s, err := n.store.GetServerAdmin(rule.ServerUUID); err == nil {
			name = s.Name
		}
		n.Send(ns, model.NotifyMessage{
			Event:  "load",
			Client: name,
			Detail: fmt.Sprintf("%s 使用率 %.1f%% ≥ %.0f%%（时间占比 %.0f%%）",
				strings.ToUpper(rule.Metric), avgMetric(points, rule.Metric), rule.Threshold, ratio*100),
			Emoji: "⚠️",
		})
		n.store.MarkLoadRuleNotified(rule.ID, now)
	}
}

func avgMetric(points []model.MinutePoint, metric string) float64 {
	if len(points) == 0 {
		return 0
	}
	var sum float64
	for _, p := range points {
		if metric == "ram" {
			sum += p.MemPct
		} else {
			sum += p.CPU
		}
	}
	return sum / float64(len(points))
}

// checkExpiry 检查服务器到期提醒（提前 N 天，每天最多一次）。
func (n *Notifier) checkExpiry(ns model.NotifySettings) {
	if !ns.ExpiryEnabled || ns.ExpiryDays <= 0 {
		return
	}
	servers, err := n.store.ListServers()
	if err != nil {
		return
	}
	now := time.Now()
	var expiring []string
	dayKey := now.Format("2006-01-02")
	for _, s := range servers {
		if s.Billing.ExpiredAt <= 0 {
			continue
		}
		days := int(time.Until(time.Unix(s.Billing.ExpiredAt, 0)).Hours() / 24)
		if days <= ns.ExpiryDays {
			expiring = append(expiring, fmt.Sprintf("%s（剩 %d 天）", s.Name, days))
		}
	}
	if len(expiring) == 0 {
		return
	}
	// 每天只提醒一次：以 settings 表记录上次提醒日期
	lastKey, _ := n.store.GetSetting("expiry_last_notify_day")
	if lastKey == dayKey {
		return
	}
	n.Send(ns, model.NotifyMessage{
		Event:  "expiry",
		Client: strings.Join(expiring, ", "),
		Detail: fmt.Sprintf("以下服务器将在 %d 天内到期", ns.ExpiryDays),
		Emoji:  "⏰",
	})
	_ = n.store.SetSetting("expiry_last_notify_day", dayKey)
}

// trafficNotifyState 记录每台服务器流量提醒的最近梯度档位（内存态，5% 梯度）。
var trafficNotifyState = map[string]int{}
var trafficMu = make(chan struct{}, 1)

// checkTraffic 检查流量用量（百分比阈值，5% 梯度持续提醒）。
func (n *Notifier) checkTraffic(ns model.NotifySettings) {
	if ns.TrafficPct <= 0 {
		return
	}
	servers, err := n.store.ListServers()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, s := range servers {
		if s.Billing.TrafficLimit <= 0 {
			continue // 未设置流量阈值不提醒
		}
		rep, ok := n.hub.Latest(s.UUID)
		if !ok {
			continue
		}
		used := monthlyTrafficUsed(s, &rep)
		pct := int(used * 100 / s.Billing.TrafficLimit)
		if pct < ns.TrafficPct {
			continue
		}
		// 5% 梯度：只在跨过新的 5% 档位时提醒
		level := pct / 5
		trafficMu <- struct{}{}
		last, seen := trafficNotifyState[s.UUID]
		trafficNotifyState[s.UUID] = level
		<-trafficMu
		if seen && last == level {
			continue
		}
		n.Send(ns, model.NotifyMessage{
			Event:  "traffic",
			Client: s.Name,
			Detail: fmt.Sprintf("流量已用 %s / %s（%d%%）",
				humanBytes(used), humanBytes(s.Billing.TrafficLimit), pct),
			Emoji: "📶",
		})
		_ = now
	}
}

// monthlyTrafficUsed 计算本计费周期的已用流量（依据月度基准）。
func monthlyTrafficUsed(s model.ServerAdmin, rep *model.Report) int64 {
	used := int64(rep.NetTotalIn) + int64(rep.NetTotalOut) - int64(s.MonthlyBaseIn) - int64(s.MonthlyBaseOut)
	if used < 0 {
		used = 0
	}
	switch s.Billing.TrafficLimitType {
	case "up":
		used = int64(rep.NetTotalOut) - int64(s.MonthlyBaseOut)
	case "down":
		used = int64(rep.NetTotalIn) - int64(s.MonthlyBaseIn)
	}
	if used < 0 {
		used = 0
	}
	return used
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ---- 发送 ----

// Send 渲染模板并通过配置的渠道发送一条通知。
func (n *Notifier) Send(ns model.NotifySettings, msg model.NotifyMessage) {
	if !ns.Enabled || ns.Channel == "none" {
		return
	}
	text := renderTemplate(ns.MessageTPL, msg)
	var err error
	switch ns.Channel {
	case "telegram":
		err = n.sendTelegram(ns, text)
	default:
		err = fmt.Errorf("未知通知渠道: %s", ns.Channel)
	}
	if err != nil {
		log.Printf("[notify] 发送失败(%s/%s): %v", ns.Channel, msg.Event, err)
	} else {
		log.Printf("[notify] 已发送 %s 通知: %s", msg.Event, msg.Client)
	}
}

// renderTemplate 渲染消息模板，支持 {{emoji}} {{event}} {{client}} {{detail}}。
func renderTemplate(tpl string, msg model.NotifyMessage) string {
	r := strings.NewReplacer(
		"{{emoji}}", msg.Emoji,
		"{{event}}", msg.Event,
		"{{client}}", msg.Client,
		"{{detail}}", msg.Detail,
	)
	out := r.Replace(tpl)
	if !strings.Contains(tpl, "{{detail}}") && msg.Detail != "" {
		out += "\n" + msg.Detail
	}
	return out
}

// sendTelegram 通过 Bot API 发送 Telegram 消息。
func (n *Notifier) sendTelegram(ns model.NotifySettings, text string) error {
	if ns.TGBotToken == "" || ns.TGChatID == "" {
		return fmt.Errorf("Telegram Bot Token / Chat ID 未配置")
	}
	endpoint := strings.TrimRight(ns.TGEndpoint, "/")
	if endpoint == "" {
		endpoint = "https://api.telegram.org/bot"
	}
	apiURL := endpoint + ns.TGBotToken + "/sendMessage"

	form := url.Values{}
	form.Set("chat_id", ns.TGChatID)
	form.Set("text", text)
	if ns.TGThreadID != "" {
		form.Set("message_thread_id", ns.TGThreadID)
	}
	resp, err := n.cli.PostForm(apiURL, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	return nil
}

// NotifyLogin 发送登录通知。
func (n *Notifier) NotifyLogin(username, ip, ua string) {
	ns := n.store.GetNotifySettings()
	if !ns.Enabled || !ns.LoginEnabled {
		return
	}
	n.Send(ns, model.NotifyMessage{
		Event:  "login",
		Client: username,
		Detail: fmt.Sprintf("IP: %s\nUA: %s", ip, ua),
		Emoji:  "🔔",
	})
}

// SendTest 发送测试消息，返回错误信息给前端。
func (n *Notifier) SendTest(ns model.NotifySettings) error {
	if ns.Channel == "telegram" && (ns.TGBotToken == "" || ns.TGChatID == "") {
		return fmt.Errorf("请先填写 Bot Token 与 Chat ID")
	}
	if ns.Channel == "none" {
		return fmt.Errorf("请选择通知渠道")
	}
	msg := model.NotifyMessage{Event: "test", Client: "Meerkat", Detail: "这是一条测试消息", Emoji: "✅"}
	text := renderTemplate(ns.MessageTPL, msg)
	switch ns.Channel {
	case "telegram":
		return n.sendTelegram(ns, text)
	}
	return fmt.Errorf("未知通知渠道: %s", ns.Channel)
}

// dbSizeBytes 返回数据库文件大小（仪表盘统计用）。
func dbSizeBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// marshalForLog 序列化为 JSON（调试用）。
func marshalForLog(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var _ = context.Background // 预留 context 使用
