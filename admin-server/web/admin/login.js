const $ = id => document.getElementById(id);
let setupMode = false;

function safeMessage(message) {
  const value = String(message || "请求失败");
  return /HTTP\s*421|trustTokens|[A-Za-z0-9+/_=-]{120,}/i.test(value) ? "服务认证状态已更新，请刷新页面后重试。" : value;
}

function setStatus(message = "", success = false) {
  $("auth-status").textContent = safeMessage(message);
  $("auth-status").classList.toggle("success", success);
}

function setBusy(busy) {
  const button = $("submit-auth");
  button.disabled = busy;
  button.classList.toggle("is-loading", busy);
  button.setAttribute("aria-busy", String(busy));
  button.querySelector("span").textContent = busy ? (setupMode ? "正在保护账号" : "正在验证") : (setupMode ? "完成设置并进入总控台" : "登录总控台");
}

function configureMode(initialized) {
  setupMode = !initialized;
  $("mode-badge").textContent = setupMode ? "首次安全设置" : "管理员登录";
  $("auth-title").textContent = setupMode ? "创建管理员账号" : "欢迎回来";
  $("auth-subtitle").textContent = setupMode ? "设置你自己的用户名和密码，今后访问总控台都需要登录。" : "输入管理员用户名和密码进入 CYMail 总控台。";
  $("setup-fields").classList.toggle("hidden", !setupMode);
  $("confirm-password").required = setupMode;
  $("password").autocomplete = setupMode ? "new-password" : "current-password";
  $("password").placeholder = setupMode ? "至少 12 个字符" : "输入登录密码";
  $("submit-auth").querySelector("span").textContent = setupMode ? "完成设置并进入总控台" : "登录总控台";
  $("submit-auth").disabled = false;
  if (setupMode) $("username").value = "admin";
  $("username").focus();
}

function updateStrength() {
  const password = $("password").value;
  let score = 0;
  if (password.length >= 12) score++;
  if (password.length >= 16) score++;
  if (/[a-z]/.test(password) && /[A-Z]/.test(password)) score++;
  if (/\d/.test(password) && /[^A-Za-z0-9]/.test(password)) score++;
  const widths = [0, 28, 52, 76, 100];
  const labels = ["至少 12 个字符", "强度较弱", "强度一般", "强度良好", "强度很强"];
  const colors = ["#c64755", "#d46a43", "#c59223", "#2e9b76", "#087f64"];
  $("strength-bar").style.width = `${widths[score]}%`;
  $("strength-bar").style.background = colors[score];
  $("strength-text").textContent = labels[score];
}

async function request(path, payload) {
  const response = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload)
  });
  let body;
  try { body = await response.json(); } catch { throw new Error("服务器返回了无效响应"); }
  if (!response.ok || !body.success) throw new Error(body.message || "请求失败");
  return body.data;
}

async function initialize() {
  try {
    const response = await fetch("/api/auth/status", { credentials: "same-origin", cache: "no-store" });
    const body = await response.json();
    if (!response.ok || !body.success) throw new Error(body.message || "无法读取登录状态");
    if (body.data.authenticated) {
      window.location.replace("/");
      return;
    }
    configureMode(Boolean(body.data.initialized));
  } catch (error) {
    $("mode-badge").textContent = "连接失败";
    $("auth-title").textContent = "暂时无法登录";
    $("auth-subtitle").textContent = "请确认 CYMail 后端服务正在运行。";
    setStatus(error.message);
  }
}

document.querySelectorAll("[data-reveal]").forEach(button => button.addEventListener("click", () => {
  const input = $(button.dataset.reveal);
  const showing = input.type === "text";
  input.type = showing ? "password" : "text";
  button.textContent = showing ? "显示" : "隐藏";
  button.setAttribute("aria-label", showing ? "显示密码" : "隐藏密码");
}));

$("password").addEventListener("input", updateStrength);
$("auth-form").addEventListener("submit", async event => {
  event.preventDefault();
  setStatus();
  const username = $("username").value.trim();
  const password = $("password").value;
  if (username.length < 2) return setStatus("用户名至少需要 2 个字符");
  if (setupMode && password.length < 12) return setStatus("密码至少需要 12 个字符");
  if (setupMode && password !== $("confirm-password").value) return setStatus("两次输入的密码不一致");
  setBusy(true);
  try {
    await request(setupMode ? "/api/auth/setup" : "/api/auth/login", { username, password });
    setStatus(setupMode ? "管理员账号已创建，正在进入总控台。" : "登录成功，正在进入总控台。", true);
    window.location.replace("/");
  } catch (error) {
    setStatus(error.message);
    setBusy(false);
  }
});

initialize();
