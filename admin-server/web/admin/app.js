const $ = id => document.getElementById(id);
let state = { accounts: [], mailboxes: [], orders: [], messages: [], schedulerJobs: [] };
let aliasSnapshot = { accountID: "", aliases: [], activeCount: 0, target: 750 };
let toastTimer;
let lastStatusWasError = false;
let browserAuthPoll;
let browserAuthRequest = null;
let browserAuthHandoff = null;
let forwardAuthPoll;
let forwardAuthRequest = null;
let lastInboxMessages = [];
let selectedAllocationMailboxIDs = new Set();
let selectedOrderIDs = new Set();
let selectedAliasIDs = new Set();
let aliasBatchDeleting = false;
let aliasBatchRunning = false;
let aliasBatchCancelRequested = false;
let aliasTimeSortDirection = "desc";
let mailboxTimeSortDirection = "desc";
const browserAuthChannel = "cymail-browser-auth-v1";
const views = {
  aliases: ["新建邮箱", "创建、停用、恢复或永久删除 iCloud 隐藏邮箱"],
  overview: ["邮箱管理总览", "统一管理邮箱库存、邮件归档、取件码和发货订单"],
  mailboxes: ["全部邮箱", "查看所有 iCloud 隐藏邮箱及其库存和取件状态"],
  forwarding: ["转发邮箱", "管理 Apple 转发目标、默认邮箱和目的邮箱网页收件授权"],
  inbox: ["统一收件箱", "跨账号、跨邮箱搜索邮件和验证码"],
  orders: ["取件码与发货", "生成、查看状态并重新签发客户取件凭证"],
  accounts: ["iCloud 账号", "添加 iCloud 账号并完成网页登录授权"]
};
const statusLabels = { available: "可用", reserved: "已发货", disabled: "已停用", retired: "已退役", active: "有效", pending: "等待授权", error: "授权失败", expired: "已过期" };

function element(tag, className = "", text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function cell(text = "") { return element("td", "", text ?? ""); }
function clippedCell(text = "") {
  const value = text || "-";
  const td = cell(value);
  td.className = "clipped-cell";
  td.title = value;
  return td;
}
function badge(status) { return element("span", `badge ${status || ""}`, statusLabels[status] || status || "-"); }
function formatDate(value) {
  if (!value) return "-";
  const raw = String(value).trim();
  let normalized = value;
  if (/^-?\d+$/.test(raw)) {
    const numeric = Number(raw);
    normalized = Math.abs(numeric) < 1e12 ? numeric * 1000 : numeric;
  }
  const date = new Date(normalized);
  if (Number.isNaN(date.getTime())) return "-";
  const pad = number => String(number).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
function dateTimestamp(value) {
  if (!value) return Number.NaN;
  const raw = String(value).trim();
  let normalized = value;
  if (/^-?\d+$/.test(raw)) {
    const numeric = Number(raw);
    normalized = Math.abs(numeric) < 1e12 ? numeric * 1000 : numeric;
  }
  return new Date(normalized).getTime();
}
function compareCreatedAt(left, right, direction = "desc") {
  const leftTime = dateTimestamp(left);
  const rightTime = dateTimestamp(right);
  const leftMissing = Number.isNaN(leftTime);
  const rightMissing = Number.isNaN(rightTime);
  if (leftMissing || rightMissing) {
    if (leftMissing === rightMissing) return 0;
    return leftMissing ? 1 : -1;
  }
  return direction === "asc" ? leftTime - rightTime : rightTime - leftTime;
}
function updateTimeSortControl(kind, direction) {
  const heading = $(`${kind}-created-heading`);
  const button = $(`${kind}-created-sort`);
  if (!heading || !button) return;
  const descending = direction === "desc";
  heading.setAttribute("aria-sort", descending ? "descending" : "ascending");
  button.title = descending ? "当前最新优先，点击改为最早优先" : "当前最早优先，点击改为最新优先";
  const indicator = button.querySelector("span");
  if (indicator) indicator.textContent = descending ? "↓" : "↑";
}
function dateTimeCell(value) {
  const formatted = formatDate(value);
  const td = cell();
  td.className = "timestamp-cell";
  td.title = formatted;
  if (formatted === "-") {
    td.textContent = "-";
    return td;
  }
  const [date, time] = formatted.split(" ");
  const stamp = element("time", "timestamp");
  stamp.dateTime = String(value || "");
  stamp.append(element("strong", "", date), element("small", "", time));
  td.append(stamp);
  return td;
}
function formatRemaining(value) {
  if (!value) return "-";
  const seconds = Math.max(0, Math.floor((new Date(value).getTime() - Date.now()) / 1000));
  const h = String(Math.floor(seconds / 3600)).padStart(2, "0");
  const m = String(Math.floor((seconds % 3600) / 60)).padStart(2, "0");
  const s = String(seconds % 60).padStart(2, "0");
  return `${h}:${m}:${s}`;
}
function formatDurationSeconds(value) {
  const seconds = Math.max(0, Math.ceil(Number(value) || 0));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remainder = seconds % 60;
  return `${hours ? `${hours} 小时 ` : ""}${minutes ? `${minutes} 分 ` : ""}${remainder} 秒`;
}
function countdown(value) {
  if (!value) return cell("-");
  const td = cell();
  const node = element("span", "countdown", formatRemaining(value));
  node.dataset.expiresAt = value;
  node.title = `到期时间：${formatDate(value)}`;
  if (new Date(value).getTime() <= Date.now()) node.classList.add("expired");
  td.append(node);
  return td;
}
function updateCountdowns() {
  document.querySelectorAll("[data-expires-at]").forEach(node => {
    node.textContent = formatRemaining(node.dataset.expiresAt);
    node.classList.toggle("expired", new Date(node.dataset.expiresAt).getTime() <= Date.now());
  });
}
function compactDate(value) {
  if (!value) return "-";
  const timestamp = dateTimestamp(value);
  if (Number.isNaN(timestamp)) return "-";
  const date = new Date(timestamp), pad = number => String(number).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}
function sanitizeStatusMessage(message) {
  const text = String(message || "请求失败");
  if (/HTTP\s*421|trustTokens|[A-Za-z0-9+/_=-]{120,}/i.test(text)) {
    return "Apple 网页信任状态已刷新，请重试；若仍失败，请重新完成 iCloud 网页授权。";
  }
  return text;
}
function setStatus(message, ok = false) {
  $("status").textContent = sanitizeStatusMessage(message);
  $("status").style.color = ok ? "#0b8c70" : "#c24a56";
  $("status").dataset.kind = ok ? "success" : "error";
  lastStatusWasError = !ok;
}
function setButtonBusy(button, busy) {
  if (!button) return;
  button.classList.toggle("is-loading", busy);
  button.setAttribute("aria-busy", String(busy));
  button.disabled = busy;
}
function updateFlow(rootID, completed = [], current = "") {
  const root = $(rootID);
  if (!root) return;
  root.querySelectorAll(".flow-step").forEach(step => {
    const key = step.dataset.forwardStep || step.dataset.aliasStep;
    step.classList.toggle("is-done", completed.includes(key));
    step.classList.toggle("is-current", key === current);
  });
}
function toast(message) {
  $("toast").textContent = message;
  $("toast").classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => $("toast").classList.remove("show"), 1800);
}
function wait(milliseconds) { return new Promise(resolve => setTimeout(resolve, milliseconds)); }
async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) }
  });
  let payload;
  try { payload = await response.json(); } catch { throw new Error("后端返回了无效响应"); }
  if (response.status === 401) {
    const error = new Error("登录会话已失效，当前页面已保留；请刷新页面并重新登录后再试");
    error.name = "SessionExpiredError";
    throw error;
  }
  if (!response.ok || !payload.success) {
    const error = new Error(sanitizeStatusMessage(payload.message));
    error.status = response.status;
    error.code = payload.code || "";
    error.retryAfterSeconds = Math.max(0, Number(payload.retry_after_seconds || response.headers.get("Retry-After")) || 0);
    throw error;
  }
  return payload.data;
}
async function copyText(text) {
  try {
    let copied = false;
    if (navigator.clipboard && window.isSecureContext) {
      try { await navigator.clipboard.writeText(text); copied = true; } catch {}
    }
    if (!copied) {
      const area = element("textarea");
      area.value = text;
      area.setAttribute("readonly", "");
      area.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0";
      document.body.append(area);
      area.select();
      copied = document.execCommand("copy");
      area.remove();
    }
    if (!copied) throw new Error("copy unavailable");
    toast("已复制到剪贴板");
    return true;
  } catch {
    setStatus("浏览器阻止了自动复制，新码已显示在页面上，请点击下方复制按钮或手动复制");
    return false;
  }
}
function switchView(view) {
  document.querySelectorAll(".view").forEach(section => section.classList.toggle("hidden", section.id !== `view-${view}`));
  document.querySelectorAll(".nav").forEach(button => button.classList.toggle("active", button.dataset.view === view));
  const [title, subtitle] = views[view] || views.overview;
  $("page-title").textContent = title;
  $("page-subtitle").textContent = subtitle;
  document.body.dataset.view = view;
  if (lastStatusWasError && $("connection").classList.contains("online")) setStatus("后端已连接", true);
  if (view === "aliases" && $("alias-account")?.value) {
    loadAliasManagement().catch(error => setStatus(error.message));
  }
}
function fillSelect(select, items, placeholder) {
  const value = select.value;
  select.replaceChildren();
  const first = element("option", "", placeholder); first.value = ""; select.append(first);
  for (const item of items) { const option = element("option", "", item.label); option.value = item.value; select.append(option); }
  if ([...select.options].some(option => option.value === value)) select.value = value;
}
function messageCountByMailbox() {
  const counts = new Map();
  for (const message of state.messages) counts.set(message.mailbox_id, (counts.get(message.mailbox_id) || 0) + 1);
  return counts;
}
function activeOrderByMailbox() {
  const map = new Map();
  for (const order of state.orders) if (order.status === "active") map.set(order.mailbox_id, order);
  return map;
}

