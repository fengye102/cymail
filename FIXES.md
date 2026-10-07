# FIXES — 审查问题修复记录

对应 `REVIEW.md` 中的 12 项发现。验证：`go build ./...`、`go vet ./...`、`gofmt -l`（空）、`go test ./...` 全部通过（8 包 ok）；扩展与前端 JS 通过 `node --check`。

## 高

**H1 首次设置管理员在反代拓扑下必然 403**
- 文件：`admin-server/internal/server/admin_auth.go`、`internal/server/server.go`
- 改法：弃用 `requestIP(c).IsLoopback()` 作为唯一判据。改为一次性引导令牌：服务启动且管理员未初始化时，`openAdminAuthStore` 后调用 `ensureBootstrapToken()` 生成 24 字节随机令牌并打印到启动日志；`setupAdmin` 改由 `bootstrapAuthorized()` 放行——请求头 `X-Bootstrap-Token` 恒时比对通过，或直连对端为 loopback 且受信（纯本机部署兼容）。管理员创建成功后令牌立即作废（`setup` 中清空，`bootstrapTokenAllowed` 对已初始化状态恒 false）。

**H2 登录限速退化为全局单桶（5 次失败锁定全体）**
- 文件：`admin-server/internal/server/admin_auth.go`
- 改法：限速键改为 `adminLoginClientKey(用户名, 真实IP)`（用户名小写 + `|` + IP）。真实 IP 由重写的 `requestIP()` 提供：仅当直连对端命中受信代理列表（环境变量 `TRUSTED_PROXIES`，默认 `127.0.0.0/8, ::1/128, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16`，覆盖 Docker/Compose 拓扑）才解析 `X-Forwarded-For`（从右向左取第一个非受信地址），否则一律用直连对端——伪造头不再影响限速与判定。

## 中

**M1 字符串拼接构造 JSON 请求体**
- 文件：`admin-server/internal/hme/auth.go`（`authFederate`、`authenticateWeb`）
- 改法：两处手写 JSON 拼接改为匿名结构体 + `json.Marshal`，消除含 `"`/`\` 值的报文注入与畸形请求。

**M2 `isSessionError` 按自由文本子串判断**
- 文件：`admin-server/internal/hme/client.go`、`internal/server/server.go`
- 改法：新增结构化错误 `hme.HTTPStatusError{StatusCode, Message}`（`safeAppleHTTPError` 返回该类型，错误文本格式不变）；`isSessionError` 改为只接受 `error`，依据 `errors.Is(err, hme.ErrTrustSessionRequired)` 或 `HTTPStatusError.StatusCode ∈ {401,403}` 判断。5 处调用点全部从 `err.Error()` 改传 `err`。不再匹配子串。

**M3 单连接 + 全局互斥锁串行化数据库访问**
- 文件：`admin-server/internal/store/postgres.go`
- 改法：`*pgx.Conn` + `sync.Mutex` 改为 `pgxpool.Pool`（`OpenPostgres`/`Close` 相应调整），删除全部方法内的 `p.mu.Lock()`；`AllocateMailbox` 拆出 `allocateMailboxOnce`，外层对 SQLSTATE 40001（序列化失败）做最多 3 次指数退避重试（100/200/400ms，尊重 ctx 取消）。方法签名全部未变。

**M4 前端固定单页拉取、超限静默丢数据**
- 文件：`admin-server/web/admin/app.js`
- 改法：新增 `fetchListCapped()`：按后端上限拉取（mailboxes/orders 1000、messages 500），返回条数达到上限时在状态栏显式告警「仅显示前 N 条」，不再静默截断。（后端列表接口不支持 offset 分页，故采用报告中的显式提示方案而非分页循环。）

**M5 扩展内容脚本未覆盖生产管理域名、失效报错含糊**
- 文件：`browser-extension/README.md`、`admin-server/web/admin/app.js`
- 改法：README 顶部新增「生产部署必做：配置管理域名」章节，明确不执行 `configure-domain.ps1` 授权链路会静默失效及补救命令；管理页握手超时提示语同步指明该原因。

## 低

**L1 LIKE 通配符未转义**
- 文件：`admin-server/internal/store/postgres.go`
- 改法：新增 `escapeLikePattern()`（转义 `\` `%` `_`），查询词按字面量匹配，SQL 加 `ESCAPE '\'`。

**L2 `secureRequest` 无条件采信 `X-Forwarded-Proto`**
- 文件：`admin-server/internal/server/admin_auth.go`
- 改法：`secureRequest` 改为 Server 方法：直连 TLS、或 `PUBLIC_BASE_URL` 为 https（强制 Secure）、或直连对端受信且转发头为 https 三者之一才返回 true。

**L3 扩展授权模式丢弃端口**
- 文件：`browser-extension/background.js`、`popup.js`
- 改法：存储 pending 授权时记录 `backend_port`（background 在提交前核实端口与申请时一致，不一致则清除待授权并要求重新发起）；popup 注释更新说明按主机授权、端口由提交时核实。

**L4 内容脚本注入任意本地页面可覆写待授权数据**
- 文件：`browser-extension/admin-bridge.js`、`background.js`
- 改法：pending 授权的校验与写入整体移入 background（`cymail_store_pending_auth`）：仅接受内容脚本消息（`sender.tab`/`sender.origin` 由 Chrome 填写、页面不可伪造）、要求顶层框架、声明来源必须等于实际 tab 来源、backend 必须与页面同源。伪造页面的 postMessage 无法再写入待授权数据。

**L5 取件限速按提交密钥分桶、对枚举无防护**
- 文件：`admin-server/internal/server/fulfillment.go`
- 改法：在原密钥哈希桶之外叠加 `ip:<真实IP>` 第二层桶（复用 H1 修好的 `requestIP`），allowed/failure 双桶联动，单来源爆破触发整体限速。

## 验证输出摘要

```
$ gofmt -l .            # （空输出）
$ go build ./...        # 通过
$ go vet ./...          # 通过
$ go test ./...
ok  icloud-hme
ok  icloud-hme/internal/account
ok  icloud-hme/internal/fulfillment
ok  icloud-hme/internal/hme
ok  icloud-hme/internal/mail
ok  icloud-hme/internal/server
?   icloud-hme/internal/srp  [no test files]
ok  icloud-hme/internal/store

$ node --check browser-extension/*.js admin-server/web/admin/app.js   # 全部通过
```
