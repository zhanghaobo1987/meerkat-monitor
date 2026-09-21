/* meerkat 管理后台逻辑（原生 JS，零依赖，侧边栏 SPA） */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.add("show");
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.remove("show"), 2200);
}
async function api(url, opts = {}) {
  const r = await fetch(url, { headers: { "Content-Type": "application/json" }, ...opts });
  if (r.status === 401) { showLogin(); throw new Error("未登录"); }
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch (e) {}
    throw new Error(msg);
  }
  return r.json();
}
function copyText(text) {
  if (navigator.clipboard) return navigator.clipboard.writeText(text).then(() => toast("已复制"));
  const ta = document.createElement("textarea");
  ta.value = text; document.body.appendChild(ta); ta.select();
  document.execCommand("copy"); ta.remove(); toast("已复制");
}
const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';

/* ---------- 格式化 ---------- */
function fmtBytes(n, digits) {
  if (n == null || isNaN(n) || n < 0) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(i === 0 ? 0 : (digits ?? 1)) + " " + units[i];
}
function fmtBytesSpeed(n) { return fmtBytes(n, 1) + "/s"; }
function fmtTime(ts) {
  if (!ts) return "-";
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, "0");
  return `${d.getFullYear()}/${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function fmtDate(ts) {
  if (!ts) return "-";
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, "0");
  return `${d.getFullYear()}/${p(d.getMonth() + 1)}/${p(d.getDate())}`;
}
function cycleLabel(c) {
  return { monthly: "月", quarterly: "季", semiannual: "半年", yearly: "年", none: "" }[c] || "";
}
function daysLeft(expiredAt) {
  if (!expiredAt) return null;
  return Math.floor((expiredAt * 1000 - Date.now()) / 86400000);
}

/* ---------- 视图切换 ---------- */
let servers = [];
let editingUuid = null;
let editingRuleId = null;
let createdToken = null;

function showLogin() {
  $("#loginView").style.display = "";
  $("#adminLayout").style.display = "none";
}
function showAdmin() {
  $("#loginView").style.display = "none";
  $("#adminLayout").style.display = "";
  loadAll();
}
function navTo(page) {
  $$(".page").forEach((p) => p.classList.remove("show"));
  const el = $("#page-" + page);
  if (el) el.classList.add("show");
  $$(".nav-item").forEach((n) => n.classList.remove("active"));
  const nav = document.querySelector(`.nav-item[data-page="${page}"]`);
  if (nav) {
    nav.classList.add("active");
    const sub = nav.closest(".nav-sub");
    if (sub) sub.classList.add("show");
  }
  location.hash = page;
  // 按页懒加载
  if (page === "dashboard") loadDashboard();
  if (page === "servers") loadServers();
  if (page === "themes") loadThemes();
  if (page === "notify-channel") loadNotify();
  if (page === "notify-offline") loadOffline();
  if (page === "notify-load") loadRules();
  if (page === "notify-general") loadNotifyGeneral();
  if (page === "settings") loadSettings();
}

$$(".nav-item").forEach((n) => {
  if (n.dataset.page) n.addEventListener("click", () => navTo(n.dataset.page));
  if (n.dataset.sub) n.addEventListener("click", () => {
    n.classList.toggle("open");
    $("#" + n.dataset.sub).classList.toggle("show");
  });
});

function loadAll() {
  loadServers();
  loadSettings();
  loadNotify();
}

/* ---------- 登录 ---------- */
$("#loginForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/admin/login", {
      method: "POST",
      body: JSON.stringify({ username: $("#loginUser").value, password: $("#loginPass").value }),
    });
    showAdmin();
    navTo(location.hash.slice(1) || "dashboard");
  } catch (err) { toast(err.message); }
});
$("#btnLogout").addEventListener("click", async () => {
  try { await api("/api/admin/logout", { method: "POST" }); } catch (err) {}
  showLogin();
});

/* ---------- 服务器列表 ---------- */
async function loadServers() {
  try {
    servers = await api("/api/admin/servers");
    renderSrvRows();
    renderOfflineRows();
    fillRuleServerSelect();
  } catch (err) { if (err.message !== "未登录") toast(err.message); }
}

function renderSrvRows() {
  const tb = $("#srvRows");
  const kw = ($("#srvSearch").value || "").toLowerCase();
  const list = servers.filter((s) => !kw || s.name.toLowerCase().includes(kw) || (s.note || "").toLowerCase().includes(kw) || (s.group || "").toLowerCase().includes(kw));
  $("#srvCount").textContent = servers.length;
  if (!list.length) {
    tb.innerHTML = `<tr><td colspan="8" style="color:#94a3b8;text-align:center;padding:24px">${servers.length ? "无匹配结果" : "还没有服务器，点击右上角「添加节点」开始"}</td></tr>`;
    return;
  }
  tb.innerHTML = "";
  for (const s of list) {
    const tr = document.createElement("tr");
    const rep = s.report || {};
    const ip4 = rep.ipv4 || s.ipv4 || "";
    const ip6 = rep.ipv6 || s.ipv6 || "";
    const ipCell = [ip4, ip6].filter(Boolean).map((ip) =>
      `<div style="display:flex;align-items:center;gap:4px"><span class="mono" style="color:var(--text-2)">${esc(ip)}</span><button class="copy-btn" data-copy="${esc(ip)}">${copySvg}</button></div>`
    ).join("") || '<span style="color:#cbd5e1">-</span>';
    // 账单标签
    let bills = "";
    if (s.billing.price > 0 || s.billing.billing_cycle !== "none") {
      const priceStr = s.billing.price > 0 ? `${s.billing.currency}${s.billing.price}` : "免费";
      bills += `<span class="bill-chip bill-price">${esc(priceStr)}/${s.billing.billing_cycle === "none" ? "月" : cycleLabel(s.billing.billing_cycle)}</span>`;
    }
    if (s.billing.expired_at > 0) {
      const d = daysLeft(s.billing.expired_at);
      if (d === null) {} else if (d < 0) bills += `<span class="bill-chip bill-expired">已到期</span>`;
      else if (d <= 30) bills += `<span class="bill-chip" style="background:#fef3c7;color:#b45309">余${d}天</span>`;
      else bills += `<span class="bill-chip bill-days">余${d}天</span>`;
    } else if (s.billing.billing_cycle !== "none" || s.billing.price > 0) {
      bills += `<span class="bill-chip bill-days">长期</span>`;
    }
    if (s.region) bills += `<span class="bill-chip bill-region">${esc(s.region)}</span>`;
    tr.innerHTML = `
      <td><span class="dot ${s.online ? "on" : "off"}" style="display:inline-block"></span></td>
      <td><b>${esc(s.name)}</b>${s.hidden ? ' <span style="font-size:10px;color:#94a3b8">[隐藏]</span>' : ""}</td>
      <td>${ipCell}</td>
      <td><span class="mono" style="color:var(--text-2)">${esc(s.version || "-")}</span></td>
      <td>${esc(s.group || "-")}</td>
      <td style="color:var(--text-3);max-width:140px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(s.note || "-")}</td>
      <td>${bills || "-"}</td>
      <td>
        <button class="btn sm" data-act="edit" data-uuid="${s.uuid}">编辑</button>
        <button class="btn sm" data-act="reset" data-uuid="${s.uuid}">重置令牌</button>
        <button class="btn sm danger" data-act="del" data-uuid="${s.uuid}">删除</button>
      </td>`;
    tb.appendChild(tr);
  }
}
$("#srvSearch").addEventListener("input", renderSrvRows);

$("#srvRows").addEventListener("click", async (e) => {
  const cp = e.target.closest("[data-copy]");
  if (cp) { copyText(cp.dataset.copy); return; }
  const btn = e.target.closest("[data-act]");
  if (!btn) return;
  const uuid = btn.dataset.uuid;
  const s = servers.find((x) => x.uuid === uuid);
  if (!s) return;
  if (btn.dataset.act === "edit") openSrvModal(s);
  if (btn.dataset.act === "reset") {
    if (!confirm(`确定重置「${s.name}」的接入令牌？旧令牌将立即失效。`)) return;
    try {
      const r = await api(`/api/admin/servers/${uuid}/reset-token`, { method: "POST" });
      createdToken = r.token;
      $("#srvModalTitle").textContent = "令牌已重置";
      showCreatedPanel();
      loadServers();
    } catch (err) { toast(err.message); }
  }
  if (btn.dataset.act === "del") {
    if (!confirm(`确定删除服务器「${s.name}」？其全部历史数据将被清除，不可恢复。`)) return;
    try {
      await api(`/api/admin/servers/${uuid}`, { method: "DELETE" });
      toast("已删除");
      loadServers();
    } catch (err) { toast(err.message); }
  }
});

/* ---------- 服务器编辑弹层 ---------- */
function openSrvModal(s) {
  editingUuid = s ? s.uuid : null;
  createdToken = null;
  $("#srvModalTitle").textContent = s ? "编辑服务器" : "添加服务器";
  $("#srvForm").style.display = "";
  $("#srvCreated").style.display = "none";
  $("#fName").value = s ? s.name : "";
  $("#fGroup").value = s ? s.group || "" : "";
  $("#fRegion").value = s ? s.region || "" : "";
  $("#fTag").value = s ? s.tag || "" : "";
  $("#fNote").value = s ? s.note || "" : "";
  $("#fPrice").value = s ? s.billing.price || "" : "";
  $("#fCurrency").value = s ? (s.billing.currency || "$") : "$";
  $("#fCycle").value = s ? (s.billing.billing_cycle || "none") : "none";
  $("#fExpired").value = s && s.billing.expired_at ? new Date(s.billing.expired_at * 1000).toISOString().slice(0, 10) : "";
  $("#fTraffic").value = s && s.billing.traffic_limit ? (s.billing.traffic_limit / 1024 / 1024 / 1024) : "";
  $("#fTrafficType").value = s ? (s.billing.traffic_limit_type || "sum") : "sum";
  $("#fOfflineNotify").checked = s ? s.offline_notify_enabled : true;
  $("#fGrace").value = s ? s.offline_grace_seconds || 60 : 60;
  $("#fOrder").value = s ? s.sort_order || 0 : 0;
  $("#fHidden").checked = s ? !!s.hidden : false;
  $("#srvMask").classList.add("show");
  setTimeout(() => $("#fName").focus(), 50);
}
function showCreatedPanel() {
  $("#srvForm").style.display = "none";
  $("#srvCreated").style.display = "";
  $("#newToken").textContent = createdToken || "";
  const ep = location.origin;
  const cmd = `curl -fsSL ${ep}/install.sh | bash -s -- -t ${createdToken || "<令牌>"}`;
  $("#agentRunCmd").textContent = cmd;
  $("#agentInstallCmd").textContent =
    `# Ubuntu / Debian / CentOS（自动检测面板地址与平台，注册 systemd 服务）\n${cmd}\n\n` +
    `# 脚本优先从本面板下载 Agent 二进制（放置方法见 README「Agent 二进制直传」），\n` +
    `# 未放置时自动从 GitHub Release 下载（仓库已公开，无需 Token）`;
}
$("#btnAdd").addEventListener("click", () => openSrvModal(null));
$("#srvClose").addEventListener("click", () => $("#srvMask").classList.remove("show"));
$("#srvMask").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#srvMask").classList.remove("show"); });
$("#btnSrvDone").addEventListener("click", () => { $("#srvMask").classList.remove("show"); loadServers(); });