function renderAccounts() {
  const rows = $("account-rows"); rows.replaceChildren();
  for (const account of state.accounts) {
    const tr = element("tr");
    tr.append(cell(account.name), cell(account.id), cell(account.icloud_email || account.real_email || "-"), cell(`${account.alias_active || 0}/${account.alias_total || 0}`));
    const status = cell(); status.append(element("span", `badge ${account.status || ""}`, account.status === "active" ? "已授权" : statusLabels[account.status] || account.status || "-"));
    const appleAccount = account.apple_account_status || {};
    const newInterface = cell();
    if (appleAccount.enabled) {
      const wrap = element("div", "apple-account-cell");
      const okBadge = appleAccount.last_check_ok ? "active" : "error";
      wrap.append(element("span", `badge ${okBadge}`, appleAccount.last_check_ok ? "已启用" : "需刷新"));
      if (appleAccount.last_status) wrap.append(element("small", "muted", appleAccount.last_status));
      if (appleAccount.manage_expires_at) wrap.append(element("small", "muted", `到期 ${formatDate(appleAccount.manage_expires_at)}`));
      const clearButton = element("button", "subtle", "清除"); clearButton.dataset.appleAccountClear = account.id;
      wrap.append(clearButton);
      newInterface.append(wrap);
    } else {
      newInterface.append(element("span", "badge", "未启用"));
    }
    const actions = cell(); actions.className = "row-actions";
    const authorize = element("button", "subtle", account.status === "active" ? "重新授权" : "网页登录授权"); authorize.dataset.authorizeAccount = account.id;
    const paste = element("button", "subtle", "粘贴新接口Cookie"); paste.dataset.appleAccountPaste = account.id; paste.title = "粘贴 account.apple.com 的会话 Cookie 以启用新接口（约 20 个/小时）";
    const login = element("button", "subtle", "新接口密码登录"); login.dataset.appleAccountLogin = account.id; login.title = "用 Apple ID 密码登录新接口（免浏览器采集）";
    const remove = element("button", "danger", "删除"); remove.dataset.deleteAccount = account.id;
    actions.append(authorize, paste, login, remove);
    tr.append(status, newInterface, cell(formatDate(account.last_validated)), actions); rows.append(tr);
  }
  if (!rows.children.length) { const tr = element("tr"); const td = cell("还没有添加 iCloud 账号"); td.colSpan = 8; td.className = "empty"; tr.append(td); rows.append(tr); }
  const options = state.accounts.map(account => ({ value: account.id, label: `${account.name || account.id} · ${account.id}` }));
  fillSelect($("sync-account"), options, "选择账号");
  fillSelect($("alias-account"), options, "选择账号");
  fillSelect($("sched-account"), options, "选择账号");
  fillSelect($("message-account"), options, "全部账号");
	if (!$('sync-account').value && state.accounts.length) $('sync-account').value = state.accounts[0].id;
	renderForwarding(state.accounts.find(account => account.id === $('sync-account').value));
	renderForwardingTable();
	renderMessageForwardOptions();
}

function randomAliasLabel(existingAliases) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
  const used = new Set((existingAliases || []).map(alias => String(alias.label || "").trim().toLowerCase()).filter(Boolean));
  for (let attempt = 0; attempt < 32; attempt++) {
    const bytes = crypto.getRandomValues(new Uint8Array(10));
    const label = [...bytes].map(value => alphabet[value % alphabet.length]).join("");
    if (!/[a-z]/.test(label) || !/[0-9]/.test(label) || used.has(label)) continue;
    return label;
  }
  throw new Error("无法生成唯一随机标签，请重试");
}

function filteredAliases() {
  const query = $("alias-query").value.trim().toLowerCase();
  return aliasSnapshot.aliases
    .filter(alias => !query || `${alias.email || ""} ${alias.label || ""}`.toLowerCase().includes(query))
    .sort((left, right) => compareCreatedAt(left.createdAt, right.createdAt, aliasTimeSortDirection)
      || String(left.email || "").localeCompare(String(right.email || "")));
}

function renderAliasManagement() {
  const aliases = filteredAliases();
  updateTimeSortControl("alias", aliasTimeSortDirection);
  $("alias-active-count").textContent = String(aliasSnapshot.activeCount);
  $("alias-capacity-target").textContent = String(aliasSnapshot.target);
  const capacityPercent = aliasSnapshot.target > 0 ? Math.min(100, aliasSnapshot.activeCount / aliasSnapshot.target * 100) : 0;
  $("alias-capacity-progress").style.width = `${capacityPercent}%`;
  updateFlow("alias-flow", aliasSnapshot.accountID ? ["account"] : [], aliasSnapshot.accountID ? "generate" : "account");
  const selectableIDs = new Set(aliasSnapshot.aliases.filter(alias => alias.anonymousId).map(alias => alias.anonymousId));
  selectedAliasIDs = new Set([...selectedAliasIDs].filter(id => selectableIDs.has(id)));
  const rows = $("alias-rows");
  rows.replaceChildren();
  for (const alias of aliases) {
    const tr = element("tr");
    const selection = cell(); selection.className = "alias-select-cell";
    if (alias.anonymousId) {
      const checkbox = element("input"); checkbox.type = "checkbox"; checkbox.checked = selectedAliasIDs.has(alias.anonymousId); checkbox.dataset.aliasSelection = alias.anonymousId; checkbox.setAttribute("aria-label", `选择隐藏邮箱 ${alias.email || alias.label || ""}`); selection.append(checkbox);
    } else selection.append(element("span", "muted", "—"));
    const status = cell(); status.append(badge(alias.active ? "active" : "disabled"));
    const actions = cell(); actions.className = "row-actions alias-actions-cell";
    if (alias.anonymousId) {
      const toggle = element("button", "subtle", alias.active ? "停用" : "恢复");
      toggle.dataset.aliasAction = alias.active ? "deactivate" : "reactivate";
      toggle.dataset.aliasId = alias.anonymousId;
      actions.append(toggle);
      const remove = element("button", "danger", "永久删除");
      remove.dataset.aliasAction = "delete";
      remove.dataset.aliasId = alias.anonymousId;
      remove.dataset.aliasEmail = alias.email || "";
      actions.append(remove);
    } else {
      actions.append(element("span", "muted", "Apple 未返回可操作 ID"));
    }
    tr.append(selection, clippedCell(alias.label), clippedCell(alias.email), clippedCell(alias.forwardToEmail), status, dateTimeCell(alias.createdAt), actions);
    rows.append(tr);
  }
  if (!rows.children.length) {
    const tr = element("tr"), td = cell(aliasSnapshot.accountID ? "没有匹配的隐藏邮箱" : "请先选择 iCloud 账号");
    td.colSpan = 7; td.className = "empty"; tr.append(td); rows.append(tr);
  }
  const selectedCount = selectedAliasIDs.size;
  const selectAll = $("alias-select-all");
  const selectableCount = aliases.filter(alias => alias.anonymousId).length;
  selectAll.disabled = selectableCount === 0;
  selectAll.checked = selectableCount > 0 && aliases.every(alias => !alias.anonymousId || selectedAliasIDs.has(alias.anonymousId));
  selectAll.indeterminate = selectedCount > 0 && !selectAll.checked;
  const deleteButton = $("delete-selected-aliases");
  deleteButton.disabled = selectedCount === 0 || aliasBatchDeleting;
  deleteButton.textContent = selectedCount ? `删除 ${selectedCount} 个` : "批量删除";
}

function normalizedAliasBatchCount() {
  const input = $("alias-batch-count");
  const count = Math.max(2, Math.min(50, Math.trunc(Number(input.value) || 25)));
  input.value = String(count);
  return count;
}

function normalizedAliasCreateIntervalSeconds() {
  const input = $("alias-create-interval");
  const numeric = Number(input.value);
  const seconds = Number.isFinite(numeric) ? Math.max(0, Math.min(86400, Math.trunc(numeric))) : 144;
  input.value = String(seconds);
  return seconds;
}

function updateAliasBatchControls() {
  const count = normalizedAliasBatchCount();
  $("create-alias-batch").querySelector("span").textContent = aliasBatchRunning ? "正在批量创建" : `批量生成 ${count} 个`;
  $("alias-batch-progress").setAttribute("aria-valuemax", String(count));
  document.querySelectorAll("[data-alias-batch-count]").forEach(button => button.classList.toggle("active", Number(button.dataset.aliasBatchCount) === count));
}

function updateAliasBatchProgress(completed, total) {
  const progress = $("alias-batch-progress");
  progress.setAttribute("aria-valuenow", String(completed));
  progress.setAttribute("aria-valuemax", String(total));
  progress.querySelector("i").style.width = `${total ? completed / total * 100 : 0}%`;
}

async function waitForAliasBatchRetry(seconds, index, total, completed) {
  const deadline = Date.now() + Math.max(1, seconds) * 1000;
  while (!aliasBatchCancelRequested && Date.now() < deadline) {
    const remaining = Math.max(1, Math.ceil((deadline - Date.now()) / 1000));
    const duration = formatDurationSeconds(remaining);
    $("alias-create-status").textContent = `正在等待自定义创建间隔或 Apple 限速。${duration}后自动继续第 ${index + 1} / ${total} 个；已成功 ${completed} 个。`;
    $("alias-create-status").dataset.state = "warning";
    $("create-alias-batch").querySelector("span").textContent = `等待 ${duration}`;
    await wait(Math.min(1000, Math.max(1, deadline - Date.now())));
  }
  return !aliasBatchCancelRequested;
}

function renderAliasBatchResult(created, failure = "") {
  const root = $("alias-batch-result"), items = $("alias-batch-items");
  root.classList.remove("hidden");
  root.classList.toggle("has-error", Boolean(failure));
  $("alias-batch-summary").textContent = failure ? `已成功 ${created.length} 个，任务提前停止` : `已成功创建 ${created.length} 个隐藏邮箱`;
  items.replaceChildren();
  for (const item of created) {
    const card = element("div", "alias-batch-item");
    const details = `${item.label || "随机标签"} · 创建于 ${formatDate(item.created_at || item.createdAt)}`;
    card.append(element("strong", "", item.email || "-"), element("small", "", details));
    items.append(card);
  }
  if (failure) {
    const error = element("div", "alias-batch-item alias-batch-error");
    error.append(element("strong", "", "停止原因"), element("small", "", failure));
    items.append(error);
  }
}

async function loadAliasManagement() {
  const accountID = $("alias-account").value;
  if (!accountID) {
    aliasSnapshot = { accountID: "", aliases: [], activeCount: 0, target: 750 };
    renderAliasManagement();
    $("alias-create-status").dataset.state = "idle";
    return;
  }
  $("alias-create-status").textContent = "正在从 Apple 刷新隐藏邮箱…";
  $("alias-create-status").dataset.state = "loading";
  const data = await api(`/api/aliases?account_id=${encodeURIComponent(accountID)}`);
  if ($("alias-account").value !== accountID) return;
  const aliases = data.aliases || [];
  aliasSnapshot = {
    accountID,
    aliases,
    activeCount: Number.isFinite(data.active_count) ? data.active_count : aliases.filter(alias => alias.active).length,
    target: Number.isFinite(data.managed_capacity_target) ? data.managed_capacity_target : 750
  };
  renderAliasManagement();
  $("alias-create-status").textContent = `已刷新 ${aliases.length} 个地址，其中 ${aliasSnapshot.activeCount} 个使用中。`;
  $("alias-create-status").dataset.state = "success";
  renderAppleAccountHint(accountID);
}

