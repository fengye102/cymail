const $ = id => document.getElementById(id);
const appleCookieURLs = [
  "https://www.icloud.com/",
  "https://setup.icloud.com/",
  "https://www.icloud.com.cn/",
  "https://setup.icloud.com.cn/",
  "https://idmsa.apple.com/"
];

function setStatus(message, ok = false) {
  $("status").textContent = message;
  $("status").classList.toggle("ok", ok);
}

function normalizeBackend(value) {
  const url = new URL(value.trim());
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("后台地址必须使用 HTTP 或 HTTPS");
  if (!url.pathname.endsWith("/")) url.pathname += "/";
  url.search = "";
  url.hash = "";
  return url;
}

function validPendingAuthorization(value) {
  if (!value) return false;
  const pattern = value.kind === "forwarding" ? /^fwa_[A-Za-z0-9_-]{32,100}$/ : /^iba_[A-Za-z0-9_-]{32,100}$/;
  if (!pattern.test(value.authorization_code || "")) return false;
  const expiresAt = new Date(value.expires_at).getTime();
  return Number.isFinite(expiresAt) && expiresAt > Date.now();
}

async function getCookies(url) {
  return await chrome.cookies.getAll({ url });
}

async function collectICloudCookies() {
  const byName = new Map();
  for (const url of appleCookieURLs) {
    for (const cookie of await getCookies(url)) {
      if (cookie.value) byName.set(cookie.name, cookie.value);
    }
  }
  return Object.fromEntries(byName);
}

async function requestBackendPermission(url) {
	// Chrome match patterns do not include ports. Grant only the selected host;
	// the permission still covers the explicitly entered local development port.
	const originPattern = `${url.protocol}//${url.hostname}/*`;
  const granted = await chrome.permissions.request({ origins: [originPattern] });
  if (!granted) throw new Error("未授予访问 CYMail 后台地址的权限");
}

async function restore() {
  const saved = await chrome.storage.local.get("cymail_pending_auth");
  if (!validPendingAuthorization(saved.cymail_pending_auth)) {
    await chrome.storage.local.remove("cymail_pending_auth");
    setStatus("请返回 CYMail 后台，点击“一键打开 iCloud 授权”。");
    return;
  }
  const pending = saved.cymail_pending_auth;
  const backend = normalizeBackend(pending.backend);
  $("backend-host").textContent = backend.host;
  $("expires-at").textContent = `授权链接有效期至 ${new Date(pending.expires_at).toLocaleTimeString("zh-CN", { hour12: false })}`;
  $("pending").classList.remove("hidden");
  $("authorize").disabled = false;
  const isForwarding = pending.kind === "forwarding";
  $("open").textContent = isForwarding ? "重新打开 163 官方邮箱" : "重新打开 iCloud 官方登录";
  $("authorize").textContent = isForwarding ? "我已进入收件箱，确认授权" : "我已登录，确认授权 CYMail";
  setStatus(isForwarding ? `授权请求已接收。进入 ${pending.email} 的 163 收件箱后请确认。` : "授权请求已接收。登录 iCloud 后请点击确认。", true);
}

$("open").addEventListener("click", async () => {
  const saved = await chrome.storage.local.get("cymail_pending_auth");
  const forwarding = saved.cymail_pending_auth?.kind === "forwarding";
  await chrome.tabs.create({ url: forwarding ? "https://mail.163.com/" : "https://www.icloud.com/" });
  setStatus(forwarding ? "请在新标签页完成 163 官方登录并进入收件箱。" : "请在新标签页完成 Apple 官方登录。", true);
});

$("authorize").addEventListener("click", async () => {
  const button = $("authorize");
  button.disabled = true;
  try {
    const saved = await chrome.storage.local.get("cymail_pending_auth");
    if (!validPendingAuthorization(saved.cymail_pending_auth)) throw new Error("授权请求不存在或已过期，请从 CYMail 后台重新开始");
    const backend = normalizeBackend(saved.cymail_pending_auth.backend);
    await requestBackendPermission(backend);
    const response = await chrome.runtime.sendMessage({ type: "cymail_complete_pending_auth" });
    if (!response?.ok) throw new Error(response?.error || "CYMail 后台拒绝了授权");
    $("pending").classList.add("hidden");
    setStatus(`授权成功，已提交 ${response.data.cookies_count} 个网页会话 Cookie。`, true);
  } catch (error) {
    setStatus(error.message || String(error));
  } finally {
    const saved = await chrome.storage.local.get("cymail_pending_auth");
    button.disabled = !validPendingAuthorization(saved.cymail_pending_auth);
  }
});

restore();