$("#btnSrvSave").addEventListener("click", async () => {
  const expiredStr = $("#fExpired").value;
  const expiredAt = expiredStr ? Math.floor(new Date(expiredStr + "T23:59:59").getTime() / 1000) : 0;
  const trafficGB = parseFloat($("#fTraffic").value) || 0;
  const billing = {
    price: parseFloat($("#fPrice").value) || 0,
    currency: $("#fCurrency").value,
    billing_cycle: $("#fCycle").value,
    expired_at: expiredAt,
    traffic_limit: Math.round(trafficGB * 1024 * 1024 * 1024),
    traffic_limit_type: $("#fTrafficType").value,
  };
  const base = {
    name: $("#fName").value,
    group: $("#fGroup").value,
    region: $("#fRegion").value,
    tag: $("#fTag").value,
    note: $("#fNote").value,
    sort_order: Number($("#fOrder").value) || 0,
    hidden: $("#fHidden").checked,
    billing,
    offline_notify_enabled: $("#fOfflineNotify").checked,
    offline_grace_seconds: Number($("#fGrace").value) || 60,
  };
  try {
    if (editingUuid) {
      await api(`/api/admin/servers/${editingUuid}`, { method: "PUT", body: JSON.stringify(base) });
      toast("已保存");
      $("#srvMask").classList.remove("show");
      loadServers();
    } else {
      const r = await api("/api/admin/servers", {
        method: "POST",
        body: JSON.stringify({ name: base.name, note: base.note, tag: base.tag, group: base.group, region: base.region }),
      });
      createdToken = r.token;
      // 创建成功后立即保存编辑字段（账单等）
      await api(`/api/admin/servers/${r.uuid}`, { method: "PUT", body: JSON.stringify(base) }).catch(() => {});
      $("#srvModalTitle").textContent = "服务器已创建";
      showCreatedPanel();
      loadServers();
    }
  } catch (err) { toast(err.message); }
});