function renderAppleAccountHint(accountID) {
  const account = state.accounts.find(item => item.id === accountID);
  const status = account?.apple_account_status || {};
  const hint = $("alias-create-status");
  if (status.enabled) {
    const stateText = status.last_check_ok ? "新接口已启用" : "新接口需刷新";
    hint.textContent = `${hint.textContent} ${stateText}：创建优先走 Apple Account 管理接口（约 20 个/小时），限速自动回退旧接口（约 5 个/小时）。`;
  } else {
    hint.textContent = `${hint.textContent} 未启用新接口（仅旧接口约 5 个/小时）：可在「iCloud 账号」页粘贴 account.apple.com Cookie 或重新授权以提升到约 25 个/小时。`;
  }
}

function forwardingMailbox(account, email) {
  if (!account || !email) return null;
  return account.forward_mailboxes?.[email.toLowerCase()] || null;
}

function renderForwarding(account) {
  const select = $("sync-forward");
  const previous = select.value;
  select.replaceChildren();
  const emails = account?.forward_to_emails || [];
  if (!emails.length) {
    const option = element("option", "", account ? "请刷新 Apple 转发设置" : "请先选择账号");
    option.value = ""; select.append(option);
    updateForwardingForm(account, "");
    return;
  }
  for (const email of emails) {
    const cfg = forwardingMailbox(account, email);
    const labels = [email];
    if (email.toLowerCase() === (account.selected_forward_to || "").toLowerCase()) labels.push("Apple 当前默认");
    labels.push(cfg?.authorized ? "已授权" : "未授权");
    const option = element("option", "", `${labels[0]}（${labels.slice(1).join(" · ")}）`);
    option.value = email; select.append(option);
  }
  const preferred = [previous, account.selected_forward_to, emails[0]].find(value => emails.some(email => email.toLowerCase() === (value || "").toLowerCase()));
  select.value = preferred || emails[0];
  updateForwardingForm(account, select.value);
}

function renderForwardingTable() {
  const rows = $("forwarding-rows");
  rows.replaceChildren();
  let destinationCount = 0, authorizedCount = 0;
  for (const account of state.accounts) {
    for (const email of account.forward_to_emails || []) {
      destinationCount++;
      const cfg = forwardingMailbox(account, email);
      if (cfg?.authorized) authorizedCount++;
      const tr = element("tr");
      if (account.id === $("sync-account").value && email.toLowerCase() === $("sync-forward").value.toLowerCase()) tr.classList.add("is-selected");
      const isDefault = email.toLowerCase() === (account.selected_forward_to || "").toLowerCase();
      const provider = cfg?.provider === "netease_163" || email.toLowerCase().endsWith("@163.com") ? "163 网页" : "暂不支持";
      const defaultCell = cell();
      defaultCell.append(element("span", `badge ${isDefault ? "active" : ""}`, isDefault ? "当前默认" : "候选"));
      const authCell = cell();
      authCell.append(element("span", `badge ${cfg?.authorized ? "active" : "pending"}`, cfg?.authorized ? "已授权" : "未授权"));
      const actions = cell(); actions.className = "row-actions";
      const manage = element("button", "subtle", "管理");
      manage.dataset.manageForwardAccount = account.id;
      manage.dataset.manageForwardEmail = email;
      actions.append(manage);
      tr.append(cell(account.name || account.id), cell(email), cell(provider), defaultCell, authCell, cell(formatDate(cfg?.last_validated)), actions);
      rows.append(tr);
    }
  }
  $("forward-destination-count").textContent = String(destinationCount);
  $("forward-authorized-count").textContent = String(authorizedCount);
  if (!rows.children.length) {
    const tr = element("tr");
    const td = cell("还没有转发邮箱，请先添加并授权 iCloud 账号");
    td.colSpan = 7; td.className = "empty"; tr.append(td); rows.append(tr);
  }
}

function updateForwardingForm(account, email) {
  const cfg = forwardingMailbox(account, email);
  const status = $("forwarding-status");
  status.classList.remove("authorized", "unauthorized");
  if (!email) {
    status.textContent = account ? "还没有转发候选项，请点击“刷新转发设置”。" : "选择账号后读取 Apple 转发设置。";
    updateFlow("forwarding-flow", account ? ["account"] : [], account ? "destination" : "account");
  } else if (cfg?.authorized) {
    status.classList.add("authorized");
    status.textContent = `已授权网页收件：${email}${cfg.last_validated ? ` · 最近验证 ${formatDate(cfg.last_validated)}` : ""}`;
    updateFlow("forwarding-flow", ["account", "destination", "authorize"], "");
  } else {
    status.classList.add("unauthorized");
    status.textContent = `未授权网页收件：${email}${cfg?.last_error ? ` · ${cfg.last_error}` : " · 请先登录 163 官方网页并授权"}`;
    updateFlow("forwarding-flow", ["account", "destination"], "authorize");
  }
}

async function startForwardWebAuthorization(accountID, email) {
  const data = await api(`/api/accounts/${encodeURIComponent(accountID)}/forwarding/web-auth/start`, {
    method: "POST", body: JSON.stringify({ email })
  });
  forwardAuthRequest = { accountID, requestID: data.request_id, email };
  const payload = {
    version: 1,
    kind: "forwarding",
    backend: new URL("/", window.location.href).href,
    authorization_code: data.authorization_code,
    expires_at: data.expires_at,
    email: data.email,
    provider: data.provider
  };
  $("forward-web-auth-box").classList.remove("hidden");
  $("forward-web-auth-status").textContent = "正在连接 CYMail 扩展并打开 163 官方邮箱…";
  clearInterval(forwardAuthPoll);
  forwardAuthPoll = setInterval(pollForwardWebAuthorization, 2000);
  const handoff = handoffAuthorizationToExtension(payload);
  window.open(data.target_url, "_blank", "noopener,noreferrer");
  await handoff;
  $("forward-web-auth-status").textContent = `扩展已接收 ${email} 的授权请求。登录并进入 163 收件箱后，在右上角确认授权。`;
}

async function pollForwardWebAuthorization() {
  if (!forwardAuthRequest) return;
  try {
    const data = await api(`/api/accounts/${encodeURIComponent(forwardAuthRequest.accountID)}/forwarding/web-auth/status?request_id=${encodeURIComponent(forwardAuthRequest.requestID)}`);
    if (data.status === "completed") {
      clearInterval(forwardAuthPoll); forwardAuthPoll = null;
      $("forward-web-auth-status").textContent = `${data.email} 网页授权成功，会话已加密保存。`;
      const accountID = forwardAuthRequest.accountID;
      forwardAuthRequest = null;
      await loadForwarding(accountID);
      setStatus("163 网页收件授权成功", true);
    } else if (data.status === "error") {
      clearInterval(forwardAuthPoll); forwardAuthPoll = null;
      $("forward-web-auth-status").textContent = data.error || "163 网页授权失败，请重新开始。";
      forwardAuthRequest = null;
    } else {
      $("forward-web-auth-status").textContent = "等待扩展从已登录的 163 收件箱提交网页会话…";
    }
  } catch (error) {
    clearInterval(forwardAuthPoll); forwardAuthPoll = null;
    forwardAuthRequest = null;
    $("forward-web-auth-status").textContent = error.message;
  }
}

function mergeAccount(updated) {
  const index = state.accounts.findIndex(account => account.id === updated.id);
  if (index >= 0) state.accounts[index] = updated; else state.accounts.push(updated);
  renderAccounts();
}

async function loadForwarding(accountID) {
  if (!accountID) return renderForwarding(null);
  const account = await api(`/api/accounts/${encodeURIComponent(accountID)}/forwarding`);
  mergeAccount(account);
  return account;
}

function renderMessageForwardOptions() {
  const accountID = $("message-account").value;
  const mailboxes = state.mailboxes.filter(mailbox => !accountID || mailbox.account_id === accountID);
  const emails = [...new Set(mailboxes.map(mailbox => mailbox.forward_to_email).filter(Boolean))];
  fillSelect($("message-forward"), emails.map(email => ({ value: email, label: email })), "全部收件邮箱");
  renderMessageMailboxOptions();
}

function renderMessageMailboxOptions() {
  const accountID = $("message-account").value, forwardTo = $("message-forward").value;
  const mailboxes = state.mailboxes.filter(mailbox => (!accountID || mailbox.account_id === accountID) && (!forwardTo || mailbox.forward_to_email === forwardTo));
  fillSelect($("message-mailbox"), mailboxes.map(mailbox => ({ value: mailbox.id, label: mailbox.label ? `${mailbox.address} · ${mailbox.label}` : mailbox.address })), "全部隐藏邮箱");
  const suggestions = $("message-mailbox-suggestions");
  suggestions.replaceChildren();
  for (const mailbox of mailboxes) {
    const option = element("option");
    option.value = mailbox.address;
    option.label = mailbox.label || mailbox.address;
    suggestions.append(option);
  }
}

async function startBrowserAuthorization(accountID) {
  const data = await api(`/api/accounts/${encodeURIComponent(accountID)}/browser-auth/start`, { method: "POST", body: "{}" });
  browserAuthRequest = { accountID, requestID: data.request_id };
  const account = state.accounts.find(item => item.id === accountID);
  const iCloudURL = account?.host === "icloud.com.cn" ? "https://www.icloud.com.cn/" : "https://www.icloud.com/";
  browserAuthHandoff = {
    target: iCloudURL,
    payload: {
      version: 1,
      backend: new URL("/", window.location.href).href,
      authorization_code: data.authorization_code,
      expires_at: data.expires_at
    }
  };
  $("open-browser-auth").disabled = false;
  $("browser-auth-status").textContent = "点击按钮后会打开标准 Apple 登录页；登录完成后在页面右上角确认 CYMail 授权。";
  $("browser-auth-box").classList.remove("hidden");
  clearInterval(browserAuthPoll);
  browserAuthPoll = setInterval(pollBrowserAuthorization, 2000);
  setStatus("账号已创建，请在五分钟内完成网页登录授权", true);
}

