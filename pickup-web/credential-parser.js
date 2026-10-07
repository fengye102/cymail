(function exposePickupCredentialParser(root) {
  function validPickupKey(value) {
    return /^tok_[A-Za-z0-9_-]{28,156}$/.test(String(value || "").trim());
  }

  function validEmail(value) {
    return /^[^\s@]+@[^\s@]+$/.test(String(value || "").trim());
  }

  function parsePickupURL(value, baseURL) {
    try {
      const parsed = new URL(value, baseURL || "http://127.0.0.1/pickup");
      const fragment = new URLSearchParams(parsed.hash.slice(1));
      return { email: fragment.get("email") || "", key: fragment.get("key") || "" };
    } catch {
      return { email: "", key: "" };
    }
  }

  function parseCredential(value, baseURL) {
    const input = String(value || "").trim();
    if (!input || /[\r\n]/.test(input)) return { email: "", key: "" };

    const pair = input.match(/^(.+?)-----\s*(tok_[A-Za-z0-9_-]{28,156})$/);
    if (pair && validEmail(pair[1])) return { email: pair[1].trim(), key: pair[2].trim() };

    const legacy = input.match(/^(.+?)---\s*(tok_[A-Za-z0-9_-]{28,156})(?:---(.+))?$/);
    if (legacy && validEmail(legacy[1])) return { email: legacy[1].trim(), key: legacy[2].trim() };

    if (/^https?:\/\//i.test(input)) return parsePickupURL(input, baseURL);
    if (validPickupKey(input)) return { email: "", key: input };
    return { email: "", key: "" };
  }

  root.CYMailPickupCredential = { validPickupKey, parsePickupURL, parseCredential };
})(globalThis);