/* ---------- 仪表盘 ---------- */
async function loadDashboard() {
  try {
    const d = await api("/api/admin/dashboard");
    $("#dashOnline").innerHTML = `${d.online} <small>/ ${d.total} 台</small>`;
    const warn = $("#dashOfflineWarn");
    if (d.offline > 0) {
      warn.style.display = "";
      warn.textContent = `⚠ ${d.offline} 台服务器离线，请注意检查`;
    } else warn.style.display = "none";
    $("#dashDB").textContent = fmtBytes(d.db_bytes);
    if (!d.expiring.length) $("#dashExpiring").textContent = "无服务器即将到期";
    else $("#dashExpiring").innerHTML = d.expiring.map((x) =>
      `<div style="font-size:13px;padding:2px 0">${esc(x.name)} <span class="bill-chill" style="color:${x.days <= 7 ? "#b91c1c" : "#b45309"}">余 ${x.days} 天</span></div>`
    ).join("");
    renderTrafficChart(d.traffic_series || []);
    renderRank("#rankTraffic", d.traffic_rank, (v) => fmtBytes(v), (i) => `峰值 ${fmtBytesSpeed(i.peak)}`);
    renderRank("#rankCPU", d.cpu_rank, (v) => v.toFixed(1) + "%", (i) => `峰值 ${i.peak.toFixed(1)}%`);
    renderRank("#rankMem", d.mem_rank, (v) => v.toFixed(1) + "%", (i) => `峰值 ${i.peak.toFixed(1)}%`);
  } catch (err) { if (err.message !== "未登录") toast(err.message); }
}

