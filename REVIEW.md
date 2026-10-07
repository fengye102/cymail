# CYMail 代码审查报告

## 一、项目概览

CYMail 是面向批量账号卖家的 iCloud 隐藏邮箱（HME）多账号管理与「取件发货」系统：Go/Gin 后端（Postgres 库存 + 定时采集），管理后台与取件页为原生 JS 静态站，另有一个 Chrome MV3 扩展，负责把用户显式授权的 iCloud / 163 网页会话 Cookie 交给后端。部署采用 Caddy 自动 HTTPS + 双域名（管理域 / 取件域）+ nginx 反代到容器内 8081 的拓扑；敏感凭据以 AES-256-GCM 加密落盘，管理员口令用 Argon2id，会话与取件密钥只存 SHA-256。安全基线设计明显高于同类小项目，但存在一处会直接导致首次部署无法完成的信任边界缺陷。

## 二、发现清单

### 高

**H1 `admin-server/internal/server/admin_auth.go:391-409`（配合 `:325-329`）— 首次设置管理员在生产拓扑下必然 403，首次部署无法完成**
`requestIP` 只在直连对端为 loopback 时才采信 `X-Forwarded-For` / `X-Real-IP`。生产链路是 client→Caddy→admin-web(nginx)→api:8081，api 看到的直连对端是 nginx 容器 IP（非 loopback），因此 `setupAdmin` 的 `requestIP(c).IsLoopback()` 恒为 false，返回 403「首次设置管理员只能在服务器本机完成」。README 指引首次在 `/login.html` 创建管理员，而 api 端口未对外发布（compose 仅 `expose`），按文档操作必然失败。
修复：改用一次性引导令牌（首次启动写入容器日志或环境变量），或显式配置受信代理后按 `X-Forwarded-For` 解析真实 IP；不要用 loopback 判断代替信任边界配置。

**H2 `admin-server/internal/server/admin_auth.go:354`（配合 `:204-217`、`:241-251`）— 登录限速退化为全局单桶，可被 5 次失败拒绝服务全体管理员**
`loginAdmin` 以 `requestIP(c).String()` 作为失败计数键。同 H1 的根因，容器拓扑下所有请求的真实来源都被解析成同一个 nginx 容器 IP，`attempts` 退化为单键：任何能访问 `/api/auth/login` 的人失败 5 次，即把全体管理员锁定 15 分钟；同时无法按来源区分攻击者，限速形同虚设。
修复：与 H1 同源修复真实 IP 解析；限速键改为「用户名 + 真实 IP」组合，并对触发锁定加告警。

### 中

**M1 `admin-server/internal/hme/auth.go:154`、`:333` — 字符串拼接构造 JSON 请求体**
`data := ` + `` `{"accountName":"` `` + `state.username + ...`，以及 `body := fmt.Sprintf(` + `` `{"dsWebAuthToken":"%s",...}` `` + `, ...)`，均未做 JSON 转义。含 `"` 或 `\` 的值会破坏报文结构，属请求体注入/畸形请求模式。
修复：统一用 `encoding/json.Marshal` 构造。

**M2 `admin-server/internal/server/server.go:899-905` — `isSessionError` 以自由文本子串判断会话失效**
只要错误文本含 `401` / `403` / `session` / `cookie` / `认证` 就判为会话失效。上游超时、限流等无关错误若文本恰好含这些片段，会被误判为「需重新授权」，掩盖真实故障并诱导用户做无效重授权。
修复：改为依据明确的错误类型或状态码判断，禁止对自由文本做子串匹配。

**M3 `admin-server/internal/store/postgres.go:21-31`、`:250-300` — 单连接 + 全局互斥锁串行化全部数据库访问**
`Postgres` 只持有一个 `*pgx.Conn` 并用 `sync.Mutex` 串行化所有查询；`AllocateMailbox` 在整个 Serializable 事务期间持锁，`FOR UPDATE SKIP LOCKED` 因此失去并发意义。同时未对序列化失败（SQLSTATE 40001）做重试，多副本或高并发下会直接向上返回 500。
修复：改用 `pgxpool`；对 40001 做有限重试；事务期间不要持有进程级互斥锁。

**M4 `admin-server/web/admin/app.js:895-897` — 前端静默截断库存数据**
`loadAll` 固定请求 `mailboxes?limit=1000`、`orders?limit=500`、`messages?limit=500`，后端对 limit 还有上限截断（`postgres.go:223-225`、`:305-307`、`:396-398`）。批量卖家的邮箱数常超 1000，此时「全部邮箱」列表、计数与筛选会静默丢数据，用户无从察觉。
修复：改为分页/增量加载，或在超出时显式提示「仅显示前 N 条」。

**M5 `browser-extension/manifest.json:25-33`（配合 `browser-extension/README.md:31`）— 扩展未覆盖生产管理域名，授权链路会静默失效**
`admin-bridge.js` 内容脚本仅匹配 `http://127.0.0.1/*`、`http://localhost/*`。生产管理域名需另跑 `configure-domain.ps1` 改写 manifest；一旦漏做，管理页与扩展的握手会在 2 秒后超时（`app.js:1300-1303`），只报出含义不清的「未检测到新版 CYMail 扩展」，而 iCloud / 163 授权整体不可用。
修复：把域名配置纳入 deploy 流程并在缺失时显式报错，或改用 `externally_connectable` 而非按站点注入。

