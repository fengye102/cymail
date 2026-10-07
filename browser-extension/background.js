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
  if (!message || message.type !== "cymail_complete_pending_auth") return false;
  completePendingAuthorization(sender)
    .then(data => sendResponse({ ok: true, data }))
    .catch(error => sendResponse({ ok: false, error: error.message || String(error) }));
  return true;
});