async function pollBrowserAuthorization() {
  if (!browserAuthRequest) return;
  try {
    const data = await api(`/api/accounts/${encodeURIComponent(browserAuthRequest.accountID)}/browser-auth/status?request_id=${encodeURIComponent(browserAuthRequest.requestID)}`);
    if (data.status === "completed") {
      clearInterval(browserAuthPoll); browserAuthPoll = null;
      browserAuthHandoff = null;
      $("open-browser-auth").disabled = true;
      await loadAll();
      const account = state.accounts.find(item => item.id === browserAuthRequest?.accountID);
      const appleOK = account?.apple_account_status?.enabled;
      $("browser-auth-status").textContent = appleOK
        ? "授权成功，iCloud 会话与新接口（Apple Account 管理）均已保存，创建配额约 25 个/小时。"
        : "授权成功，iCloud 会话已加密保存。未检测到 account.apple.com 会话：如需提高创建配额（约 25 个/小时），请先在浏览器登录 https://account.apple.com 后重新授权。";
      setStatus("浏览器授权成功", true);
    } else if (data.status === "error") {
      clearInterval(browserAuthPoll); browserAuthPoll = null;
      browserAuthHandoff = null;
      $("open-browser-auth").disabled = true;
      $("browser-auth-status").textContent = data.error || "授权失败，请重新生成授权链接。";
    } else {
      $("browser-auth-status").textContent = "等待浏览器扩展提交 iCloud 会话…";
    }
  } catch (error) {
    clearInterval(browserAuthPoll); browserAuthPoll = null;
    browserAuthHandoff = null;
    $("open-browser-auth").disabled = true;
    $("browser-auth-status").textContent = error.message;
  }
}

function filteredMailboxes() {
  const query = $("mailbox-query").value.trim().toLowerCase();
  const status = $("mailbox-status").value;
  return state.mailboxes
    .filter(mailbox => (!status || mailbox.status === status) && (!query || `${mailbox.address} ${mailbox.label || ""}`.toLowerCase().includes(query)))
    .sort((left, right) => compareCreatedAt(left.created_at, right.created_at, mailboxTimeSortDirection)
      || String(left.address || "").localeCompare(String(right.address || "")));
}
function renderMailboxes() {
  const rows = $("mailbox-rows"); rows.replaceChildren();
  const counts = messageCountByMailbox(), orders = activeOrderByMailbox();
  const visibleMailboxes = filteredMailboxes();
  updateTimeSortControl("mailbox", mailboxTimeSortDirection);
  $("mailbox-result-count").textContent = `${visibleMailboxes.length} / ${state.mailboxes.length} 个邮箱`;
  for (const mailbox of visibleMailboxes) {
    const tr = element("tr");
    const address = cell(); const addressWrap = element("div", "address-cell");
    addressWrap.append(element("strong", "", mailbox.address), element("small", "", mailbox.id)); address.append(addressWrap);
    const status = cell(); status.append(badge(mailbox.status));
    const order = orders.get(mailbox.id); const access = cell(); access.append(order ? badge("active") : element("span", "badge", "未生成"));
    const actions = cell(); const actionWrap = element("div", "row-actions");
    const inboxButton = element("button", "subtle", "查看邮件"); inboxButton.dataset.mailboxInbox = mailbox.id; actionWrap.append(inboxButton);
    if (order) { const orderButton = element("button", "", "取件管理"); orderButton.dataset.orderView = order.id; actionWrap.append(orderButton); }
    actions.append(actionWrap);
    tr.append(address, cell(mailbox.account_id), cell(mailbox.forward_to_email || "未同步"), cell(mailbox.label || "-"), dateTimeCell(mailbox.created_at), cell(counts.get(mailbox.id) || 0), status, access, countdown(order?.expires_at), actions);
    rows.append(tr);
  }
  if (!rows.children.length) { const tr = element("tr"); const td = cell("没有匹配的邮箱"); td.colSpan = 10; td.className = "empty"; tr.append(td); rows.append(tr); }
  renderMessageMailboxOptions();
}

function availableAllocationMailboxes() {
  const query = $("order-mailbox-query").value.trim().toLowerCase();
  return state.mailboxes.filter(mailbox => mailbox.status === "available" && (!query || `${mailbox.address} ${mailbox.label || ""} ${mailbox.forward_to_email || ""}`.toLowerCase().includes(query)));
}

function renderAllocationMailboxes() {
  const availableIDs = new Set(state.mailboxes.filter(mailbox => mailbox.status === "available").map(mailbox => mailbox.id));
  selectedAllocationMailboxIDs = new Set([...selectedAllocationMailboxIDs].filter(id => availableIDs.has(id)));
  const root = $("allocation-mailboxes"); root.replaceChildren();
  const mailboxes = availableAllocationMailboxes();
  const allAvailableCount = availableIDs.size;
  $("allocation-available-count").textContent = String(allAvailableCount);
  for (const mailbox of mailboxes) {
    const label = element("label", "allocation-mailbox");
    label.title = `${mailbox.address}${mailbox.label ? ` · ${mailbox.label}` : ""}`;
    const checkbox = element("input"); checkbox.type = "checkbox"; checkbox.checked = selectedAllocationMailboxIDs.has(mailbox.id); checkbox.dataset.allocationMailbox = mailbox.id;
    const details = element("span");
    details.append(element("strong", "", mailbox.address), element("small", "", [mailbox.label, mailbox.forward_to_email].filter(Boolean).join(" · ") || "无标签"));
    label.append(checkbox, details); root.append(label);
  }
  if (!mailboxes.length) root.append(element("div", "empty allocation-empty", "没有匹配的可用邮箱"));
  const count = selectedAllocationMailboxIDs.size;
  $("allocation-selected-count").textContent = `已选 ${count} · 显示 ${mailboxes.length} / 共 ${allAvailableCount}`;
  $("allocation-mailboxes").classList.toggle("has-selection", count > 0);
  $("allocate").textContent = count > 1 ? `批量签发 ${count} 个取件码` : count === 1 ? "签发所选邮箱" : "自动分配并签发 1 个";
}

function renderOrders() {
  const rows = $("order-rows"); rows.replaceChildren();
  const filter = $("order-status").value;
  const activeIDs = new Set(state.orders.filter(order => order.status === "active").map(order => order.id));
  selectedOrderIDs = new Set([...selectedOrderIDs].filter(id => activeIDs.has(id)));
  const visibleOrders = state.orders.filter(item => !filter || item.status === filter);
  for (const order of visibleOrders) {
	const waitingForPickup = order.status === "active" && !order.activated_at;
    const tr = element("tr");
    const selection = cell(); selection.className = "order-select-cell";
    if (order.status === "active") {
      const checkbox = element("input"); checkbox.type = "checkbox"; checkbox.checked = selectedOrderIDs.has(order.id); checkbox.dataset.orderSelection = order.id; checkbox.setAttribute("aria-label", `选择订单 ${order.mailbox_address}`); selection.append(checkbox);
    } else selection.append(element("span", "muted", "—"));
	const code = cell(); code.append(element("span", "badge", order.status === "active" ? (waitingForPickup ? "已签发，等待首次取件" : "已安全保存（不显示原码）") : "已失效"));
	const status = cell(); status.append(waitingForPickup ? element("span", "badge pending", "待取件激活") : badge(order.status));
    const actions = cell(); const actionWrap = element("div", "row-actions");
    if (order.status === "active") {
      const copy = element("button", "order-copy-button", "复制新码"); copy.dataset.copyOrder = order.id; copy.title = "重新签发并复制：邮箱-----取件码";
      const reissue = element("button", "subtle", "查看新码"); reissue.dataset.reissue = order.id;
      actionWrap.append(copy, reissue);
    }
    const inbox = element("button", "subtle", "查看邮件"); inbox.dataset.mailboxInbox = order.mailbox_id; actionWrap.append(inbox); actions.append(actionWrap);
	const validity = waitingForPickup
	  ? cell(`首次成功输入取件码后开始 ${formatDurationSeconds(order.valid_for_seconds || 86400)}`)
	  : countdown(order.expires_at);
	tr.append(selection, cell(order.external_id || "-"), cell(order.mailbox_address), code, status, dateTimeCell(order.created_at), validity, actions); rows.append(tr);
  }
  if (!rows.children.length) { const tr = element("tr"); const td = cell("暂无取件订单"); td.colSpan = 8; td.className = "empty"; tr.append(td); rows.append(tr); }
  const selectedCount = selectedOrderIDs.size;
  $("selected-order-count").textContent = `已选 ${selectedCount} 个`;
  $("copy-selected-orders").disabled = selectedCount === 0;
  $("copy-selected-orders").textContent = selectedCount ? `批量复制 ${selectedCount} 个新码` : "批量复制新码";
  $("select-active-orders").disabled = !visibleOrders.some(order => order.status === "active");
}

function messageCard(message) {
  const card = element("article", "message");
  const top = element("div", "message-top"); top.append(element("span", "badge", message.recipient || "未知邮箱"));
  if (message.otp_code) { const otp = element("button", "otp", `验证码 ${message.otp_code}`); otp.addEventListener("click", () => copyText(message.otp_code)); top.append(otp); }
  card.append(top, element("h3", "", message.subject || "无主题"), element("div", "meta", `${message.sender || "未知发件人"} · ${compactDate(message.received_at)}`), element("div", "body", message.body_text || ""));
  return card;
}
function renderMessages(target, messages) {
  const root = $(target); root.replaceChildren();
  if (!messages.length) { root.append(element("div", "empty", "暂时没有邮件")); return; }
  for (const message of messages) root.append(messageCard(message));
}

function filterInboxMessages(messages) {
  const accountID = $("message-account").value;
  const forwardTo = $("message-forward").value;
  const mailboxID = $("message-mailbox").value;
  const mailboxQuery = $("message-mailbox-query").value.trim().toLowerCase();
  const mailboxMap = new Map(state.mailboxes.map(mailbox => [mailbox.id, mailbox]));
  return messages.filter(message => {
    const mailbox = mailboxMap.get(message.mailbox_id);
    if (accountID && message.account_id !== accountID) return false;
    if (forwardTo && mailbox?.forward_to_email !== forwardTo) return false;
    if (mailboxID && message.mailbox_id !== mailboxID) return false;
    if (mailboxQuery && !`${mailbox?.address || message.recipient || ""} ${mailbox?.label || ""}`.toLowerCase().includes(mailboxQuery)) return false;
    return true;
  });
}