function renderRank(sel, items, fmtVal, fmtPeak) {
  const el = $(sel);
  if (!el) return;
  if (!items || !items.length) { el.innerHTML = '<p style="color:#94a3b8;font-size:13px">暂无数据</p>'; return; }
  const max = Math.max(...items.map((i) => i.value), 0.0001);
  el.innerHTML = items.map((i) => `
    <div class="rank-row">
      <span class="name" title="${esc(i.name)}">${esc(i.name)}</span>
      <span class="bar"><i style="width:${(i.value / max * 100).toFixed(1)}%"></i></span>
      <span class="val"><b style="color:var(--text)">${fmtVal(i.value)}</b><br>${fmtPeak(i)}</span>
    </div>`).join("");
}

function renderTrafficChart(series) {
  const canvas = $("#dashTrafficChart");
  if (!canvas) return;
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const W = rect.width || 800, H = rect.height || 220;
  if (canvas.width !== W * dpr) { canvas.width = W * dpr; canvas.height = H * dpr; }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  if (!series.length) {
    ctx.fillStyle = "#94a3b8"; ctx.font = "13px sans-serif";
    ctx.fillText("暂无流量数据（等待 Agent 上报）", 20, H / 2);
    return;
  }
  const padL = 50, padR = 10, padT = 10, padB = 22;
  const iw = W - padL - padR, ih = H - padT - padB;
  let max = 0;
  for (const p of series) { if (p.net_in > max) max = p.net_in; if (p.net_out > max) max = p.net_out; }
  if (max <= 0) max = 1024;
  const niceMax = niceCeil(max);
  const xs = (i) => padL + (series.length <= 1 ? iw / 2 : (i / (series.length - 1)) * iw);
  const ys = (v) => padT + ih - (v / niceMax) * ih;
  // 网格
  ctx.font = "10px sans-serif"; ctx.strokeStyle = "#eef2f7"; ctx.fillStyle = "#94a3b8";
  for (let i = 0; i <= 4; i++) {
    const v = niceMax * i / 4, y = ys(v);
    ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(W - padR, y); ctx.stroke();
    ctx.fillText(fmtBytes(v, 0) + "/s", 4, y + 3);
  }
  // 时间刻度
  for (const frac of [0, 0.5, 1]) {
    const idx = Math.min(series.length - 1, Math.round(frac * (series.length - 1)));
    const d = new Date(series[idx].time * 1000);
    const p = (x) => String(x).padStart(2, "0");
    ctx.fillText(`${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:00`, Math.min(xs(idx), W - padR - 52), H - 6);
  }
  // 折线（下载蓝 / 上传绿）
  drawSeries(ctx, series, (p) => p.net_in, xs, ys, "#2563eb");
  drawSeries(ctx, series, (p) => p.net_out, xs, ys, "#10b981");
  // 图例
  ctx.fillStyle = "#2563eb"; ctx.fillRect(padL + 4, 0, 10, 3);
  ctx.fillStyle = "#64748b"; ctx.fillText("下载", padL + 18, 5);
  ctx.fillStyle = "#10b981"; ctx.fillRect(padL + 54, 0, 10, 3);
  ctx.fillStyle = "#64748b"; ctx.fillText("上传", padL + 68, 5);
}
function drawSeries(ctx, series, get, xs, ys, color) {
  ctx.strokeStyle = color; ctx.lineWidth = 1.6; ctx.beginPath();
  let started = false;
  for (let i = 0; i < series.length; i++) {
    const x = xs(i), y = ys(get(series[i]));
    if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y);
  }
  ctx.stroke();
}
function niceCeil(v) {
  if (v <= 1) return 1;
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (v <= m * mag) return m * mag;
  return 10 * mag;
}

