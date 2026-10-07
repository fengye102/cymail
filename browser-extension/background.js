const appleCookieURLs = [
  "https://www.icloud.com/",
  "https://setup.icloud.com/",
  "https://www.icloud.com.cn/",
  "https://setup.icloud.com.cn/",
  "https://idmsa.apple.com/",
  "https://account.apple.com/",
  "https://appleid.apple.com/"
];
const neteaseCookieURLs = [
  "https://mail.163.com/",
  "https://reg.163.com/",
  "https://dl.reg.163.com/"
];

function validICloudSender(sender) {
  try {
    const url = new URL(sender.url || "");
    return url.protocol === "https:" && (
      url.hostname === "icloud.com" || url.hostname.endsWith(".icloud.com") ||
      url.hostname === "icloud.com.cn" || url.hostname.endsWith(".icloud.com.cn")
    );
  } catch {
    return false;
  }
}

function validNeteaseSender(sender) {
  try {
    const url = new URL(sender.url || "");
    return url.protocol === "https:" && (url.hostname === "mail.163.com" || url.hostname.endsWith(".mail.163.com"));
  } catch {
    return false;
  }
}

function validPendingAuthorization(value) {
  if (!value) return false;
  const pattern = value.kind === "forwarding" ? /^fwa_[A-Za-z0-9_-]{32,100}$/ : /^iba_[A-Za-z0-9_-]{32,100}$/;
  if (!pattern.test(value.authorization_code || "")) return false;
  const expiresAt = new Date(value.expires_at).getTime();
  return Number.isFinite(expiresAt) && expiresAt > Date.now();
}

function normalizeBackend(value) {
  const url = new URL(String(value || "").trim());
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("CYMail 后台地址无效");
  if (!url.pathname.endsWith("/")) url.pathname += "/";
  url.search = "";
  url.hash = "";
  return url;
}

function backendPortOf(backend) {
  return backend.port || (backend.protocol === "https:" ? "443" : "80");
}

async function collectICloudCookies() {
  const byName = new Map();
  for (const url of appleCookieURLs) {
    for (const cookie of await chrome.cookies.getAll({ url })) {
      if (cookie.value) byName.set(cookie.name, cookie.value);
    }
  }
  return Object.fromEntries(byName);
}

async function collectNeteaseCookies() {
  const byName = new Map();
  for (const url of neteaseCookieURLs) {
    for (const cookie of await chrome.cookies.getAll({ url })) {
      if (cookie.value) byName.set(cookie.name, cookie.value);
    }
  }
  return Object.fromEntries(byName);
}

