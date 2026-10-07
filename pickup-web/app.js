const $ = id => document.getElementById(id);

let pickupEmail = "";
let pickupKey = "";
let mailbox = null;
let selected = null;
let remoteImagesEnabled = false;
let refreshTimer = null;
let countdownTimer = null;
let toastTimer = null;

const credential = $("credential");
const submit = $("submit");
const status = $("status");
const accessPanel = $("access-panel");
const workspace = $("workspace");
const messageList = $("message-list");
const normalizedPath = location.pathname.replace(/\/+$/, "");
const pickupPrefix = normalizedPath.endsWith("/pickup") ? normalizedPath.slice(0, -"/pickup".length) : "";
const pickupEndpoint = `${pickupPrefix}/public/pickup` || "/public/pickup";
const { validPickupKey, parseCredential } = globalThis.CYMailPickupCredential;

function node(tag, cls, text) {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text !== undefined) element.textContent = text;
  return element;
}

function parseDate(value) {
  if (value === null || value === undefined || value === "") return null;
  const raw = String(value).trim();
  let normalized = value;
  if (/^-?\d+$/.test(raw)) {
    const numeric = Number(raw);
    normalized = Math.abs(numeric) < 1e12 ? numeric * 1000 : numeric;
  }
  const date = new Date(normalized);
  return Number.isNaN(date.getTime()) ? null : date;
}

function formatDate(value) {
  const date = parseDate(value);
  return date ? date.toLocaleString("zh-CN", { hour12: false }) : "-";
}

function formatCompactDate(value) {
  if (!value) return "-";
  const date = parseDate(value);
  if (!date) return "-";
  const pad = number => String(number).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

function formatSender(value) {
  const sender = (value || "未知发件人").trim();
  const named = sender.split("<")[0].trim();
  return named || sender;
}

function formatRecipient(value) {
  const recipient = (value || pickupEmail || "").trim();
  const match = recipient.match(/<([^>]+)>/);
  return match ? match[1] : recipient;
}

function showToast(text) {
  const toast = $("toast");
  toast.textContent = text;
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 1800);
}

async function copyText(text) {
  try {
    let copied = false;
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(text);
        copied = true;
      } catch {}
    }
    if (!copied) {
      const area = node("textarea");
      area.value = text;
      area.setAttribute("readonly", "");
      area.style.cssText = "position:fixed;left:-9999px;top:0";
      document.body.append(area);
      area.select();
      copied = document.execCommand("copy");
      area.remove();
    }
    if (!copied) throw new Error("copy unavailable");
    showToast("已复制");
  } catch {
    showToast("复制失败，请手动复制");
  }
}

function decodeLegacyKey(value) {
  const prefix = "tok_";
  if (!value.startsWith(prefix)) return null;
  try {
    const encoded = value.slice(prefix.length).replace(/-/g, "+").replace(/_/g, "/");
    const padded = encoded + "=".repeat((4 - encoded.length % 4) % 4);
    const bytes = Uint8Array.from(atob(padded), character => character.charCodeAt(0));
    const [token, code] = JSON.parse(new TextDecoder().decode(bytes));
    if (typeof token !== "string" || typeof code !== "string") return null;
    return { token, code };
  } catch {
    return null;
  }
}

function readFragment() {
  const raw = location.hash.slice(1);
  if (!raw) return { email: "", key: "" };
  const params = new URLSearchParams(raw);
  const email = params.get("email") || "";
  const key = params.get("key") || "";
  if (key) return { email, key };
  const decoded = decodeURIComponent(raw);
  return validPickupKey(decoded) ? { email: "", key: decoded } : parseCredential(decoded, location.href);
}

function writeFragment(email, key) {
  const params = new URLSearchParams();
  if (email) params.set("email", email);
  params.set("key", key);
  history.replaceState(null, "", `${location.pathname}${location.search}#${params}`);
}

function openWorkspaceShell() {
  workspace.classList.remove("hidden");
  $("logout").classList.remove("hidden");
  $("email").textContent = pickupEmail || "正在确认邮箱…";
  $("private-link").value = location.href;
  $("remaining").textContent = "正在安全读取邮箱…";
}

function showAccess(message = "") {
  mailbox = null;
  selected = null;
  workspace.classList.add("hidden");
  $("logout").classList.add("hidden");
  $("email").textContent = "";
  $("mail-count").textContent = "0 封邮件";
  $("inbox-count").textContent = "0 封邮件";
  $("validity").textContent = "有效期 -";
  $("remaining").textContent = "输入上方取件码后加载邮箱";
  $("progress-bar").style.width = "0%";
  $("private-link").value = "";
  $("last-refresh").textContent = "尚未刷新";
  messageList.replaceChildren(node("div", "empty-list", "输入取件码后显示邮件"));
  $("message-detail").classList.add("hidden");
  $("empty-detail").classList.remove("hidden");
  status.classList.remove("success");
  status.textContent = message;
}