/* ---------- 主题管理 ---------- */
async function loadThemes() {
  try {
    const themes = await api("/api/admin/theme/list");
    const grid = $("#themeGrid");
    grid.innerHTML = "";
    for (const t of themes) {
      const el = document.createElement("div");
      el.className = "theme-card";
      const preview = t.preview
        ? `<img src="/theme-assets/${encodeURIComponent(t.short)}/${esc(t.preview)}" onerror="this.style.display='none';this.parentNode.innerHTML='<div style=\\'font-size:12px;color:#94a3b8;padding:8px\\'>预览不可用</div>'">`
        : `<div style="font-size:12px;color:#94a3b8;padding:8px;text-align:center">${esc(t.description || (t.short ? "自定义主题" : "Meerkat 默认面板"))}</div>`;
      el.innerHTML = `
        <div class="preview">${preview}${t.active ? '<span class="active-badge">使用中</span>' : ""}</div>
        <div class="info">
          <div class="meta">
            <div class="n" title="${esc(t.name)}">${esc(t.name)}</div>
            <div class="a">by ${esc(t.author || "unknown")} · ${esc(t.version || "-")}</div>
          </div>
          ${t.active
            ? (t.short ? `<button class="btn sm" data-theme="${esc(t.short)}" data-act="use">停用</button>` : "")
            : `<button class="btn sm primary" data-theme="${esc(t.short)}" data-act="use">启用</button>`}
          ${t.short ? `<button class="btn sm danger" data-theme="${esc(t.short)}" data-act="del">删除</button>` : ""}
        </div>`;
      grid.appendChild(el);
    }
  } catch (err) { if (err.message !== "未登录") toast(err.message); }
}