function renderInboxMessages(messages) {
  const root = $("all-messages"); root.replaceChildren();
  if (!messages.length) {
    root.append(element("div", "empty", "暂时没有匹配的邮件"));
    $("inbox-summary").textContent = "0 封邮件";
    return;
  }
  const mailboxMap = new Map(state.mailboxes.map(mailbox => [mailbox.id, mailbox]));
  const groups = new Map();
  for (const message of messages) {
    const key = message.mailbox_id || message.recipient || "unknown";
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(message);
  }
  const sorted = [...groups.entries()].sort(([left], [right]) => {
    const leftName = mailboxMap.get(left)?.address || groups.get(left)?.[0]?.recipient || left;
    const rightName = mailboxMap.get(right)?.address || groups.get(right)?.[0]?.recipient || right;
    return leftName.localeCompare(rightName);
  });
  for (const [mailboxID, mailboxMessages] of sorted) {
    const mailbox = mailboxMap.get(mailboxID);
    const section = element("section", "mailbox-message-group");
    const header = element("div", "mailbox-message-head");
    header.tabIndex = 0;
    header.setAttribute("role", "button");
    header.setAttribute("aria-expanded", "true");
    const title = element("div");
    title.append(element("strong", "", mailbox?.address || mailboxMessages[0]?.recipient || "未知邮箱"), element("small", "", [mailbox?.label, mailbox?.forward_to_email].filter(Boolean).join(" · ") || "未同步邮箱信息"));
    const groupCount = element("span", "badge group-count", `${mailboxMessages.length} 封 ⌄`);
    header.append(title, groupCount);
    const grid = element("div", "message-grid");
    for (const message of mailboxMessages) grid.append(messageCard(message));
    const toggleGroup = () => {
      const collapsed = section.classList.toggle("collapsed");
      header.setAttribute("aria-expanded", String(!collapsed));
      groupCount.textContent = `${mailboxMessages.length} 封 ${collapsed ? "›" : "⌄"}`;
    };
    header.addEventListener("click", toggleGroup);
    header.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); toggleGroup(); } });
    section.append(header, grid); root.append(section);
  }
  $("inbox-summary").textContent = `${groups.size} 个邮箱 · ${messages.length} 封邮件`;
}

function showDelivery(data, title = "取件信息已生成") {
  $("delivery-email").textContent = data.email;
  $("delivery-code").textContent = data.pickup_key;
  $("delivery-url").textContent = data.pickup_url;
  $("delivery-text").textContent = data.delivery_text || `${data.email}---${data.pickup_key}---${data.pickup_url}`;
  $("delivery-single").classList.remove("hidden");
  $("delivery-batch-summary").classList.add("hidden");
  $("delivery").querySelector(".delivery-output-label").textContent = "完整发货文本";
  $("copy-delivery").textContent = "复制完整发货文本";
  $("delivery").querySelector("h2").textContent = title;
  $("delivery").classList.remove("hidden");
  $("delivery").scrollIntoView({ behavior: "smooth", block: "center" });
}

function showDeliveries(deliveries, failures = []) {
  $("delivery-single").classList.add("hidden");
  $("delivery-batch-summary").classList.remove("hidden");
  $("delivery-batch-summary").textContent = `成功 ${deliveries.length} 个${failures.length ? `，失败 ${failures.length} 个` : ""}。明文取件码仅在这里显示一次。`;
  $("delivery").querySelector(".delivery-output-label").textContent = "批量复制格式：邮箱-----取件码";
  $("copy-delivery").textContent = "复制全部“邮箱-----取件码”";
  $("delivery-text").textContent = deliveries.map(item => `${item.email}-----${item.pickup_key}`).join("\n");
  $("delivery").querySelector("h2").textContent = `已批量签发 ${deliveries.length} 个取件码`;
  $("delivery").classList.remove("hidden");
  $("delivery").scrollIntoView({ behavior: "smooth", block: "center" });
}

function pickupPair(data) {
  return `${data.email || data.order?.mailbox_address || ""}-----${data.pickup_key || ""}`;
}

function showPickupPairs(deliveries, failures = []) {
  $("delivery-single").classList.add("hidden");
  $("delivery-batch-summary").classList.remove("hidden");
  $("delivery-batch-summary").textContent = `已生成 ${deliveries.length} 个新取件码${failures.length ? `，失败 ${failures.length} 个` : ""}。旧码已失效，明文新码仅在这里显示一次。`;
  $("delivery").querySelector(".delivery-output-label").textContent = "复制格式：邮箱-----取件码";
  $("copy-delivery").textContent = deliveries.length > 1 ? "复制全部“邮箱-----取件码”" : "复制“邮箱-----取件码”";
  $("delivery-text").textContent = deliveries.map(pickupPair).join("\n");
  $("delivery").querySelector("h2").textContent = deliveries.length > 1 ? `已批量复制 ${deliveries.length} 个新取件码` : "新取件码已复制";
  $("delivery").classList.remove("hidden");
  $("delivery").scrollIntoView({ behavior: "smooth", block: "center" });
}

// fetchListCapped 按后端允许的最大单页上限拉取列表。后端暂不支持分页
// （无 offset 参数），当返回条数达到上限时说明数据可能被截断——此时
// 通过状态栏给出显式告警，绝不静默丢数据。
let inventoryTruncationNoted = false;
async function fetchListCapped(path, listKey, maxLimit, label) {
  const data = await api(`${path}?limit=${maxLimit}`);
  const items = (data && data[listKey]) || [];
  if (items.length >= maxLimit) {
    if (!inventoryTruncationNoted) {
      inventoryTruncationNoted = true;
      setStatus(`注意：${label}数量达到单次拉取上限（${maxLimit} 条），页面仅显示前 ${maxLimit} 条，如需完整数据请导出或联系管理员调整。`);
    }
  } else {
    inventoryTruncationNoted = false;
  }
  return items;
}

async function loadAll() {
  try {
    // 后端对 limit 有硬上限（mailboxes/orders 1000、messages 500），
    // 按 maxLimit 拉取并在触顶时显式告警（见 fetchListCapped）。
    const [accounts, stats, schedulerJobs] = await Promise.all([
      api("/api/accounts"), api("/api/post-office/stats"), api("/api/scheduler/jobs").catch(() => [])
    ]);
    const mailboxes = await fetchListCapped("/api/mailboxes", "mailboxes", 1000, "邮箱库存");
    const orders = await fetchListCapped("/api/orders", "orders", 1000, "订单");
    const messages = await fetchListCapped("/api/messages", "messages", 500, "邮件消息");
    state = { accounts: accounts || [], mailboxes, orders, messages, schedulerJobs: schedulerJobs || [] };
    lastInboxMessages = state.messages;
    renderAccounts(); renderMailboxes(); renderOrders(); renderAllocationMailboxes(); renderInboxMessages(filterInboxMessages(lastInboxMessages)); renderMessages("latest-messages", state.messages.slice(0, 6)); renderSchedulerJobs();
    $("stat-total").textContent = state.mailboxes.length;
    $("stat-available").textContent = stats.available_mailboxes;
    $("stat-messages").textContent = stats.total_messages;
    $("stat-orders").textContent = stats.active_orders;
    $("connection").innerHTML = "<i></i>后端已连接"; $("connection").classList.add("online"); setStatus("数据已更新", true);
  } catch (error) {
    $("connection").innerHTML = "<i></i>连接失败"; $("connection").classList.remove("online"); setStatus(error.message);
  }
}

const schedulerStatusLabels = { running: "运行中", paused: "已暂停", done: "已完成", error: "异常" };

function renderSchedulerJobs() {
  const rows = $("scheduler-rows"); rows.replaceChildren();
  const jobs = state.schedulerJobs || [];
  for (const job of jobs) {
    const tr = element("tr");
    const accounts = (job.account_ids || []).map(id => { const acc = state.accounts.find(item => item.id === id); return acc?.name || id; }).join("、") || "-";
    const status = cell(); status.append(element("span", `badge ${job.status}`, schedulerStatusLabels[job.status] || job.status));
    const actions = cell(); actions.className = "row-actions";
    if (job.status === "running") {
      const pause = element("button", "subtle", "暂停"); pause.dataset.schedPause = job.id; actions.append(pause);
    }
    if (job.status === "paused") {
      const resume = element("button", "subtle", "恢复"); resume.dataset.schedResume = job.id; actions.append(resume);
    }
    const remove = element("button", "danger", "删除"); remove.dataset.schedDelete = job.id;
    actions.append(remove);
    tr.append(cell(job.name || job.id), cell(accounts), cell(`${job.interval_seconds}s`), cell(job.rounds === 0 ? "无限" : String(job.rounds)), status, cell(String(job.rounds_completed || 0)), cell(String(job.created_count || 0)), cell(formatDate(job.last_round_at)), actions);
    rows.append(tr);
  }
  if (!rows.children.length) { const tr = element("tr"); const td = cell("还没有定时创建任务"); td.colSpan = 9; td.className = "empty"; tr.append(td); rows.append(tr); }
}

async function reloadSchedulerJobs() {
  try {
    state.schedulerJobs = await api("/api/scheduler/jobs").catch(() => []);
    renderSchedulerJobs();
  } catch { /* 忽略, 下次 loadAll 再同步 */ }
}

