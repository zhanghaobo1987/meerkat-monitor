/* meerkat 面板逻辑（原生 JS，零依赖） */
"use strict";

/* ---------- 工具 ---------- */
const $ = (s) => document.querySelector(s);

function fmtBytes(n, digits) {
  if (n == null || isNaN(n)) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(i === 0 ? 0 : (digits ?? 1)) + " " + units[i];
}
function fmtBytesSpeed(n) { return fmtBytes(n, 1) + "/s"; }
function fmtUptime(sec) {
  if (!sec || sec < 0) return "-";
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分钟`;
  return `${m} 分钟`;
}
function fmtTime(ts) {
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.remove("show"), 2200);
}
async function api(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch (e) {}
    throw new Error(msg);
  }
  return r.json();
}

/* ---------- 环形图 ---------- */
function drawRing(canvas, pct, color) {
  const dpr = window.devicePixelRatio || 1;
  const size = 64;
  if (canvas.width !== size * dpr) { canvas.width = size * dpr; canvas.height = size * dpr; }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, size, size);
  const lw = 6, r = (size - lw) / 2, cx = size / 2;
  ctx.lineWidth = lw; ctx.lineCap = "round";
  ctx.strokeStyle = "#e2e8f0";
  ctx.beginPath(); ctx.arc(cx, cx, r, 0, Math.PI * 2); ctx.stroke();
  if (pct > 0) {
    ctx.strokeStyle = color;
    ctx.beginPath();
    ctx.arc(cx, cx, r, -Math.PI / 2, -Math.PI / 2 + Math.PI * 2 * Math.min(pct / 100, 1));
    ctx.stroke();
  }
}
function ringColor(p) { return p >= 90 ? "#ef4444" : p >= 75 ? "#f59e0b" : "#2563eb"; }

/* ---------- 折线图 ---------- */
function drawLineChart(canvas, series, opts) {
  opts = opts || {};
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const W = rect.width || 400, H = rect.height || 150;
  if (canvas.width !== W * dpr) { canvas.width = W * dpr; canvas.height = H * dpr; }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);

  const padL = 40, padR = 8, padT = 8, padB = 18;
  const iw = W - padL - padR, ih = H - padT - padB;
  let max = opts.max ?? 0, min = opts.min ?? 0;
  for (const s of series) for (const p of s.data) {
    if (p == null) continue;
    if (max == null || p > max) max = p;
    if (min == null || p < min) min = p;
  }
  if (!isFinite(max)) max = 1;
  if (!isFinite(min) || min == null) min = 0;
  if (opts.max === undefined && max === min) max = min + 1;
  if (opts.pct) { max = Math.max(max, 1); }
  const niceMax = opts.max !== undefined ? opts.max : niceCeil(max);

  const xs = (i, n) => padL + (n <= 1 ? iw / 2 : (i / (n - 1)) * iw);
  const ys = (v) => padT + ih - ((v - min) / (niceMax - min || 1)) * ih;

  // 横向网格与刻度
  ctx.font = "10px -apple-system, sans-serif";
  ctx.fillStyle = "#94a3b8";
  ctx.strokeStyle = "#eef2f7";
  ctx.lineWidth = 1;
  const steps = 4;
  for (let i = 0; i <= steps; i++) {
    const v = min + ((niceMax - min) * i) / steps;
    const y = ys(v);
    ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(W - padR, y); ctx.stroke();
    let label = opts.fmtTick ? opts.fmtTick(v) : fmtNum(v);
    ctx.fillText(label, 4, y + 3);
  }
  // 时间刻度
  if (series.length && series[0].data.length > 1) {
    const d = series[0].data;
    const times = series[0].times || null;
    ctx.fillStyle = "#94a3b8";
    const n = d.length;
    for (const frac of [0, 0.5, 1]) {
      const idx = Math.min(n - 1, Math.round(frac * (n - 1)));
      const x = xs(idx, n);
      const label = times ? fmtTime(times[idx]) : idx + "";
      ctx.fillText(label, Math.min(x, W - padR - 34), H - 5);
    }
  }
  // 折线
  for (const s of series) {
    const n = s.data.length;
    if (!n) continue;
    ctx.strokeStyle = s.color;
    ctx.lineWidth = 1.6;
    ctx.beginPath();
    let started = false;
    for (let i = 0; i < n; i++) {
      const v = s.data[i];
      if (v == null) { started = false; continue; }
      const x = xs(i, n), y = ys(v);
      if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y);
    }
    ctx.stroke();
    // 渐变填充（单系列）
    if (series.length === 1 && s.fill !== false) {
      ctx.globalAlpha = 0.08;
      ctx.fillStyle = s.color;
      ctx.lineTo(xs(n - 1, n), padT + ih); ctx.lineTo(xs(0, n), padT + ih);
      ctx.closePath(); ctx.fill();
      ctx.globalAlpha = 1;
    }
  }
}
function niceCeil(v) {
  if (v <= 1) return 1;
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (v <= m * mag) return m * mag;
  return 10 * mag;
}
function fmtNum(v) {
  if (Math.abs(v) >= 1000) return (v / 1000).toFixed(1) + "k";
  if (Math.abs(v) >= 10) return v.toFixed(0);
  return v.toFixed(1);
}

/* ---------- 全局状态 ---------- */
let servers = [];            // /api/public/servers 结果
let ws = null;
let wsRetry = 0;
let detailUuid = null;       // 当前弹层对应服务器
let detailHistory = [];      // 弹层历史数据
let liveRecent = {};         // uuid -> 实时样本数组（仅弹层内使用）

/* ---------- 首页渲染 ---------- */
async function loadAll() {
  try {
    const site = await api("/api/public/site").catch(() => ({}));
    if (site.site_name) $("#siteName").textContent = site.site_name;
    servers = await api("/api/public/servers");
    renderGrid();
  } catch (e) {
    toast("加载失败: " + e.message);
  }
}

function renderGrid() {
  const grid = $("#grid");
  const online = servers.filter((s) => s.online).length;
  $("#statOnline").textContent = `在线 ${online}`;
  $("#statTotal").textContent = `总计 ${servers.length}`;
  $("#empty").style.display = servers.length ? "none" : "";
  grid.innerHTML = "";
  for (const s of servers) grid.appendChild(cardOf(s));
  renderBillingBar();
}

function cardOf(s) {
  const el = document.createElement("div");
  el.className = "card";
  el.dataset.uuid = s.uuid;
  const r = s.report;
  const osLine = [s.platform, s.arch, s.cpu_cores ? s.cpu_cores + " 核" : ""].filter(Boolean).join(" · ") || (s.online ? "等待上报…" : "离线");
  const cpuPct = r ? r.cpu_usage : 0;
  const memPct = r && r.mem_total ? ((r.mem_used / r.mem_total) * 100) : 0;
  const diskPct = r && r.disk_total ? ((r.disk_used / r.disk_total) * 100) : 0;
  // 账单标签
  const b = s.billing || {};
  let bills = "";
  if (b.price > 0 || (b.billing_cycle && b.billing_cycle !== "none")) {
    const priceStr = b.price > 0 ? `${b.currency || "$"}${b.price}` : "免费";
    const cyc = b.price > 0 ? "/" + ({ monthly: "月", quarterly: "季", semiannual: "半年", yearly: "年" }[b.billing_cycle] || "月") : "";
    bills += `<span class="bill-chip bill-price">${esc(priceStr)}${cyc}</span>`;
  }
  if (b.expired_at > 0) {
    const d = Math.floor((b.expired_at * 1000 - Date.now()) / 86400000);
    if (d < 0) bills += `<span class="bill-chip bill-expired">已到期</span>`;
    else if (d <= 30) bills += `<span class="bill-chip" style="background:#fef3c7;color:#b45309">余${d}天</span>`;
    else bills += `<span class="bill-chip bill-days">余${d}天</span>`;
  } else if (b.billing_cycle && b.billing_cycle !== "none") {
    bills += `<span class="bill-chip bill-days">长期</span>`;
  }
  if (s.region) bills += `<span class="bill-chip bill-region">${esc(s.region)}</span>`;
  // 流量条
  let trafficLine = "";
  if (b.traffic_limit > 0) {
    const pct = Math.min(100, (b.traffic_used / b.traffic_limit) * 100);
    const tColor = pct >= 95 ? "#ef4444" : pct >= 80 ? "#f59e0b" : "#2563eb";
    trafficLine = `
    <div style="margin-top:10px">
      <div style="display:flex;justify-content:space-between;font-size:11.5px;color:var(--text-3);margin-bottom:3px">
        <span>📶 本月流量 ${fmtBytes(b.traffic_used, 1)} / ${fmtBytes(b.traffic_limit, 1)}</span>
        <span style="color:${tColor};font-weight:600">${pct.toFixed(1)}%</span>
      </div>
      <div style="height:6px;background:var(--bg);border-radius:6px;overflow:hidden">
        <div style="height:100%;width:${pct.toFixed(1)}%;background:${tColor};border-radius:6px"></div>
      </div>
    </div>`;
  }
  el.innerHTML = `
    <div class="card-head">
      <span class="dot ${s.online ? "on" : "off"}"></span>
      <span class="name">${esc(s.name)}</span>
      ${s.tag ? `<span class="tag-chip">${esc(s.tag)}</span>` : ""}
      <span class="spacer"></span>
    </div>
    <div class="os-line">${esc(osLine)}</div>
    <div class="meters">
      <div class="meter"><div class="ring-wrap"><canvas></canvas><div class="pct">${cpuPct.toFixed(0)}%</div></div><div class="label">CPU</div></div>
      <div class="meter"><div class="ring-wrap"><canvas></canvas><div class="pct">${memPct.toFixed(0)}%</div></div><div class="label">内存 ${r ? fmtBytes(r.mem_used, 0) : ""}</div></div>
      <div class="meter"><div class="ring-wrap"><canvas></canvas><div class="pct">${diskPct.toFixed(0)}%</div></div><div class="label">磁盘 ${r ? fmtBytes(r.disk_used, 0) : ""}</div></div>
    </div>
    <div class="netline">
      <div class="n"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="down"><path d="M12 4v12m0 0 5-5m-5 5-5-5M5 20h14"/></svg><b>${r ? fmtBytesSpeed(r.net_in) : "-"}</b></div>
      <div class="n"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="color:#10b981"><path d="M12 20V8m0 0 5 5m-5-5-5 5M5 4h14"/></svg><b>${r ? fmtBytesSpeed(r.net_out) : "-"}</b></div>
    </div>
    ${trafficLine}
    <div class="footline">
      <span>⏱ ${s.online ? fmtUptime(r ? r.uptime : 0) : "离线"}</span>
      ${r && r.ipv4 ? `<span>🌐 ${esc(r.ipv4)}</span>` : ""}
      ${r && r.tcp_conns != null ? `<span>⇄ TCP ${r.tcp_conns}</span>` : ""}
    </div>
    ${bills ? `<div style="margin-top:8px">${bills}</div>` : ""}`;
  const canvases = el.querySelectorAll("canvas");
  drawRing(canvases[0], cpuPct, ringColor(cpuPct));
  drawRing(canvases[1], memPct, "#8b5cf6");
  drawRing(canvases[2], diskPct, "#0ea5e9");
  el.addEventListener("click", () => openDetail(s.uuid));
  return el;
}

/* 账单汇总条 */
function renderBillingBar() {
  const bar = $("#billingBar");
  if (!bar) return;
  const withBill = servers.filter((s) => (s.billing && (s.billing.price > 0 || s.billing.expired_at > 0 || s.billing.traffic_limit > 0)));
  if (!withBill.length) { bar.style.display = "none"; return; }
  bar.style.display = "";
  const bOf = (s) => s.billing;
  const totalCost = withBill.reduce((acc, s) => {
    const b = bOf(s);
    if (!b.price) return acc;
    const perMonth = { monthly: b.price, quarterly: b.price / 3, semiannual: b.price / 6, yearly: b.price / 12, none: 0 }[b.billing_cycle] || 0;
    return acc + perMonth;
  }, 0);
  const cur = withBill.find((s) => bOf(s).price > 0)?.billing.currency || "$";
  const totalTraffic = withBill.reduce((a, s) => a + (bOf(s).traffic_limit > 0 ? bOf(s).traffic_limit : 0), 0);
  const usedTraffic = withBill.reduce((a, s) => a + (bOf(s).traffic_used > 0 ? bOf(s).traffic_used : 0), 0);
  const expiring = withBill.filter((s) => {
    const d = bOf(s).expired_at ? Math.floor((bOf(s).expired_at * 1000 - Date.now()) / 86400000) : null;
    return d !== null && d <= 30;
  });
  $("#billingCards").innerHTML = `
    <div class="stat-card"><div class="t">💰 月均费用</div><div class="v">${cur}${totalCost.toFixed(2)}</div></div>
    <div class="stat-card"><div class="t">📶 流量用量</div><div class="v" style="font-size:18px">${fmtBytes(usedTraffic, 1)} <small>/ ${fmtBytes(totalTraffic, 1)}</small></div></div>
    <div class="stat-card"><div class="t">⏰ 30 天内到期</div><div class="v" style="font-size:16px">${expiring.length ? expiring.map((s) => esc(s.name)).join("、") : "无"}</div></div>`;
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

/* ---------- WebSocket 实时 ---------- */
function connectWS() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onopen = () => { wsRetry = 0; };
  ws.onmessage = (ev) => {
    try {
      const m = JSON.parse(ev.data);
      if (m.type !== "update") return;
      onLive(m.uuid, m.data, m.time);
    } catch (e) {}
  };
  ws.onclose = () => {
    const delay = Math.min(1000 * Math.pow(2, wsRetry++), 15000);
    setTimeout(connectWS, delay);
  };
}

function onLive(uuid, report, ts) {
  const s = servers.find((x) => x.uuid === uuid);
  if (!s) return;
  if (!s.online) { s.online = true; }
  s.report = report;
  s.last_seen = ts;
  // 更新对应卡片（不整体重绘，避免闪烁）
  const idx = servers.indexOf(s);
  const grid = $("#grid");
  const old = grid.children[idx];
  if (old) grid.replaceChild(cardOf(s), old);
  // 弹层实时
  if (detailUuid === uuid) pushDetailLive(ts, report);
}

/* ---------- 详情弹层 ---------- */
async function openDetail(uuid) {
  detailUuid = uuid;
  const s = servers.find((x) => x.uuid === uuid);
  if (!s) return;
  $("#dTitle").textContent = s.name;
  $("#dTag").style.display = s.tag ? "" : "none";
  $("#dTag").textContent = s.tag || "";
  $("#dDot").className = "dot " + (s.online ? "on" : "off");
  renderDetailInfo(s);
  $("#detailMask").classList.add("show");
  try {
    detailHistory = await api(`/api/history?uuid=${encodeURIComponent(uuid)}&hours=24`);
  } catch (e) { detailHistory = []; }
  liveRecent[uuid] = [];
  drawDetailCharts();
}
function closeDetail() {
  $("#detailMask").classList.remove("show");
  detailUuid = null;
}

function pushDetailLive(ts, r) {
  if (!detailUuid) return;
  const arr = liveRecent[detailUuid] || (liveRecent[detailUuid] = []);
  arr.push({ time: ts, r });
  if (arr.length > 120) arr.shift();
  drawDetailCharts();
}

function drawDetailCharts() {
  const s = servers.find((x) => x.uuid === detailUuid);
  if (!s) return;
  const hist = detailHistory || [];
  const live = liveRecent[detailUuid] || [];

  // 时间轴 = 历史(分钟) + 实时样本
  const times = hist.map((p) => p.time).concat(live.map((x) => x.time));
  const cpu = hist.map((p) => p.cpu).concat(live.map((x) => x.r.cpu_usage));
  const mem = hist.map((p) => p.mem_pct).concat(live.map((x) => (x.r.mem_total ? (x.r.mem_used / x.r.mem_total) * 100 : 0)));
  const netIn = hist.map((p) => p.net_in).concat(live.map((x) => x.r.net_in));
  const netOut = hist.map((p) => p.net_out).concat(live.map((x) => x.r.net_out));
  const load = hist.map((p) => p.load1).concat(live.map((x) => x.r.load1));
  const tcp = hist.map((p) => p.tcp).concat(live.map((x) => x.r.tcp_conns));

  const t = { times };
  drawLineChart($("#cCpu"), [{ data: cpu, color: "#2563eb" }], { ...t, pct: true, max: 100, fmtTick: (v) => v.toFixed(0) + "%" });
  drawLineChart($("#cMem"), [{ data: mem, color: "#8b5cf6" }], { ...t, pct: true, max: 100, fmtTick: (v) => v.toFixed(0) + "%" });
  drawLineChart($("#cNet"), [
    { data: netIn, color: "#2563eb" },
    { data: netOut, color: "#10b981" },
  ], { ...t, fmtTick: (v) => fmtBytes(v, 0) });
  drawLineChart($("#cLoad"), [
    { data: load, color: "#f59e0b" },
    { data: tcp, color: "#ef4444" },
  ], { ...t });
}

function renderDetailInfo(s) {
  const r = s.report;
  const rows = [
    ["状态", s.online ? "🟢 在线" : "🔴 离线"],
    ["系统", [s.platform, s.os, s.arch].filter(Boolean).join(" · ") || "-"],
    ["内核", s.kernel_ver || "-"],
    ["虚拟化", s.virt || "-"],
    ["CPU", [s.cpu_model, s.cpu_cores ? s.cpu_cores + " 核" : ""].filter(Boolean).join(" · ") || "-"],
    ["内存", r ? `${fmtBytes(r.mem_used, 1)} / ${fmtBytes(r.mem_total, 1)}` : "-"],
    ["Swap", r && r.swap_total ? `${fmtBytes(r.swap_used, 1)} / ${fmtBytes(r.swap_total, 1)}` : "-"],
    ["磁盘", r ? `${fmtBytes(r.disk_used, 1)} / ${fmtBytes(r.disk_total, 1)}` : "-"],
    ["累计流量", r ? `↓ ${fmtBytes(r.net_total_in, 1)} · ↑ ${fmtBytes(r.net_total_out, 1)}` : "-"],
    ["负载", r ? `${r.load1} / ${r.load5} / ${r.load15}` : "-"],
    ["连接数", r ? `TCP ${r.tcp_conns} · UDP ${r.udp_conns} · 进程 ${r.proc_count}` : "-"],
    ["运行时长", r ? fmtUptime(r.uptime) : "-"],
    ["Agent 版本", s.version || "-"],
    ["最后上报", s.last_seen ? fmtTime(s.last_seen) : "-"],
  ];
  $("#dInfo").innerHTML = rows.map(([k, v]) => `<tr><td>${k}</td><td>${esc(v)}</td></tr>`).join("");
}

/* ---------- 启动 ---------- */
$("#dClose").addEventListener("click", closeDetail);
$("#detailMask").addEventListener("click", (e) => { if (e.target === e.currentTarget) closeDetail(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDetail(); });
window.addEventListener("resize", () => {
  if (detailUuid) drawDetailCharts();
});

loadAll();
connectWS();
setInterval(loadAll, 60 * 1000); // 兜底刷新（含在线状态）