$("#themeGrid").addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-act]");
  if (!btn) return;
  const theme = btn.dataset.theme || "";
  if (btn.dataset.act === "use") {
    // 启用与停用统一为 set（空 = 内置）
    try {
      const active = btn.classList.contains("primary");
      await api("/api/admin/theme/set", { method: "POST", body: JSON.stringify({ theme: active ? theme : "" }) });
      toast(active ? "主题已启用，前台已切换" : "已切回内置主题");
      loadThemes();
    } catch (err) { toast(err.message); }
  }
  if (btn.dataset.act === "del") {
    if (!confirm(`确定删除主题「${theme}」？`)) return;
    try {
      await api("/api/admin/theme/delete", { method: "POST", body: JSON.stringify({ theme }) });
      toast("已删除");
      loadThemes();
    } catch (err) { toast(err.message); }
  }
});

$("#themeUpload").addEventListener("change", async (e) => {
  const file = e.target.files[0];
  if (!file) return;
  if (!file.name.endsWith(".zip")) { toast("请上传 ZIP 格式主题包"); e.target.value = ""; return; }
  const fd = new FormData();
  fd.append("file", file);
  toast("上传中…");
  try {
    const r = await fetch("/api/admin/theme/upload", { method: "POST", body: fd });
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch (err) {}
      throw new Error(msg);
    }
    const ti = await r.json();
    toast(`主题「${ti.name}」安装成功`);
    loadThemes();
  } catch (err) { toast("安装失败: " + err.message); }
  e.target.value = "";
});

/* ---------- 通知渠道 ---------- */
async function loadNotify() {
  try {
    const ns = await api("/api/admin/notify");
    $("#nsEnabled").checked = !!ns.enabled;
    $("#nsChannel").value = ns.channel || "telegram";
    $("#nsTpl").value = ns.message_tpl || "";
    $("#nsBotToken").value = ns.tg_bot_token || "";
    $("#nsChatID").value = ns.tg_chat_id || "";
    $("#nsThreadID").value = ns.tg_thread_id || "";
    $("#nsEndpoint").value = ns.tg_endpoint || "https://api.telegram.org/bot";
  } catch (err) {}
}
function collectNotify() {
  return {
    enabled: $("#nsEnabled").checked,
    channel: $("#nsChannel").value,
    message_tpl: $("#nsTpl").value,
    tg_bot_token: $("#nsBotToken").value.trim(),
    tg_chat_id: $("#nsChatID").value.trim(),
    tg_thread_id: $("#nsThreadID").value.trim(),
    tg_endpoint: $("#nsEndpoint").value.trim(),
    expiry_enabled: $("#nsExpiryEnabled") ? $("#nsExpiryEnabled").checked : false,
    expiry_days: $("#nsExpiryDays") ? Number($("#nsExpiryDays").value) || 10 : 10,
    login_enabled: $("#nsLoginEnabled") ? $("#nsLoginEnabled").checked : false,
    traffic_pct: $("#nsTrafficPct") ? Number($("#nsTrafficPct").value) || 0 : 0,
  };
}
async function saveNotify() {
  try {
    await api("/api/admin/notify", { method: "PUT", body: JSON.stringify(collectNotify()) });
    toast("通知设置已保存");
  } catch (err) { toast(err.message); }
}
$("#btnSaveNotify").addEventListener("click", saveNotify);
$("#btnSaveNotify2").addEventListener("click", saveNotify);
$("#btnTestNotify").addEventListener("click", async () => {
  await saveNotify();
  try {
    await api("/api/admin/notify/test", { method: "POST" });
    toast("测试消息已发送，请查收");
  } catch (err) { toast("发送失败: " + err.message); }
});