async function fetchMailbox(silent = false) {
  if (!validPickupKey(pickupKey)) {
    status.classList.remove("success");
    showAccess("取件链接缺少有效的访问密钥");
    return;
  }
  submit.disabled = true;
  if (!silent) { status.classList.remove("success"); status.textContent = "正在打开邮箱…"; }
  try {
    const legacy = decodeLegacyKey(pickupKey);
    const response = await fetch(pickupEndpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(legacy ? { email: pickupEmail, token: legacy.token, code: legacy.code } : { email: pickupEmail, key: pickupKey })
    });
    const payload = await response.json();
    if (!response.ok || !payload.success) throw new Error(payload.message || "读取失败");
    mailbox = payload.data;
    pickupEmail = mailbox.email;
    writeFragment(pickupEmail, pickupKey);
    renderWorkspace();
    openWorkspaceShell();
    status.classList.toggle("success", !mailbox.sync?.warning);
    status.textContent = mailbox.sync?.warning || "";
  } catch (error) {
    showAccess(error.message);
  } finally {
    submit.disabled = false;
  }
}

function renderWorkspace() {
  const messages = mailbox.messages || [];
  $("email").textContent = mailbox.email;
  $("mail-count").textContent = `${messages.length} 封邮件`;
  $("inbox-count").textContent = `${messages.length} 封邮件`;
  $("private-link").value = location.href;
  $("last-refresh").textContent = `最后同步 ${formatCompactDate(mailbox.sync?.synced_at || new Date())}`;
  $("link-time").textContent = ` · ${formatCompactDate(mailbox.expires_at)}`;
  renderCountdown();
  renderList(messages);
  if (selected) {
    const next = messages.find(message => message.id === selected.id);
    if (next) selectMessage(next);
  }
  if (!selected && messages.length) {
    const preferred = messages.find(message => /temporary chatgpt login code/i.test(message.subject || "")) || messages.find(message => message.otp_code) || messages[0];
    selectMessage(preferred);
  }
}

function renderCountdown() {
  if (!mailbox) return;
  const start = parseDate(mailbox.starts_at)?.getTime() ?? Date.now();
  const end = parseDate(mailbox.expires_at)?.getTime() ?? start;
  const now = Date.now();
  const total = Math.max(1, end - start);
  const left = Math.max(0, end - now);
  const percent = Math.max(0, Math.min(100, left / total * 100));
  $("progress-bar").style.width = `${percent}%`;
  $("validity").textContent = `有效期 ${formatDuration(left)}`;
  $("remaining").textContent = left > 0 ? `剩余 ${formatClock(left)}，到期 ${formatDate(mailbox.expires_at)}` : "邮箱访问已过期";
}

function formatClock(ms) {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  const hours = String(Math.floor(seconds / 3600)).padStart(2, "0");
  const minutes = String(Math.floor((seconds % 3600) / 60)).padStart(2, "0");
  const remainder = String(seconds % 60).padStart(2, "0");
  return `${hours}:${minutes}:${remainder}`;
}

function formatDuration(ms) {
  if (ms <= 0) return "已过期";
  const minutes = Math.ceil(ms / 60000);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.ceil(minutes / 60);
  if (hours < 24) return `${hours} 小时`;
  return `${Math.ceil(hours / 24)} 天`;
}

function renderList(messages) {
  messageList.replaceChildren();
  if (!messages.length) {
    messageList.append(node("div", "empty-list", "暂时没有新邮件，系统会自动刷新"));
    return;
  }
  for (const message of messages) {
    const item = node("div", "mail-item");
    item.setAttribute("role", "button");
    item.tabIndex = 0;
    if (selected && selected.id === message.id) item.classList.add("active");
    if (message.otp_code) {
      const otpRow = node("div", "item-otp-row");
      otpRow.append(node("span", "item-otp", `验证码：${message.otp_code}`));
      const copyButton = node("button", "otp-copy", "复制");
      copyButton.type = "button";
      copyButton.addEventListener("click", event => {
        event.stopPropagation();
        copyText(message.otp_code);
      });
      otpRow.append(copyButton);
      item.append(otpRow);
    }
    item.append(node("h3", "", message.subject || "无主题"));
    const meta = node("div", "meta");
    meta.append(
      node("span", "", formatSender(message.sender)),
      node("span", "recipient", `至 ${formatRecipient(message.recipient)}`),
      node("span", "", formatCompactDate(message.received_at))
    );
    item.append(meta);
    item.addEventListener("click", () => selectMessage(message));
    item.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectMessage(message);
      }
    });
    messageList.append(item);
  }
}

