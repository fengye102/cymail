(() => {
  const channel = "cymail-browser-auth-v1";

  function normalizeBackend(value) {
    const url = new URL(String(value || "").trim());
    if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("后台地址协议无效");
    if (url.origin !== window.location.origin) throw new Error("后台地址与当前管理页面不一致");
    if (!url.pathname.endsWith("/")) url.pathname += "/";
    url.search = "";
    url.hash = "";
    return url.href;
  }

  window.addEventListener("message", async event => {
    const message = event.data;
    if (event.source !== window || event.origin !== window.location.origin) return;
    if (!message || message.channel !== channel || message.type !== "begin") return;

    const respond = (ok, error = "") => window.postMessage({
      channel,
      type: "result",
      request_id: message.request_id,
      ok,
      error
    }, window.location.origin);

    try {
      const payload = message.payload || {};
      const authorizationCode = String(payload.authorization_code || "").trim();
      const expiresAt = new Date(payload.expires_at).getTime();
      if (payload.version !== 1) throw new Error("不支持的授权请求版本");
      const kind = payload.kind === "forwarding" ? "forwarding" : "icloud";
      const pattern = kind === "forwarding" ? /^fwa_[A-Za-z0-9_-]{32,100}$/ : /^iba_[A-Za-z0-9_-]{32,100}$/;
      if (!pattern.test(authorizationCode)) throw new Error("一次性授权凭证无效");
      if (!Number.isFinite(expiresAt) || expiresAt <= Date.now()) throw new Error("一次性授权请求已过期");

      if (kind === "forwarding") {
        if (payload.provider !== "netease_163" || !/@163\.com$/i.test(String(payload.email || ""))) {
          throw new Error("163 网页授权目标无效");
        }
      }

      await chrome.storage.local.set({
        cymail_pending_auth: {
          backend: normalizeBackend(payload.backend),
          authorization_code: authorizationCode,
          expires_at: new Date(expiresAt).toISOString(),
          kind,
          email: kind === "forwarding" ? String(payload.email).toLowerCase() : "",
          provider: kind === "forwarding" ? payload.provider : ""
        }
      });
      respond(true);
    } catch (error) {
      respond(false, error.message || String(error));
    }
  });
})();
