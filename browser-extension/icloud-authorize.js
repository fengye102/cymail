(() => {
  const panelID = "cymail-icloud-authorization-panel";

  function validPendingAuthorization(value) {
    if (!value || !/^iba_[A-Za-z0-9_-]{32,100}$/.test(value.authorization_code || "")) return false;
    const expiresAt = new Date(value.expires_at).getTime();
    return Number.isFinite(expiresAt) && expiresAt > Date.now();
  }

  async function showAuthorizationPanel() {
    const saved = await chrome.storage.local.get("cymail_pending_auth");
    const pending = saved.cymail_pending_auth;
    if (!validPendingAuthorization(pending) || document.getElementById(panelID)) return;

    const backend = new URL(pending.backend);
    const host = document.createElement("div");
    host.id = panelID;
    host.style.cssText = "position:fixed;top:20px;right:20px;z-index:2147483647";
    const shadow = host.attachShadow({ mode: "closed" });
    shadow.innerHTML = `
      <style>
        *{box-sizing:border-box}section{width:340px;padding:18px;border:1px solid #c9d9f7;border-radius:16px;color:#102342;background:rgba(255,255,255,.98);box-shadow:0 18px 55px rgba(22,54,104,.25);font:13px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}header{display:flex;align-items:center;justify-content:space-between;gap:12px}strong{font-size:16px}button{border:0;border-radius:10px;padding:10px 13px;font:inherit;font-weight:800;cursor:pointer}.close{padding:3px 7px;color:#718096;background:transparent;font-size:18px}.confirm{width:100%;margin-top:12px;color:#fff;background:#315fcd}.confirm:disabled{opacity:.55;cursor:wait}p{margin:10px 0;color:#52657f}.target{padding:8px 10px;border-radius:9px;color:#31507c;background:#edf3ff;font:600 12px ui-monospace,monospace}.status{min-height:18px;margin:9px 0 0;color:#b24753;font-size:11px}.status.ok{color:#07865f}
      </style>
      <section role="dialog" aria-label="CYMail iCloud 授权">
        <header><strong>CYMail 授权请求</strong><button class="close" type="button" aria-label="关闭">×</button></header>
        <p>已检测到 CYMail 的一次性授权请求。确认后仅提交当前 iCloud 登录会话，不会读取密码或双重验证码。</p>
        <div class="target"></div>
        <button class="confirm" type="button">确认授权 CYMail</button>
        <div class="status" role="status"></div>
      </section>`;

    const close = shadow.querySelector(".close");
    const confirm = shadow.querySelector(".confirm");
    const status = shadow.querySelector(".status");
    shadow.querySelector(".target").textContent = `目标后台：${backend.host}`;
    close.addEventListener("click", () => host.remove());
    confirm.addEventListener("click", async () => {
      confirm.disabled = true;
      status.classList.remove("ok");
      status.textContent = "正在验证并提交 iCloud 会话…";
      try {
        const response = await chrome.runtime.sendMessage({ type: "cymail_complete_pending_auth" });
        if (!response?.ok) throw new Error(response?.error || "扩展未能完成授权");
        status.classList.add("ok");
        status.textContent = `授权成功，已安全提交 ${response.data.cookies_count} 个会话 Cookie。`;
        confirm.textContent = "授权成功";
        setTimeout(() => host.remove(), 3500);
      } catch (error) {
        status.textContent = error.message || String(error);
        confirm.disabled = false;
      }
    });

    document.documentElement.append(host);
  }

  showAuthorizationPanel();
  chrome.storage.onChanged.addListener((changes, area) => {
    if (area === "local" && changes.cymail_pending_auth?.newValue) showAuthorizationPanel();
  });
})();
