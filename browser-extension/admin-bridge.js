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
      if (payload.version !== 1) throw new Error("不支持的授权请求版本");
      // 存储交给扩展 background 完成：background 能拿到 Chrome 校验过的
      // 发送方 tab 地址（sender.url），以此核实“声明来源 = 实际页面来源”，
      // 防止任意本地页面伪造 begin 消息覆写待授权数据。
      const result = await chrome.runtime.sendMessage({
        type: "cymail_store_pending_auth",
        declared_origin: window.location.origin,
        payload: { ...payload, backend: normalizeBackend(payload.backend) }
      });
      if (!result || !result.ok) throw new Error(result?.error || "扩展拒绝了本次授权请求");
      respond(true);
    } catch (error) {
      respond(false, error.message || String(error));
    }
  });
})();