/* ---------- 通知通用 ---------- */
async function loadNotifyGeneral() {
  try {
    const ns = await api("/api/admin/notify");
    $("#nsExpiryEnabled").checked = !!ns.expiry_enabled;
    $("#nsExpiryDays").value = ns.expiry_days || 10;
    $("#nsLoginEnabled").checked = !!ns.login_enabled;
    $("#nsTrafficPct").value = ns.traffic_pct ?? 80;
  } catch (err) {}
}
$("#btnSaveNotifyGeneral").addEventListener("click", saveNotify);

/* ---------- 离线通知 ---------- */
function renderOfflineRows() {
  const tb = $("#offlineRows");
  if (!tb) return;
  if (!servers.length) {
    tb.innerHTML = '<tr><td colspan="5" style="color:#94a3b8;text-align:center;padding:24px">暂无服务器</td></tr>';
    return;
  }
  tb.innerHTML = "";
  for (const s of servers) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td><b>${esc(s.name)}</b></td>
      <td>${s.offline_notify_enabled
        ? '<span class="bill-chip bill-days">启用</span>'
        : '<span class="bill-chip" style="background:#fee2e2;color:#b91c1c">禁用</span>'}</td>
      <td>${s.offline_grace_seconds || 60} 秒</td>
      <td style="color:var(--text-3)">${s.last_offline_notify ? fmtTime(s.last_offline_notify) : "-"}</td>
      <td><button class="btn sm" data-uuid="${s.uuid}">编辑</button></td>`;
    tb.appendChild(tr);
  }
}
$("#offlineRows").addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-uuid]");
  if (!btn) return;
  const s = servers.find((x) => x.uuid === btn.dataset.uuid);
  if (s) openSrvModal(s);
});

/* ---------- 负载通知规则 ---------- */
async function loadRules() {
  try {
    const rules = await api("/api/admin/loadrules");
    const tb = $("#ruleRows");
    tb.innerHTML = "";
    if (!rules.length) {
      tb.innerHTML = '<tr><td colspan="7" style="color:#94a3b8;text-align:center;padding:24px">暂无规则，点击右上角「添加」创建</td></tr>';
      return;
    }
    for (const r of rules) {
      const sname = r.server_uuid ? (servers.find((x) => x.uuid === r.server_uuid)?.name || r.server_uuid.slice(0, 12) + "…") : "全部服务器";
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td><b>${esc(r.name)}</b></td>
        <td style="color:var(--text-2)">${esc(sname)}</td>
        <td><span class="bill-chip" style="background:#eff6ff;color:#2563eb">${r.metric.toUpperCase()}</span></td>
        <td>${r.threshold}%</td>
        <td>${r.ratio}</td>
        <td>${r.interval_min} 分钟</td>
        <td>
          <button class="btn sm" data-id="${r.id}" data-act="edit">编辑</button>
          <button class="btn sm danger" data-id="${r.id}" data-act="del">删除</button>
        </td>`;
      tb.appendChild(tr);
    }
  } catch (err) { if (err.message !== "未登录") toast(err.message); }
}
function fillRuleServerSelect() {
  const sel = $("#rServer");
  if (!sel) return;
  sel.innerHTML = '<option value="">全部服务器</option>' + servers.map((s) => `<option value="${s.uuid}">${esc(s.name)}</option>`).join("");
}
$("#btnAddRule").addEventListener("click", () => {
  editingRuleId = null;
  $("#ruleModalTitle").textContent = "添加负载通知规则";
  $("#rName").value = "";
  $("#rServer").value = "";
  $("#rMetric").value = "cpu";
  $("#rThreshold").value = 80;
  $("#rRatio").value = 0.8;
  $("#rInterval").value = 2;
  $("#ruleMask").classList.add("show");
});
$("#ruleRows").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  const id = Number(btn.dataset.id);
  if (btn.dataset.act === "del") {
    if (!confirm("确定删除该规则？")) return;
    try { await api(`/api/admin/loadrules/${id}`, { method: "DELETE" }); toast("已删除"); loadRules(); } catch (err) { toast(err.message); }
  } else {
    const rules = await api("/api/admin/loadrules");
    const r = rules.find((x) => x.id === id);
    if (!r) return;
    editingRuleId = id;
    $("#ruleModalTitle").textContent = "编辑负载通知规则";
    $("#rName").value = r.name;
    $("#rServer").value = r.server_uuid;
    $("#rMetric").value = r.metric;
    $("#rThreshold").value = r.threshold;
    $("#rRatio").value = r.ratio;
    $("#rInterval").value = r.interval_min;
    $("#ruleMask").classList.add("show");
  }
});
$("#ruleClose").addEventListener("click", () => $("#ruleMask").classList.remove("show"));
$("#ruleMask").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#ruleMask").classList.remove("show"); });
$("#btnRuleSave").addEventListener("click", async () => {
  const body = JSON.stringify({
    name: $("#rName").value,
    server_uuid: $("#rServer").value,
    metric: $("#rMetric").value,
    threshold: Number($("#rThreshold").value) || 80,
    ratio: Number($("#rRatio").value) || 0.8,
    interval_min: Number($("#rInterval").value) || 2,
  });
  try {
    if (editingRuleId) await api(`/api/admin/loadrules/${editingRuleId}`, { method: "PUT", body });
    else await api("/api/admin/loadrules", { method: "POST", body });
    toast("已保存");
    $("#ruleMask").classList.remove("show");
    loadRules();
  } catch (err) { toast(err.message); }
});