### 低

**L1 `admin-server/internal/store/postgres.go:410` — LIKE 通配符未转义**
查询词被直接拼成 `'%' + lower(query) + '%'`，`%` / `_` 未转义；传入 `%` 可匹配全部邮件（结果膨胀、变慢）。当前仅管理员与持密钥机器调用，影响有限。
修复：转义 `%`、`_`、`\`，或改用 `position()` / 全文检索。

**L2 `admin-server/internal/server/admin_auth.go:384-389` — `secureRequest` 无条件采信 `X-Forwarded-Proto`**
若 api:8081 被误暴露（当前 compose 未发布），会话 Cookie 的 `Secure` 属性将由客户端请求头决定。
修复：仅在受信代理来源下采信该头；或当 `PUBLIC_BASE_URL` 为 https 时强制置 Secure。

**L3 `browser-extension/popup.js:46-52`、`background.js:98-102` — 授权模式丢弃端口**
申请的 origin 模式为 `${protocol}//${hostname}/*`，等于对同一主机的所有端口授权（如 `http://127.0.0.1/*` 覆盖全部本地端口）。
修复：Chrome 匹配模式本就不含端口，可在校验时额外确认端口，或对本地开发固定使用 `127.0.0.1` 并在文档中说明该限制。

**L4 `browser-extension/admin-bridge.js:14-17` — 内容脚本注入到任意本地页面**
任何运行在 `127.0.0.1` / `localhost` 的页面都能向扩展 postMessage，覆写 `chrome.storage.local` 中的 `cymail_pending_auth`（进一步利用仍需有效一次性码，实际危害低）。
修复：收窄匹配范围（限定端口/路径），或要求消息携带扩展侧校验的 nonce。

**L5 `admin-server/internal/server/fulfillment.go:395-404` — 取件限速按「提交的密钥」分桶，对枚举无防护**
失败计数以被猜密钥的哈希为键，每次猜测都得到全新桶，故对密钥枚举毫无减速作用；仅因 `tok_` 密钥为 48 字节随机数才不可枚举。
修复：叠加按来源 IP 维度的限速，使该控制真正生效。

## 三、部署前必须处理的 Top 3

1. **H1**：修正 `admin_auth.go:391-409` 的信任边界，否则首次部署无法创建管理员（功能阻断）。
2. **H2**：修正 `admin_auth.go:354` 的限速键，否则任何人 5 次失败即可锁定全体管理员登录（拒绝服务）。
3. **M5**：确认生产管理域名已写入 `manifest.json` 的 `host_permissions` 与内容脚本 `matches`，否则扩展授权链路静默失效（核心功能阻断）。

## 四、总体结论

代码整体可信度较高，属于「工程质量良好、个别关键路径存在硬伤」的水平。值得肯定的控制包括：凭据 AES-256-GCM 加密、口令 Argon2id、会话与取件密钥仅存 SHA-256、比较用 `subtle.ConstantTimeCompare`；公开端点由高熵一次性码把关，`/public/` 下未匹配路径在 nginx 层直接 404；邮件 HTML 在服务端（`netease_web_client.go:415`）与客户端（`pickup-web/app.js:314`）双重净化，并渲染在 `sandbox=""` 的 iframe 中，前端 CSP 统一为 `default-src 'none'`；调试日志对 Cookie / scnt / apiKey 做脱敏；备份文件 0600、目录 0700 且用临时文件 + rename 落盘。

风险集中在「按来源 IP 做安全决策」这一条主线：`requestIP` 的信任边界与实际容器拓扑不符，同时打穿了首部署初始化（H1）和登录防爆破（H2），修复成本低但必须先做。其次是若干健壮性问题（M1 报文拼接、M2 文本匹配、M3 单连接串行、M4 前端截断）与两处容器/扩展的部署前置条件（M5、L2）。上述问题修复后，本项目可以进入生产使用；在修复 H1、H2 之前不建议上线。