document.querySelectorAll("[data-view]").forEach(button => button.addEventListener("click", () => switchView(button.dataset.view)));
document.querySelectorAll("[data-view-jump]").forEach(button => button.addEventListener("click", () => switchView(button.dataset.viewJump)));
$("logout").addEventListener("click", async () => {
  const button = $("logout");
  setButtonBusy(button, true);
  try { await fetch("/api/auth/logout", { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: "{}" }); }
  finally { window.location.replace("/login.html"); }
});
$("collect").addEventListener("click", async () => { try { const data = await api("/api/mail/collect", { method: "POST", body: "{}" }); await loadAll(); setStatus(`收件完成：处理 ${data.processed} 封，失败 ${(data.failures || []).length} 项`, true); } catch (error) { setStatus(error.message); } });
$("sync-account").addEventListener("change", async () => { try { await loadForwarding($("sync-account").value); } catch (error) { setStatus(error.message); } });
$("sync-forward").addEventListener("change", () => {
  updateForwardingForm(state.accounts.find(account => account.id === $("sync-account").value), $("sync-forward").value);
  renderForwardingTable();
});
$("refresh-forwarding").addEventListener("click", async () => { const accountID = $("sync-account").value; if (!accountID) return setStatus("请先选择 iCloud 账号"); try { await loadForwarding(accountID); setStatus("Apple 转发设置已刷新", true); } catch (error) { setStatus(error.message); } });
$("set-default-forwarding").addEventListener("click", async () => {
  const accountID = $("sync-account").value, email = $("sync-forward").value;
  if (!accountID || !email) return setStatus("请先选择账号和转发邮箱");
  try { const account = await api(`/api/accounts/${encodeURIComponent(accountID)}/forwarding/default`, { method: "PUT", body: JSON.stringify({ email }) }); mergeAccount(account); setStatus(`Apple 默认转发目标已改为 ${email}`, true); } catch (error) { setStatus(error.message); }
});
$("authorize-forwarding").addEventListener("click", async () => {
  const accountID = $("sync-account").value, email = $("sync-forward").value;
  if (!accountID || !email) return setStatus("请先选择账号和转发邮箱");
  if (!email.toLowerCase().endsWith("@163.com")) return setStatus("目前仅支持 163 邮箱网页登录取件");
  try { await startForwardWebAuthorization(accountID, email); }
  catch (error) { $("forward-web-auth-box").classList.remove("hidden"); $("forward-web-auth-status").textContent = error.message; setStatus(error.message); }
});
$("sync").addEventListener("click", async () => {
  const accountId = $("sync-account").value, email = $("sync-forward").value;
  if (!accountId) return setStatus("请先选择 iCloud 账号");
  const account = state.accounts.find(item => item.id === accountId);
  if (!email || !forwardingMailbox(account, email)?.authorized) return setStatus("请先选择并授权转发目的邮箱，再同步库存");
  try { const data = await api("/api/inventory/sync", { method: "POST", body: JSON.stringify({ account_id: accountId }) }); await loadAll(); setStatus(`已同步 ${data.synced} 个邮箱及其转发关系`, true); } catch (error) { setStatus(error.message); }
});
$("mailbox-query").addEventListener("input", renderMailboxes);
$("mailbox-status").addEventListener("change", renderMailboxes);
$("mailbox-created-sort").addEventListener("click", () => {
  mailboxTimeSortDirection = mailboxTimeSortDirection === "desc" ? "asc" : "desc";
  renderMailboxes();
});
$("order-status").addEventListener("change", renderOrders);
$("select-active-orders").addEventListener("click", () => {
  const filter = $("order-status").value;
  for (const order of state.orders) if (order.status === "active" && (!filter || order.status === filter)) selectedOrderIDs.add(order.id);
  renderOrders();
});
$("clear-order-selection").addEventListener("click", () => { selectedOrderIDs.clear(); renderOrders(); });
$("order-rows").addEventListener("change", event => {
  const checkbox = event.target.closest("[data-order-selection]");
  if (!checkbox) return;
  if (checkbox.checked) selectedOrderIDs.add(checkbox.dataset.orderSelection); else selectedOrderIDs.delete(checkbox.dataset.orderSelection);
  renderOrders();
});
$("copy-selected-orders").addEventListener("click", async () => {
  const orders = state.orders.filter(order => order.status === "active" && selectedOrderIDs.has(order.id));
  if (!orders.length) return setStatus("请先选择至少一个有效订单");
  if (!confirm(`将为 ${orders.length} 个订单重新签发取件码并复制。所有旧取件码会立即失效，确定继续吗？`)) return;
  const button = $("copy-selected-orders"), deliveries = [], failures = [];
  setButtonBusy(button, true);
  try {
    for (let index = 0; index < orders.length; index++) {
      button.textContent = `正在处理 ${index + 1} / ${orders.length}`;
      const order = orders[index];
      try {
        const data = await api(`/api/orders/${encodeURIComponent(order.id)}/reissue`, { method: "POST", body: JSON.stringify({ ttl_hours: Number($("ttl-hours").value) || 24 }) });
        deliveries.push(data);
      } catch (error) {
        failures.push({ order, message: error.message });
      }
    }
    if (!deliveries.length) throw new Error(failures[0]?.message || "所选订单均无法重新签发");
    showPickupPairs(deliveries, failures);
    const copied = await copyText(deliveries.map(pickupPair).join("\n"));
    selectedOrderIDs.clear();
    await loadAll();
    switchView("orders");
    setStatus(`${copied ? "已复制" : "已生成"} ${deliveries.length} 行“邮箱-----取件码”${failures.length ? `，失败 ${failures.length} 个` : ""}`, copied && !failures.length);
  } catch (error) { setStatus(error.message); }
  finally { setButtonBusy(button, false); renderOrders(); }
});
$("alias-query").addEventListener("input", () => { selectedAliasIDs.clear(); renderAliasManagement(); });
$("alias-created-sort").addEventListener("click", () => {
  aliasTimeSortDirection = aliasTimeSortDirection === "desc" ? "asc" : "desc";
  renderAliasManagement();
});
$("alias-account").addEventListener("change", () => loadAliasManagement().catch(error => setStatus(error.message)));
$("refresh-aliases").addEventListener("click", () => loadAliasManagement().catch(error => setStatus(error.message)));
$("alias-select-all").addEventListener("change", () => {
  if ($("alias-select-all").checked) {
    for (const alias of filteredAliases()) if (alias.anonymousId) selectedAliasIDs.add(alias.anonymousId);
  } else selectedAliasIDs.clear();
  renderAliasManagement();
});
$("alias-rows").addEventListener("change", event => {
  const checkbox = event.target.closest("[data-alias-selection]");
  if (!checkbox) return;
  if (checkbox.checked) selectedAliasIDs.add(checkbox.dataset.aliasSelection); else selectedAliasIDs.delete(checkbox.dataset.aliasSelection);
  renderAliasManagement();
});
$("delete-selected-aliases").addEventListener("click", async () => {
  const accountID = $("alias-account").value;
  if (!accountID) return setStatus("请先选择 iCloud 账号");
  const targets = aliasSnapshot.aliases.filter(alias => alias.anonymousId && selectedAliasIDs.has(alias.anonymousId));
  if (!targets.length) return setStatus("请先勾选要删除的隐藏邮箱");
  if (!confirm(`确定永久删除 ${targets.length} 个隐藏邮箱吗？\n\n这不是停用，删除后无法在 CYMail 中恢复。`)) return;
  const button = $("delete-selected-aliases");
  aliasBatchDeleting = true;
  setButtonBusy(button, true);
  const succeeded = [], failed = [];
  try {
    for (let index = 0; index < targets.length; index++) {
      button.textContent = `正在删除 ${index + 1} / ${targets.length}`;
      const alias = targets[index];
      try {
        await api(`/api/aliases/${encodeURIComponent(alias.anonymousId)}`, { method: "DELETE", body: JSON.stringify({ account_id: accountID }) });
        succeeded.push(alias);
      } catch (error) {
        failed.push({ alias, message: error.message });
      }
    }
    selectedAliasIDs.clear();
    if (failed.length) {
      const details = failed.map(item => `${item.alias.email || item.alias.label || item.alias.anonymousId}：${item.message}`).join("；");
      setStatus(`批量删除完成：成功 ${succeeded.length} 个，失败 ${failed.length} 个。失败项：${details}`);
    } else {
      setStatus(`已永久删除 ${succeeded.length} 个隐藏邮箱`, true);
    }
    await loadAliasManagement();
    try { await api("/api/inventory/sync", { method: "POST", body: JSON.stringify({ account_id: accountID }) }); await loadAll(); } catch {}
    $("alias-account").value = accountID;
  } catch (error) {
    setStatus(error.message);
  } finally {
    aliasBatchDeleting = false;
    setButtonBusy(button, false);
    renderAliasManagement();
  }
});
$("refresh-scheduler").addEventListener("click", () => reloadSchedulerJobs().then(() => setStatus("定时任务已刷新", true)));
$("create-scheduler-job").addEventListener("click", async () => {
  const accountID = $("sched-account").value;
  if (!accountID) return setStatus("请先选择 iCloud 账号");
  const interval = Math.max(30, Math.min(86400, Math.trunc(Number($("sched-interval").value) || 300)));
  const rounds = Math.max(0, Math.min(10000, Math.trunc(Number($("sched-rounds").value) || 0)));
  const button = $("create-scheduler-job");
  setButtonBusy(button, true);
  try {
    const job = await api("/api/scheduler/jobs", { method: "POST", body: JSON.stringify({ name: $("sched-name").value.trim(), account_ids: [accountID], interval_seconds: interval, rounds }) });
    await reloadSchedulerJobs();
    setStatus(`定时任务已创建：${job.name || job.id}（每 ${interval} 秒一轮${rounds ? `，共 ${rounds} 轮` : "，无限轮"}）`, true);
  } catch (error) {
    setStatus(error.message);
  } finally {
    setButtonBusy(button, false);
  }
});
$("alias-batch-count").addEventListener("input", updateAliasBatchControls);
$("alias-create-interval").addEventListener("change", normalizedAliasCreateIntervalSeconds);
document.querySelectorAll("[data-alias-batch-count]").forEach(button => button.addEventListener("click", () => {
  $("alias-batch-count").value = button.dataset.aliasBatchCount;
  updateAliasBatchControls();
}));
$("clear-alias-batch-result").addEventListener("click", () => $("alias-batch-result").classList.add("hidden"));
$("cancel-alias-batch").addEventListener("click", () => {
  if (!aliasBatchRunning) return;
  aliasBatchCancelRequested = true;
  $("cancel-alias-batch").disabled = true;
  $("alias-create-status").textContent = "正在停止批量任务；已经成功创建的邮箱会保留…";
  $("alias-create-status").dataset.state = "warning";
});
$("create-alias").addEventListener("click", async () => {
  if (aliasBatchRunning) return;
  const accountID = $("alias-account").value;
  if (!accountID) return setStatus("请先选择 iCloud 账号");
  const intervalSeconds = normalizedAliasCreateIntervalSeconds();
  const button = $("create-alias"), batchButton = $("create-alias-batch");
  setButtonBusy(button, true);
  batchButton.disabled = true;
  updateFlow("alias-flow", ["account"], "generate");
  $("alias-create-status").textContent = "正在请求 Apple 生成并保留新地址，请勿重复点击…";
  $("alias-create-status").dataset.state = "loading";
  try {
    if (aliasSnapshot.accountID !== accountID) await loadAliasManagement();
    const label = randomAliasLabel(aliasSnapshot.aliases);
    const created = await api("/api/create", { method: "POST", body: JSON.stringify({ account_id: accountID, label, interval_seconds: intervalSeconds }) });
    updateFlow("alias-flow", ["account", "generate"], "sync");
    let syncWarning = "";
    try { await api("/api/inventory/sync", { method: "POST", body: JSON.stringify({ account_id: accountID }) }); }
    catch (error) { syncWarning = `；但库存同步失败：${error.message}`; }
    await loadAll();
    $("alias-account").value = accountID;
    await loadAliasManagement();
    $("alias-create-status").textContent = `已创建 ${created.email}，随机标签 ${created.label}${syncWarning}`;
    $("alias-create-status").dataset.state = syncWarning ? "warning" : "success";
    updateFlow("alias-flow", ["account", "generate", "sync"], "");
    setStatus(`隐藏邮箱 ${created.email} 已创建${syncWarning}`, !syncWarning);
  } catch (error) {
    $("alias-create-status").textContent = `创建失败：${error.message}`;
    $("alias-create-status").dataset.state = "error";
    setStatus(error.message);
  } finally {
    setButtonBusy(button, false);
    batchButton.disabled = false;
  }
});
$("create-alias-batch").addEventListener("click", async event => {
  event.preventDefault();
  event.stopPropagation();
  if (aliasBatchRunning || $("create-alias").disabled) return;
  const accountID = $("alias-account").value;
  if (!accountID) return setStatus("请先选择 iCloud 账号");
  const count = normalizedAliasBatchCount();
  const intervalSeconds = normalizedAliasCreateIntervalSeconds();
  if (!window.confirm(`将向 Apple 逐个申请 ${count} 个隐藏邮箱，创建间隔 ${intervalSeconds} 秒。已成功创建的地址不会因后续失败而撤销，确认继续吗？`)) return;

  const batchButton = $("create-alias-batch"), singleButton = $("create-alias");
  const cancelButton = $("cancel-alias-batch"), accountSelect = $("alias-account");
  const created = [];
  let failure = "", syncWarning = "";
  aliasBatchRunning = true;
  aliasBatchCancelRequested = false;
  setButtonBusy(batchButton, true);
  singleButton.disabled = true;
  accountSelect.disabled = true;
  cancelButton.disabled = false;
  cancelButton.classList.remove("hidden");
  $("alias-batch-count").disabled = true;
  $("alias-create-interval").disabled = true;
  document.querySelectorAll("[data-alias-batch-count]").forEach(button => { button.disabled = true; });
  updateAliasBatchControls();
  updateAliasBatchProgress(0, count);
  $("alias-batch-result").classList.add("hidden");
  updateFlow("alias-flow", ["account"], "generate");

  try {
    let index = 0;
    while (index < count && !aliasBatchCancelRequested) {
      $("alias-create-status").textContent = `正在创建第 ${index + 1} / ${count} 个；已成功 ${created.length} 个，请勿关闭页面…`;
      $("alias-create-status").dataset.state = "loading";
      try {
        const item = await api("/api/create", { method: "POST", body: JSON.stringify({ account_id: accountID, interval_seconds: intervalSeconds }) });
        created.push(item);
        index++;
        updateAliasBatchProgress(created.length, count);
      } catch (error) {
        if (error.code === "alias_rate_limited" || error.status === 429) {
          const retryAfterSeconds = Math.max(1, error.retryAfterSeconds || 3600);
          if (!await waitForAliasBatchRetry(retryAfterSeconds, index, count, created.length)) {
            failure = "批量任务已由你停止";
            break;
          }
          continue;
        }
        failure = error.message;
        break;
      }
      if (index < count && !aliasBatchCancelRequested && intervalSeconds > 0) await wait(intervalSeconds * 1000);
    }
    if (aliasBatchCancelRequested && !failure) failure = "批量任务已由你停止";

    if (created.length) {
      updateFlow("alias-flow", ["account", "generate"], "sync");
      $("alias-create-status").textContent = `已创建 ${created.length} 个，正在统一同步库存…`;
      try { await api("/api/inventory/sync", { method: "POST", body: JSON.stringify({ account_id: accountID }) }); }
      catch (error) { syncWarning = `库存同步失败：${error.message}`; }
      await loadAll();
      $("alias-account").value = accountID;
      await loadAliasManagement();
    }

    renderAliasBatchResult(created, failure || syncWarning);
    const summary = failure ? `批量任务已停止：成功 ${created.length} / ${count} 个；${failure}` : syncWarning ? `已创建 ${created.length} 个；${syncWarning}` : `批量创建完成：成功 ${created.length} 个`;
    $("alias-create-status").textContent = summary;
    $("alias-create-status").dataset.state = failure || syncWarning ? "warning" : "success";
    updateFlow("alias-flow", ["account", ...(created.length ? ["generate", "sync"] : [])], "");
    setStatus(summary, !failure && !syncWarning);
  } catch (error) {
    failure = failure || error.message;
    renderAliasBatchResult(created, failure);
    $("alias-create-status").textContent = `批量任务已停止：成功 ${created.length} / ${count} 个；${failure}`;
    $("alias-create-status").dataset.state = "warning";
    setStatus(failure);
  } finally {
    aliasBatchRunning = false;
    aliasBatchCancelRequested = false;
    setButtonBusy(batchButton, false);
    singleButton.disabled = false;
    accountSelect.disabled = false;
    cancelButton.disabled = false;
    cancelButton.classList.add("hidden");
    $("alias-batch-count").disabled = false;
    $("alias-create-interval").disabled = false;
    document.querySelectorAll("[data-alias-batch-count]").forEach(button => { button.disabled = false; });
    updateAliasBatchControls();
  }
});
window.addEventListener("beforeunload", event => {
  if (!aliasBatchRunning) return;
  event.preventDefault();
  event.returnValue = "";
});
$("message-account").addEventListener("change", () => { renderMessageForwardOptions(); renderInboxMessages(filterInboxMessages(lastInboxMessages)); });
$("message-forward").addEventListener("change", () => { renderMessageMailboxOptions(); renderInboxMessages(filterInboxMessages(lastInboxMessages)); });
$("message-mailbox").addEventListener("change", () => renderInboxMessages(filterInboxMessages(lastInboxMessages)));
$("message-mailbox-query").addEventListener("input", () => {
  const query = $("message-mailbox-query").value.trim().toLowerCase();
  const exact = state.mailboxes.find(mailbox => mailbox.address.toLowerCase() === query || (mailbox.label || "").toLowerCase() === query);
  if (exact && [...$("message-mailbox").options].some(option => option.value === exact.id)) $("message-mailbox").value = exact.id;
  else if (!query) $("message-mailbox").value = "";
  renderInboxMessages(filterInboxMessages(lastInboxMessages));
});
$("order-mailbox-query").addEventListener("input", renderAllocationMailboxes);
$("select-all-mailboxes").addEventListener("click", () => { for (const mailbox of availableAllocationMailboxes()) selectedAllocationMailboxIDs.add(mailbox.id); renderAllocationMailboxes(); });
$("clear-mailbox-selection").addEventListener("click", () => { selectedAllocationMailboxIDs.clear(); renderAllocationMailboxes(); });
$("allocation-mailboxes").addEventListener("change", event => {
  const checkbox = event.target.closest("[data-allocation-mailbox]");
  if (!checkbox) return;
  if (checkbox.checked) selectedAllocationMailboxIDs.add(checkbox.dataset.allocationMailbox); else selectedAllocationMailboxIDs.delete(checkbox.dataset.allocationMailbox);
  renderAllocationMailboxes();
});
$("collect-selected").addEventListener("click", async () => {
  const accountID = $("message-account").value, forwardTo = $("message-forward").value;
  if (!accountID || !forwardTo) return setStatus("请先选择 iCloud 账号和收件邮箱");
  try { const data = await api("/api/mail/collect", { method: "POST", body: JSON.stringify({ account_id: accountID, forward_to: forwardTo }) }); await loadAll(); $("message-account").value = accountID; renderMessageForwardOptions(); $("message-forward").value = forwardTo; setStatus(`已从 ${forwardTo} 同步 ${data.processed} 封，失败 ${(data.failures || []).length} 项`, !(data.failures || []).length); } catch (error) { setStatus(error.message); }
});
$("search-messages").addEventListener("click", async () => { const params = new URLSearchParams({ limit: "500" }); if ($("message-query").value.trim()) params.set("q", $("message-query").value.trim()); if ($("message-account").value) params.set("account_id", $("message-account").value); if ($("message-mailbox").value) params.set("mailbox_id", $("message-mailbox").value); try { const data = await api(`/api/messages?${params}`); lastInboxMessages = data.messages || []; const messages = filterInboxMessages(lastInboxMessages); renderInboxMessages(messages); setStatus(`找到 ${messages.length} 封邮件，已按隐藏邮箱分类`, true); } catch (error) { setStatus(error.message); } });
$("allocate").addEventListener("click", async () => {
  const button = $("allocate");
  const mailboxIDs = [...selectedAllocationMailboxIDs];
  const payload = { external_id: $("external-id").value.trim(), ttl_hours: Number($("ttl-hours").value) };
  setButtonBusy(button, true);
  try {
    if (mailboxIDs.length > 1) {
      const data = await api("/api/orders/allocate-batch", { method: "POST", body: JSON.stringify({ ...payload, mailbox_ids: mailboxIDs }) });
      showDeliveries(data.deliveries || [], data.failures || []);
      await loadAll(); switchView("orders");
      setStatus(`已签发 ${data.count} 个取件码${(data.failures || []).length ? `，失败 ${(data.failures || []).length} 个` : ""}，请立即复制全部发货文本`, !(data.failures || []).length);
    } else {
      if (mailboxIDs.length === 1) payload.mailbox_id = mailboxIDs[0];
      const data = await api("/api/orders/allocate", { method: "POST", body: JSON.stringify(payload) });
      showDelivery(data); await loadAll(); switchView("orders"); setStatus("邮箱和取件码已生成，请立即复制发货文本", true);
    }
  } catch (error) { setStatus(error.message); }
  finally { setButtonBusy(button, false); renderAllocationMailboxes(); }
});
$("copy-delivery").addEventListener("click", () => copyText($("delivery-text").textContent));
$("add-account").addEventListener("click", async () => {
  const name = $("account-name").value.trim();
  if (!name) return setStatus("请填写账号名称");
  try {
    const account = await api("/api/accounts", { method: "POST", body: JSON.stringify({ name, host: $("account-host").value, cookies: "" }) });
    $("account-name").value = "";
    await loadAll();
    await startBrowserAuthorization(account.id);
  } catch (error) { setStatus(error.message); }
});
function handoffAuthorizationToExtension(payload) {
  return new Promise((resolve, reject) => {
    const requestID = crypto.randomUUID();
    const timeout = setTimeout(() => {
      window.removeEventListener("message", receive);
      reject(new Error("未检测到新版 CYMail 扩展：请在扩展管理页点击重新加载；若当前是生产管理域名，请先运行 configure-domain.ps1 写入域名后再重试"));
    }, 2000);
    function receive(event) {
      const message = event.data;
      if (event.source !== window || event.origin !== window.location.origin) return;
      if (!message || message.channel !== browserAuthChannel || message.type !== "result" || message.request_id !== requestID) return;
      clearTimeout(timeout);
      window.removeEventListener("message", receive);
      if (message.ok) resolve();
      else reject(new Error(message.error || "扩展拒绝了授权请求"));
    }
    window.addEventListener("message", receive);
    window.postMessage({ channel: browserAuthChannel, type: "begin", request_id: requestID, payload }, window.location.origin);
  });
}

