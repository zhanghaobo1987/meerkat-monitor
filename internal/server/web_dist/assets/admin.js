/* meerkat 管理后台逻辑（原生 JS，零依赖） */
"use strict";

const $ = (s) => document.querySelector(s);
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
  const r = await fetch(url, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
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

let servers = [];
let editingUuid = null; // null=新增
let createdToken = null;

/* ---------- 视图切换 ---------- */
function showLogin() {
  $("#loginView").style.display = "";
  $("#adminView").style.display = "none";
  $("#adminMeta").style.display = "none";
}
function showAdmin() {
  $("#loginView").style.display = "none";
  $("#adminView").style.display = "";
  $("#adminMeta").style.display = "";
  loadServers();
  loadSettings();
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
  } catch (err) { toast(err.message); }
});
$("#btnLogout").addEventListener("click", async (e) => {
  e.preventDefault();
  try { await api("/api/admin/logout", { method: "POST" }); } catch (err) {}
  showLogin();
});

/* ---------- 服务器列表 ---------- */
async function loadServers() {
  try {
    servers = await api("/api/admin/servers");
    renderRows();
  } catch (err) { if (err.message !== "未登录") toast(err.message); }
}

function renderRows() {
  const tb = $("#srvRows");
  if (!servers.length) {
    tb.innerHTML = '<tr><td colspan="7" style="color:#94a3b8;text-align:center;padding:24px">还没有服务器，点击右上角「添加服务器」开始</td></tr>';
    return;
  }
  tb.innerHTML = "";
  for (const s of servers) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${s.online ? '<span class="dot on" style="display:inline-block"></span>' : '<span class="dot off" style="display:inline-block"></span>'}</td>
      <td><b>${esc(s.name)}</b></td>
      <td style="color:#64748b">${esc(s.note || "-")}</td>
      <td>${s.tag ? `<span class="tag-chip">${esc(s.tag)}</span>` : "-"}</td>
      <td style="color:#64748b">${esc([s.platform, s.arch].filter(Boolean).join(" · ") || "-")}</td>
      <td><div class="token-cell mono"><span class="t" data-token="${esc(s.token)}">${esc(s.token.slice(0, 6))}…${esc(s.token.slice(-4))}</span>
        <button class="copy-btn" title="复制令牌" data-copy="${esc(s.token)}">${copySvg}</button></div></td>
      <td>
        <button class="btn sm" data-act="edit" data-uuid="${s.uuid}">编辑</button>
        <button class="btn sm" data-act="reset" data-uuid="${s.uuid}">重置令牌</button>
        <button class="btn sm danger" data-act="del" data-uuid="${s.uuid}">删除</button>
      </td>`;
    tb.appendChild(tr);
  }
}

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

/* ---------- 添加/编辑弹层 ---------- */
function openSrvModal(s) {
  editingUuid = s ? s.uuid : null;
  createdToken = null;
  $("#srvModalTitle").textContent = s ? "编辑服务器" : "添加服务器";
  $("#srvForm").style.display = "";
  $("#srvCreated").style.display = "none";
  $("#fEditRow").style.display = s ? "" : "none";
  $("#fName").value = s ? s.name : "";
  $("#fNote").value = s ? s.note || "" : "";
  $("#fTag").value = s ? s.tag || "" : "";
  $("#fOrder").value = s ? s.sort_order || 0 : 0;
  $("#srvMask").classList.add("show");
  setTimeout(() => $("#fName").focus(), 50);
}
function showCreatedPanel() {
  $("#srvForm").style.display = "none";
  $("#srvCreated").style.display = "";
  $("#newToken").textContent = createdToken || "";
  const ep = location.origin;
  $("#agentRunCmd").textContent =
    `curl -fsSL ${ep}/install.sh | bash -s -- -e ${ep} -t ${createdToken || "<令牌>"}`;
  $("#agentInstallCmd").textContent =
    `# Linux / macOS（自动下载对应平台的 meerkat 并注册为系统服务）\ncurl -fsSL ${ep}/install.sh | bash -s -- -e ${ep} -t ${createdToken || "<令牌>"}`;
}
$("#btnAdd").addEventListener("click", () => openSrvModal(null));
$("#srvClose").addEventListener("click", () => $("#srvMask").classList.remove("show"));
$("#srvMask").addEventListener("click", (e) => { if (e.target === e.currentTarget) $("#srvMask").classList.remove("show"); });
$("#btnSrvDone").addEventListener("click", () => { $("#srvMask").classList.remove("show"); loadServers(); });

$("#btnSrvSave").addEventListener("click", async () => {
  const body = JSON.stringify({
    name: $("#fName").value,
    note: $("#fNote").value,
    tag: $("#fTag").value,
    sort_order: Number($("#fOrder").value) || 0,
  });
  try {
    if (editingUuid) {
      await api(`/api/admin/servers/${editingUuid}`, { method: "PUT", body });
      toast("已保存");
      $("#srvMask").classList.remove("show");
      loadServers();
    } else {
      const r = await api("/api/admin/servers", { method: "POST", body });
      createdToken = r.token;
      $("#srvModalTitle").textContent = "服务器已创建";
      showCreatedPanel();
      loadServers();
    }
  } catch (err) { toast(err.message); }
});

/* ---------- 设置 ---------- */
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
  } catch (e) {
    showLogin();
  }
  // 站点名（登录前也显示）
  fetch("/api/public/site").then((r) => r.json()).then((s) => {
    if (s.site_name) $("#siteName").textContent = s.site_name;
  }).catch(() => {});
})();