/* ---------- 站点设置 ---------- */
async function loadSettings() {
  try {
    const st = await api("/api/admin/settings");
    $("#setName").value = st.site_name;
    $("#setInterval").value = st.report_interval;
    $("#setKeep").value = st.data_keep_days;
    if (st.site_name) $("#siteName").textContent = st.site_name;
  } catch (err) {}
}
$("#btnSaveSettings").addEventListener("click", async () => {
  try {
    await api("/api/admin/settings", {
      method: "PUT",
      body: JSON.stringify({
        site_name: $("#setName").value,
        report_interval: Number($("#setInterval").value) || 2,
        data_keep_days: Number($("#setKeep").value) || 30,
      }),
    });
    toast("设置已保存，Agent 将在下次上报时应用新间隔");
  } catch (err) { toast(err.message); }
});
$("#btnChangePw").addEventListener("click", async () => {
  try {
    await api("/api/admin/password", {
      method: "PUT",
      body: JSON.stringify({ old: $("#pwOld").value, new: $("#pwNew").value }),
    });
    $("#pwOld").value = ""; $("#pwNew").value = "";
    toast("密码修改成功");
  } catch (err) { toast(err.message); }
});

/* ---------- 启动 ---------- */
(async function init() {
  try {
    await api("/api/admin/servers");
    showAdmin();
    navTo(location.hash.slice(1) || "dashboard");
  } catch (e) {
    showLogin();
  }
  fetch("/api/public/site").then((r) => r.json()).then((s) => {
    if (s.site_name) $("#siteName").textContent = s.site_name;
  }).catch(() => {});
  // 仪表盘定时刷新
  setInterval(() => {
    if ($("#page-dashboard").classList.contains("show")) loadDashboard();
  }, 60 * 1000);
})();