$("open-browser-auth").addEventListener("click", async () => {
  if (!browserAuthHandoff) return setStatus("请先为账号生成授权请求");
  const handoff = handoffAuthorizationToExtension(browserAuthHandoff.payload);
  window.open(browserAuthHandoff.target, "_blank", "noopener,noreferrer");
  $("browser-auth-status").textContent = "正在连接 CYMail 扩展…";
  try {
    await handoff;
    $("browser-auth-status").textContent = "扩展已接收授权请求。请在 iCloud 页面右上角的 CYMail 浮层中确认授权。";
  } catch (error) {
    $("browser-auth-status").textContent = error.message;
    setStatus(error.message);
  }
});

document.addEventListener("click", async event => {
  const aliasAction = event.target.closest("[data-alias-action]");
  if (aliasAction) {
    const accountID = $("alias-account").value;
    const anonymousID = aliasAction.dataset.aliasId;
    const action = aliasAction.dataset.aliasAction;
    if (!accountID || !anonymousID) return setStatus("隐藏邮箱操作参数不完整");
    if (action === "delete" && !confirm(`确定永久删除 ${aliasAction.dataset.aliasEmail || "该隐藏邮箱"} 吗？\n\n这不是停用，删除后无法在 CYMail 中恢复。`)) return;
    aliasAction.disabled = true;
    try {
      const path = `/api/aliases/${encodeURIComponent(anonymousID)}${action === "delete" ? "" : `/${action}`}`;
      const result = await api(path, { method: action === "delete" ? "DELETE" : "POST", body: JSON.stringify({ account_id: accountID }) });
      if (action !== "delete" && result?.success !== true) throw new Error(`Apple 未确认${action === "deactivate" ? "停用" : "恢复"}操作`);
      await loadAliasManagement();
      try { await api("/api/inventory/sync", { method: "POST", body: JSON.stringify({ account_id: accountID }) }); await loadAll(); } catch {}
      $("alias-account").value = accountID;
      setStatus(action === "delete" ? "隐藏邮箱已永久删除" : action === "deactivate" ? "隐藏邮箱已停用，可随时恢复" : "隐藏邮箱已恢复", true);
    } catch (error) {
      aliasAction.disabled = false;
      setStatus(error.message);
    }
    return;
  }
  const manageForward = event.target.closest("[data-manage-forward-account]");
  if (manageForward) {
    const accountID = manageForward.dataset.manageForwardAccount;
    const email = manageForward.dataset.manageForwardEmail;
    $("sync-account").value = accountID;
    const account = state.accounts.find(item => item.id === accountID);
    renderForwarding(account);
    $("sync-forward").value = email;
    updateForwardingForm(account, email);
    switchView("forwarding");
    return;
  }
  const remove = event.target.closest("[data-delete-account]");
  if (remove) {
    const accountID = remove.dataset.deleteAccount;
    const account = state.accounts.find(item => item.id === accountID);
    const label = account?.name ? `“${account.name}” (${accountID})` : accountID;
    if (!window.confirm(`确定删除账号 ${label}？\n\n此操作会移除该账号在 CYMail 中保存的登录配置。`)) return;
    remove.disabled = true;
    try {
      await api(`/api/accounts/${encodeURIComponent(accountID)}`, { method: "DELETE" });
      if (browserAuthRequest?.accountID === accountID) {
        clearInterval(browserAuthPoll); browserAuthPoll = null;
        browserAuthRequest = null; browserAuthHandoff = null;
        $("open-browser-auth").disabled = true;
        $("browser-auth-box").classList.add("hidden");
      }
      await loadAll();
      setStatus(`账号 ${label} 已删除`, true);
    } catch (error) {
      remove.disabled = false;
      setStatus(error.message);
    }
    return;
  }
  const authorize = event.target.closest("[data-authorize-account]");
  if (authorize) { try { await startBrowserAuthorization(authorize.dataset.authorizeAccount); } catch (error) { setStatus(error.message); } return; }
  const applePaste = event.target.closest("[data-apple-account-paste]");
  if (applePaste) {
    const accountID = applePaste.dataset.appleAccountPaste;
    const account = state.accounts.find(item => item.id === accountID);
    const raw = window.prompt(
      `为账号「${account?.name || accountID}」粘贴 account.apple.com 会话 Cookie（Header 字符串或 JSON）。\n\n获取方式：浏览器打开 https://account.apple.com 并登录，按 F12 → Application → Cookies，复制该域名的 Cookie。\n\n留空可取消。`
    );
    if (raw === null) return;
    if (!String(raw).trim()) { setStatus("已取消"); return; }
    applePaste.disabled = true;
    try {
      const data = await api(`/api/accounts/${encodeURIComponent(accountID)}/apple-account`, { method: "PUT", body: JSON.stringify({ cookies: raw }) });
      await loadAll();
      setStatus(`新接口已启用（${data.apple_account?.last_status || "会话正常"}），创建配额约 25 个/小时`, true);
    } catch (error) {
      setStatus(error.message);
    } finally {
      applePaste.disabled = false;
    }
    return;
  }
  const appleClear = event.target.closest("[data-apple-account-clear]");
  if (appleClear) {
    if (!confirm("确定清除该账号的新接口会话？创建将回退旧接口（约 5 个/小时）。")) return;
    appleClear.disabled = true;
    try {
      await api(`/api/accounts/${encodeURIComponent(appleClear.dataset.appleAccountClear)}/apple-account`, { method: "DELETE" });
      await loadAll();
      setStatus("新接口会话已清除", true);
    } catch (error) {
      setStatus(error.message);
    } finally {
      appleClear.disabled = false;
    }
    return;
  }
  const appleLogin = event.target.closest("[data-apple-account-login]");
  if (appleLogin) {
    const accountID = appleLogin.dataset.appleAccountLogin;
    const account = state.accounts.find(item => item.id === accountID);
    const password = window.prompt(`为账号「${account?.name || accountID}」输入 Apple ID 密码以登录新接口（密码不会保存）。\n\n留空可取消。`);
    if (password === null || !String(password).trim()) { setStatus("已取消"); return; }
    appleLogin.disabled = true;
    try {
      let body = { password };
      try {
        const data = await api(`/api/accounts/${encodeURIComponent(accountID)}/apple-account/login`, { method: "PUT", body: JSON.stringify(body) });
        await loadAll();
        setStatus("新接口密码登录成功，创建配额约 25 个/小时", true);
        return;
      } catch (error) {
        const code = error?.payload?.code || "";
        const needs2FA = /409/.test(String(error?.status || "")) || String(error.message || "").includes("双重认证") || /otp_code/.test(String(error.message || ""));
        if (!needs2FA && code !== "apple_2fa_required") throw error;
        const otp = window.prompt("该账号启用了双重认证：请查看受信任设备上的 6 位验证码并输入。\n\n留空可取消。");
        if (otp === null || !String(otp).trim()) { setStatus("已取消（未登录）"); return; }
        body.otp_code = String(otp).trim();
        const data = await api(`/api/accounts/${encodeURIComponent(accountID)}/apple-account/login`, { method: "PUT", body: JSON.stringify(body) });
        await loadAll();
        setStatus("新接口密码登录成功（含双重认证），创建配额约 25 个/小时", true);
      }
    } catch (error) {
      setStatus(error.message);
    } finally {
      appleLogin.disabled = false;
    }
    return;
  }
  const schedPause = event.target.closest("[data-sched-pause]");
  if (schedPause) {
    try { await api(`/api/scheduler/jobs/${encodeURIComponent(schedPause.dataset.schedPause)}/pause`, { method: "POST", body: "{}" }); await reloadSchedulerJobs(); setStatus("定时任务已暂停", true); } catch (error) { setStatus(error.message); }
    return;
  }
  const schedResume = event.target.closest("[data-sched-resume]");
  if (schedResume) {
    try { await api(`/api/scheduler/jobs/${encodeURIComponent(schedResume.dataset.schedResume)}/resume`, { method: "POST", body: "{}" }); await reloadSchedulerJobs(); setStatus("定时任务已恢复", true); } catch (error) { setStatus(error.message); }
    return;
  }
  const schedDelete = event.target.closest("[data-sched-delete]");
  if (schedDelete) {
    if (!confirm("确定删除该定时创建任务？")) return;
    try { await api(`/api/scheduler/jobs/${encodeURIComponent(schedDelete.dataset.schedDelete)}`, { method: "DELETE" }); await reloadSchedulerJobs(); setStatus("定时任务已删除", true); } catch (error) { setStatus(error.message); }
    return;
  }
  const inbox = event.target.closest("[data-mailbox-inbox]");
  if (inbox) {
    $("message-account").value = ""; renderMessageForwardOptions(); $("message-forward").value = ""; renderMessageMailboxOptions();
    $("message-mailbox-query").value = ""; $("message-mailbox").value = inbox.dataset.mailboxInbox;
    switchView("inbox"); $("search-messages").click(); return;
  }
  const orderView = event.target.closest("[data-order-view]"); if (orderView) { switchView("orders"); return; }
  const copyOrder = event.target.closest("[data-copy-order]");
  if (copyOrder) {
    if (!confirm("复制已签发订单需要生成一个新取件码，旧码会立即失效。确定继续吗？")) return;
    setButtonBusy(copyOrder, true);
    try {
      const data = await api(`/api/orders/${encodeURIComponent(copyOrder.dataset.copyOrder)}/reissue`, { method: "POST", body: JSON.stringify({ ttl_hours: Number($("ttl-hours").value) || 24 }) });
      showPickupPairs([data]);
      const copied = await copyText(pickupPair(data));
      await loadAll();
      switchView("orders");
      setStatus(copied ? "已复制：邮箱-----取件码；旧取件码已经失效" : "新取件码已生成，请从上方手动复制", copied);
    } catch (error) { setStatus(error.message); }
    finally { setButtonBusy(copyOrder, false); }
    return;
  }
  const reissue = event.target.closest("[data-reissue]");
  if (reissue) {
    if (!confirm("重新签发后，旧取件码会立即失效。确定继续吗？")) return;
    try { const data = await api(`/api/orders/${encodeURIComponent(reissue.dataset.reissue)}/reissue`, { method: "POST", body: JSON.stringify({ ttl_hours: Number($("ttl-hours").value) || 24 }) }); showDelivery(data, "新取件码已签发"); await loadAll(); switchView("orders"); setStatus("重新签发成功，旧取件码已经失效", true); } catch (error) { setStatus(error.message); }
  }
});

setInterval(updateCountdowns, 1000);
updateAliasBatchControls();
loadAll();