function emailCSP(allowRemoteImages) {
  return [
  "default-src 'none'",
  "script-src 'none'",
  "object-src 'none'",
  "frame-src 'none'",
  "connect-src 'none'",
  "form-action 'none'",
  "base-uri 'none'",
  allowRemoteImages ? "img-src https: http: data:" : "img-src data:",
  "style-src 'unsafe-inline'",
  "font-src data:",
  "media-src 'none'"
  ].join("; ");
}

function safeEmailURL(value, attribute) {
  const raw = String(value || "").trim();
  if (!raw || raw.startsWith("#") || raw.startsWith("/") || raw.startsWith("./") || raw.startsWith("../")) return true;
  try {
    const parsed = new URL(raw, "https://invalid.local/");
    if (parsed.protocol === "https:" || parsed.protocol === "http:") return true;
    if (parsed.protocol === "mailto:") return attribute === "href";
    if (parsed.protocol === "cid:") return ["src", "poster", "background"].includes(attribute);
    if (parsed.protocol === "data:") return attribute === "src" && /^data:image\/(?:png|jpe?g|gif|webp);/i.test(raw);
  } catch { return false; }
  return false;
}

function buildSafeEmailDocument(rawHTML, allowRemoteImages = false) {
  const documentNode = new DOMParser().parseFromString(String(rawHTML || ""), "text/html");
  documentNode.querySelectorAll("script,iframe,frame,frameset,object,embed,applet,base,input").forEach(element => element.remove());
  documentNode.querySelectorAll("form").forEach(form => {
    const replacement = documentNode.createElement("div");
    replacement.append(...form.childNodes);
    form.replaceWith(replacement);
  });
  documentNode.querySelectorAll("button,textarea,select,option").forEach(control => {
    const replacement = documentNode.createElement("span");
    replacement.append(...control.childNodes);
    control.replaceWith(replacement);
  });
  documentNode.querySelectorAll("meta[http-equiv]").forEach(meta => {
    if ((meta.getAttribute("http-equiv") || "").toLowerCase() === "refresh") meta.remove();
  });
  const dangerousCSS = /javascript\s*:|vbscript\s*:|data\s*:\s*text\/html|expression\s*\(|behavior\s*:|-moz-binding\s*:/i;
  documentNode.querySelectorAll("*").forEach(element => {
    for (const attribute of [...element.attributes]) {
      const name = attribute.name.toLowerCase();
      const value = attribute.value.trim();
      if (name.startsWith("on") || name === "srcdoc" || name === "target" || name === "ping" || name.startsWith("form") || name === "action") {
        element.removeAttribute(attribute.name);
      } else if (name === "style" && dangerousCSS.test(value)) {
        element.removeAttribute(attribute.name);
      } else if (["href", "src", "poster", "background", "xlink:href"].includes(name) && !safeEmailURL(value, name)) {
        element.removeAttribute(attribute.name);
      } else if (name === "srcset" && (dangerousCSS.test(value) || /data:/i.test(value) || /[\r\n]/.test(value))) {
        element.removeAttribute(attribute.name);
      }
    }
  });
  documentNode.querySelectorAll("a").forEach(link => {
    link.setAttribute("target", "_blank");
    link.setAttribute("rel", "noopener noreferrer");
  });
  documentNode.querySelectorAll("style").forEach(style => {
    style.textContent = style.textContent.replace(/javascript\s*:|vbscript\s*:|data\s*:\s*text\/html|expression\s*\(|behavior\s*:|-moz-binding\s*:/gi, "blocked:");
  });
  const charset = documentNode.createElement("meta");
  charset.setAttribute("charset", "utf-8");
  const csp = documentNode.createElement("meta");
  csp.setAttribute("http-equiv", "Content-Security-Policy");
  csp.setAttribute("content", emailCSP(allowRemoteImages));
  documentNode.head.prepend(csp);
  documentNode.head.prepend(charset);
  return `<!doctype html>${documentNode.documentElement.outerHTML}`;
}

function renderMessageBody(message) {
  const frame = $("detail-html");
  const fallback = $("detail-text");
  const remoteButton = $("load-remote-images");
  if (typeof message.body_html === "string" && message.body_html.trim()) {
    frame.setAttribute("sandbox", "");
    frame.referrerPolicy = "no-referrer";
    frame.srcdoc = buildSafeEmailDocument(message.body_html, remoteImagesEnabled);
    frame.classList.remove("hidden");
    fallback.classList.add("hidden");
    const hasRemoteImages = /(?:\bsrc(?:set)?|\bbackground)\s*=\s*["']?\s*https?:|url\(\s*["']?\s*https?:/i.test(message.body_html);
    remoteButton.classList.toggle("hidden", !hasRemoteImages);
    remoteButton.textContent = remoteImagesEnabled ? "阻止远程图片" : "加载远程图片";
    return;
  }
  frame.removeAttribute("srcdoc");
  frame.classList.add("hidden");
  fallback.classList.remove("hidden");
  remoteButton.classList.add("hidden");
}

function selectMessage(message) {
  selected = message;
	remoteImagesEnabled = false;
  $("empty-detail").classList.add("hidden");
  $("message-detail").classList.remove("hidden");
  $("detail-subject").textContent = message.subject || "无主题";
  $("detail-sender").textContent = formatSender(message.sender);
  $("detail-time").textContent = formatCompactDate(message.received_at);
  $("detail-recipient").textContent = `收件：${formatRecipient(message.recipient)}`;
  $("detail-body").textContent = message.body_text || "（邮件正文为空）";
	 renderMessageBody(message);
  const identity = `${message.sender || ""} ${message.subject || ""}`.toLowerCase();
  $("detail-brand").textContent = identity.includes("openai") || identity.includes("chatgpt") ? "OpenAI" : message.sender || "QuickMail";
  $("detail-lead").textContent = message.otp_code ? (identity.includes("openai") || identity.includes("chatgpt") ? "You can also enter this temporary code:" : "本邮件中识别到的验证码：") : "邮件正文：";
  const otp = $("detail-otp");
  if (message.otp_code) {
    otp.classList.remove("hidden");
    otp.querySelector("strong").textContent = message.otp_code;
  } else {
    otp.classList.add("hidden");
  }
  renderList(mailbox.messages || []);
}

function closeDetail() {
  selected = null;
	$("detail-html").removeAttribute("srcdoc");
  $("message-detail").classList.add("hidden");
  $("empty-detail").classList.remove("hidden");
  renderList(mailbox?.messages || []);
}

function submitCredential() {
  const parsed = parseCredential(credential.value, location.href);
  if (!validPickupKey(parsed.key)) {
    status.classList.remove("success");
    status.textContent = /[\r\n]/.test(credential.value) ? "请一次只粘贴一行“邮箱-----取件码”" : "请粘贴“邮箱-----取件码”或完整取件链接";
    return;
  }
  pickupEmail = parsed.email;
  pickupKey = parsed.key;
  writeFragment(pickupEmail, pickupKey);
  fetchMailbox(true);
}

submit.addEventListener("click", submitCredential);
credential.addEventListener("keydown", event => {
  if (event.key === "Enter") submitCredential();
});
credential.addEventListener("input", () => {
  const parsed = parseCredential(credential.value, location.href);
  const recognized = validPickupKey(parsed.key);
  credential.classList.toggle("credential-valid", recognized);
  status.classList.toggle("success", recognized);
  if (!credential.value.trim()) status.textContent = "";
  else if (recognized) status.textContent = parsed.email ? `已识别邮箱：${parsed.email}` : "已识别取件码";
  else status.textContent = /[\r\n]/.test(credential.value) ? "请一次只粘贴一行" : "格式应为：邮箱-----取件码";
});
$("refresh").addEventListener("click", () => fetchMailbox(true));
$("copy-email").addEventListener("click", () => copyText(mailbox?.email || pickupEmail));
$("copy-link").addEventListener("click", () => copyText(location.href));
$("open-link").addEventListener("click", () => window.open(location.href, "_blank", "noopener"));
$("copy-body").addEventListener("click", () => copyText(selected?.body_text || ""));
$("load-remote-images").addEventListener("click", () => {
  if (!selected) return;
  remoteImagesEnabled = !remoteImagesEnabled;
  renderMessageBody(selected);
});
$("close-detail").addEventListener("click", closeDetail);
$("logout").addEventListener("click", () => {
  pickupEmail = "";
  pickupKey = "";
  mailbox = null;
  selected = null;
  credential.value = "";
  history.replaceState(null, "", `${location.pathname}${location.search}`);
  showAccess();
});
document.querySelectorAll("[data-lang]").forEach(button => button.addEventListener("click", () => {
  document.querySelectorAll("[data-lang]").forEach(item => item.classList.toggle("active", item === button));
  showToast(button.dataset.lang === "zh" ? "已切换中文" : "English UI is coming soon");
}));

const initial = readFragment();
pickupEmail = initial.email;
pickupKey = initial.key;
if (validPickupKey(pickupKey)) {
  credential.value = pickupKey;
  fetchMailbox(true);
} else {
  showAccess(location.hash ? "取件链接格式不正确" : "");
}

refreshTimer = setInterval(() => {
  if (mailbox) fetchMailbox(true);
}, 20000);
countdownTimer = setInterval(renderCountdown, 1000);