function extractSessionID(pageURL, cookies) {
  const decoded = String(pageURL || "");
  const match = decoded.match(/[?&#]sid=([^&#]+)/i);
  if (match) {
    try { return decodeURIComponent(match[1]).slice(0, 512); } catch { return match[1].slice(0, 512); }
  }
  for (const name of ["Coremail.sid", "sid", "MAIL_SID"]) {
    if (cookies[name]) return String(cookies[name]).slice(0, 512);
  }
  return "";
}

async function storePendingAuthorization(sender, message) {
  // 只接受来自扩展自身内容脚本的请求（content script 消息的 sender.tab / id
  // 由 Chrome 填写，页面无法伪造），并核实声明的页面来源与实际 tab 来源一致。
  if (!sender.tab || !sender.origin) throw new Error("授权请求必须来自扩展内容脚本");
  const tab = sender.tab;
  if (tab.id === undefined || sender.frameId !== 0) throw new Error("授权请求必须来自管理页顶层框架");
  const declared = String(message.declared_origin || "");
  const actualOrigin = new URL(tab.url || "").origin;
  if (declared !== actualOrigin) throw new Error("管理页来源校验失败");

  const payload = message.payload || {};
  const backend = normalizeBackend(payload.backend);
  if (backend.origin !== actualOrigin) throw new Error("后台地址与当前管理页面不一致");
  const authorizationCode = String(payload.authorization_code || "").trim();
  const expiresAt = new Date(payload.expires_at).getTime();
  const pattern = payload.kind === "forwarding" ? /^fwa_[A-Za-z0-9_-]{32,100}$/ : /^iba_[A-Za-z0-9_-]{32,100}$/;
  if (!pattern.test(authorizationCode)) throw new Error("一次性授权凭证无效");
  if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) throw new Error("一次性授权请求已过期");

  const entry = {
    backend: backend.href,
    authorization_code: authorizationCode,
    expires_at: new Date(expiresAt).toISOString(),
    kind: payload.kind === "forwarding" ? "forwarding" : "icloud",
    email: payload.kind === "forwarding" ? String(payload.email || "").toLowerCase() : "",
    provider: payload.kind === "forwarding" ? payload.provider : ""
  };
  if (entry.kind === "forwarding") {
    if (entry.provider !== "netease_163" || !/@163\.com$/i.test(entry.email)) {
      throw new Error("163 网页授权目标无效");
    }
  }
  // 记录来源端口；Chrome match pattern 不含端口，后续提交前用它核实
  // 授权请求确实来自申请权限时的同一端口（L3）。
  entry.backend_port = backend.port || (backend.protocol === "https:" ? "443" : "80");
  entry.page_origin = actualOrigin;
  await chrome.storage.local.set({ cymail_pending_auth: entry });
  return { ok: true };
}

function backendPortOf(backend) {
  return backend.port || (backend.protocol === "https:" ? "443" : "80");
}

async function completePendingAuthorization(sender) {
  if ((sender.url || "").startsWith(chrome.runtime.getURL(""))) {
    const [activeTab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (activeTab?.url) sender = { ...sender, url: activeTab.url };
  }
  const saved = await chrome.storage.local.get("cymail_pending_auth");
  const pending = saved.cymail_pending_auth;
  if (!validPendingAuthorization(pending)) {
    await chrome.storage.local.remove("cymail_pending_auth");
    throw new Error("授权请求不存在或已过期，请返回 CYMail 后台重新开始");
  }

  const backend = normalizeBackend(pending.backend);
  const originPattern = `${backend.protocol}//${backend.hostname}/*`;
  if (!await chrome.permissions.contains({ origins: [originPattern] })) {
    throw new Error(`扩展没有访问 ${backend.host} 的权限，请重新加载扩展后重试`);
  }
  // L3：Chrome match pattern 不含端口，这里用存储的端口核实提交目标
  // 与授权申请时的端口一致，避免同主机任意端口都通过校验。
  if (pending.page_origin && new URL(pending.page_origin).origin !== backend.origin) {
    await chrome.storage.local.remove("cymail_pending_auth");
    throw new Error("后台地址与发起授权的管理页不一致，请返回 CYMail 后台重新开始");
  }
  if (pending.backend_port && backendPortOf(backend) !== pending.backend_port) {
    await chrome.storage.local.remove("cymail_pending_auth");
    throw new Error("后台端口与发起授权时不一致，请返回 CYMail 后台重新开始");
  }

  if (pending.kind === "forwarding") {
    if (!validNeteaseSender(sender)) throw new Error("只能从 163 官方邮箱页面确认收件授权");
    const cookies = await collectNeteaseCookies();
    if (!Object.keys(cookies).length) throw new Error("没有检测到 163 登录会话，请先进入 163 收件箱");
    const sessionID = extractSessionID(sender.url, cookies);
    if (!sessionID) throw new Error("没有检测到 163 收件箱会话 SID，请刷新收件箱页面后重试");
    const response = await fetch(new URL("public/forwarding/web-auth/complete", backend).href, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        authorization_code: pending.authorization_code,
        email: pending.email,
        provider: pending.provider,
        cookies,
        session_id: sessionID
      })
    });
    let payload;
    try { payload = await response.json(); } catch { throw new Error("CYMail 后台返回了无效响应"); }
    if (!response.ok || !payload.success) throw new Error(payload.message || "CYMail 后台拒绝了 163 授权");
    await chrome.storage.local.remove("cymail_pending_auth");
    return { kind: "forwarding", email: pending.email, cookies_count: payload.data.cookies_count, backend: backend.host };
  }

  if (!validICloudSender(sender)) throw new Error("只能从 Apple 官方 iCloud 页面确认授权");

  const cookies = await collectICloudCookies();
  if (!Object.keys(cookies).length) throw new Error("没有检测到 iCloud 登录会话，请先完成 Apple 登录");

  const response = await fetch(new URL("public/browser-auth/complete", backend).href, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ authorization_code: pending.authorization_code, cookies })
  });
  let payload;
  try { payload = await response.json(); } catch { throw new Error("CYMail 后台返回了无效响应"); }
  if (!response.ok || !payload.success) throw new Error(payload.message || "CYMail 后台拒绝了授权");

  await chrome.storage.local.remove("cymail_pending_auth");
  return { cookies_count: payload.data.cookies_count, backend: backend.host };
}

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (!message || typeof message !== "object") return false;
  if (message.type === "cymail_store_pending_auth") {
    storePendingAuthorization(sender, message)
      .then(data => sendResponse(data))
      .catch(error => sendResponse({ ok: false, error: error.message || String(error) }));
    return true;
  }
  if (message.type !== "cymail_complete_pending_auth") return false;
  completePendingAuthorization(sender)
    .then(data => sendResponse({ ok: true, data }))
    .catch(error => sendResponse({ ok: false, error: error.message || String(error) }));
  return true;
});
