# iCloud Hide My Email API 文档

## 概述

HTTP JSON API，所有接口返回统一格式：

管理网页通过 `HttpOnly` 登录会话访问 `/api`。首次使用时在本机管理页创建管理员账号；内部自动化也可以使用可选的 `Authorization: Bearer <CYMAIL_INTERNAL_API_KEY>` 或 `X-API-Key` 请求头。未通过鉴权返回 `401 Unauthorized`。

```json
{
  "success": true,
  "data": {},
  "message": ""
}
```

**错误响应:**
- `400 Bad Request` — 参数错误
- `401 Unauthorized` — 会话失效
- `404 Not Found` — 账号不存在
- `409 Conflict` — Apple 明确返回隐藏邮箱总容量已满
- `429 Too Many Requests` — Apple 临时限速或尚未达到管理员设置的创建间隔
- `502 Bad Gateway` — iCloud 服务错误

---

## 核心接口

### 1. 创建 HME 别名

```http
POST /api/create
Content-Type: application/json

{
  "account_id": "acc_1",
  "label": "注册某网站",
  "interval_seconds": 3
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "label": "注册某网站",
    "created_at": "2024-01-15T10:30:00Z",
    "account_id": "acc_1"
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID
- `label` (可选) — 别名标签，默认为 "Created YYYY-MM-DD HH:mm"
- `interval_seconds` (可选) — 当前账号两次成功创建之间的最小间隔，允许 `0–86400` 秒；`0` 表示不使用 CYMail 本地间隔限制

`created_at` 与别名列表中的 `createdAt` 均规范化为 UTC RFC3339（精确到秒）；管理页统一显示为本机时区的 `YYYY-MM-DD HH:mm:ss`。Apple 返回的秒、毫秒、微秒、纳秒时间戳和嵌套时间字段均可解析。

**错误情况:**
- `401` — Cookie 过期，需更新
- `409` — Apple 明确返回总地址容量已满
- `429` — 临时创建限速；根据 `Retry-After` 或 `retry_after_seconds` 稍后重试
- `502` — 其他 iCloud 服务错误

---

### 2. 读取邮件

```http
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&limit=20&days=7
```

**响应 (走 IMAP,App Password):**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "count": 2,
    "method": "imap",
    "messages": [
      {
        "id": "1042",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "2026-07-09T14:32:10+08:00",
        "preview": "Almost done! To finish setting up your account, we just need to verify.."
      }
    ]
  }
}
```

**响应 (回退到 Web API,Cookie):** `method` 变为 `web_api`
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "count": 1,
    "method": "web_api",
    "messages": [
      {
        "id": "AQMkAD...",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "Wed, 09 Jul 2026 06:32:10 GMT",
        "preview": "Almost done! To finish setting up your account.."
      }
    ]
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID
- `alias` (可选) — 只返回发到该别名的邮件;不传返回收件箱最近邮件
- `limit` (可选) — 返回邮件数量，默认 20
- `days` (可选) — 查找最近几天的邮件，默认 7 (仅 IMAP 模式)

**邮件读取双路径 (自动选择):**
1. **优先: IMAP (App Password)** — 设置了 App Password 时使用,支持服务端按收件人搜索
2. **回退: Web API (Cookie 认证)** — 无 App Password 或 IMAP 失败时,通过 iCloud mccgateway 端点读取

响应中 `"method": "imap"` 或 `"method": "web_api"` 标识实际使用的读取方式。

**别名过滤逻辑:**
- **IMAP (`FindByRecipient`):** 先用原生 IMAP `TO` 头搜索 (配合 `days` 时间范围);无结果时拉取最近 `limit*3` 条本地按 `To` 兜底过滤
- **Web API (`FindByAlias`):** iCloud Web API 不支持按收件人搜索,拉取 `limit*2` (至少 50) 条后本地对 `Subject`/`From`/`To` 做包含匹配

**返回字段差异 (两条路径):**
- `id` — IMAP 是 UID 数字串,Web API 是 iCloud GUID
- `date` — IMAP 走 RFC3339,Web API 是原始邮件头 RFC1123 串
- `preview` — 正文摘要,非完整正文

---

## 账号管理接口

### 3. 列出所有账号

```http
GET /api/accounts
```

**响应:**
```json
{
  "success": true,
  "data": [
    {
      "id": "acc_1",
      "name": "主号",
      "host": "imap.mail.me.com"
    }
  ]
}
```

**注意:** 响应中不包含敏感信息（cookies、app_passwords）

---

### 4. 添加账号

**简化版（cookies 可选）:**
```http
POST /api/accounts
Content-Type: application/json

{
  "name": "新账号",
  "host": "icloud.com",
  "proxy": "http://user:pass@host:port"
}
```

**完整版（包含 Cookie）:**
```http
POST /api/accounts
Content-Type: application/json

{
  "name": "新账号",
  "cookies": "{\"x-apple-session-token\":\"token_value\"}",
  "host": "icloud.com",
  "proxy": "http://user:pass@host:port"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_3",
    "name": "新账号",
    "host": "icloud.com",
    "status": "pending"
  }
}
```

**参数说明:**
- `name` (必填) — 账号名称
- `cookies` (可选) — Cookie 字符串,支持两种格式:
  - JSON: `"{\"name\":\"value\"}"`
  - Header: `"name1=value1; name2=value2"`
- `host` (可选) — iCloud 域名,默认 `icloud.com`
- `proxy` (可选) — HTTP/SOCKS5 代理

**注意:** 不传 cookies 时,账号状态为 `pending`,需通过 `/login` 接口登录获取 Cookie

---

### 5. 账号密码登录（服务端保存 Cookie）

```http
POST /api/accounts/:id/login
Content-Type: application/json

{
  "password": "用户的常规iCloud密码",
  "otp_code": "123456"  // 可选,2FA 验证码
}
```

**参数说明:**
- `:id` (路径参数) — 账号 ID
- `password` (必填) — iCloud 账号的常规密码(**不是** App Password)
- `otp_code` (可选) — 双重认证验证码

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_1",
    "cookies_count": 12
  }
}
```

**注意事项:**
- 密码是登录 appleid.apple.com 的**常规账号密码**,不是 App 专用密码
- 登录前账号必须已设置 `icloud_email` 字段
- 登录成功后 Cookie 会加密保存到 accounts.json，不会通过 API 返回
- 启用 2FA 时,第一次请求会被拒绝,需要带 `otp_code` 重试

---

### 6. 删除账号

```http
DELETE /api/accounts/:id
```


**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_3"
  }
}
```

**错误情况:**
- `404` — 账号不存在

---

### 7. 设置 App Password

```http
POST /api/accounts/:id/password
Content-Type: application/json

{
  "icloud_email": "your_email@icloud.com",
  "app_password": "xxxx-xxxx-xxxx-xxxx"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "id": "acc_1",
    "icloud_email": "your_email@icloud.com"
  }
}
```

**参数说明:**
- `icloud_email` (必填) — iCloud 邮箱地址
- `app_password` (必填) — App 专用密码

**用途:** App Password 用于 IMAP 邮件读取，生成方式见 [appleid.apple.com](https://appleid.apple.com)

---

## 别名管理接口

### 8. 列出所有别名

```http
GET /api/aliases?account_id=acc_1
```

**响应:**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 15,
    "aliases": [
      {
        "email": "xyz123@icloud.com",
        "anonymousId": "abc123",
        "label": "注册某网站",
        "active": true,
        "createdAt": "2024-01-15T10:30:00Z"
      }
    ]
  }
}
```

**参数说明:**
- `account_id` (必填) — 账号 ID

**别名字段:**
- `email` — HME 邮箱地址
- `anonymousId` — 别名唯一标识（用于停用/激活/删除）
- `label` — 用户定义的标签
- `active` — 是否激活
- `createdAt` — 创建时间

---

### 9. 停用别名

```http
POST /api/aliases/:id/deactivate
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**说明:** 停用后别名不再接收邮件，但可随时激活恢复

---

### 10. 激活别名

```http
POST /api/aliases/:id/reactivate
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**说明:** 激活已停用的别名，恢复邮件接收

---

### 11. 删除别名

```http
DELETE /api/aliases/:id
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

**响应:**
```json
{
  "success": true,
  "data": {
    "anonymous_id": "abc123"
  }
}
```

**参数说明:**
- `:id` (路径参数) — 别名的 `anonymousId`
- `account_id` (必填) — 账号 ID

**注意:** 删除不可恢复！如果直接删除失败，会先停用再删除

---

## 使用示例

### curl 示例

```bash
# 创建别名
curl -X POST http://localhost:8081/api/create \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1", "label": "GitHub"}'

# 读取邮件
curl "http://localhost:8081/api/inbox?account_id=acc_1&alias=xyz123@icloud.com&limit=10"

# 列出别名
curl "http://localhost:8081/api/aliases?account_id=acc_1"

# 停用别名
curl -X POST http://localhost:8081/api/aliases/abc123/deactivate \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1"}'

# 删除别名
curl -X DELETE http://localhost:8081/api/aliases/abc123 \
  -H "Content-Type: application/json" \
  -d '{"account_id": "acc_1"}'
```

### Python 示例

```python
import requests

BASE_URL = "http://localhost:8081/api"

# 创建别名
resp = requests.post(f"{BASE_URL}/create", json={
    "account_id": "acc_1",
    "label": "Netflix"
})
print(resp.json())

# 读取邮件
resp = requests.get(f"{BASE_URL}/inbox", params={
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "limit": 10
})
print(resp.json())

# 列出别名
resp = requests.get(f"{BASE_URL}/aliases", params={"account_id": "acc_1"})
for alias in resp.json()["data"]["aliases"]:
    print(f"{alias['email']} - {alias['label']} (active: {alias['active']})")
```

---

## 认证说明

### Cookie 认证 (推荐,功能最完整)

用于：创建别名、列出别名、停用/激活/删除别名、**读取邮件**

**获取方式:**
1. 浏览器登录 [icloud.com](https://www.icloud.com) 或 [icloud.com.cn](https://www.icloud.com.cn) (国区)
2. F12 → Application → Cookies
3. 导出全部 Cookie 为 `{"key":"value"}` 格式 JSON

**关键 Cookie:**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

**有效期:** 约 24 小时

### App Password 认证 (IMAP 回退)

仅用于 Web API 失败时的邮件读取回退

**获取方式:**
1. 登录 [appleid.apple.com](https://appleid.apple.com)
2. 登录和安全 → App 专用密码
3. 生成新密码

---

## 技术说明

### 邮件读取实现

**Web API 路径** (`internal/mail/web_client.go`):
1. 调用 `setup.icloud.com.cn/setup/ws/1/validate` 获取 `mccgateway` URL
2. 调用 `mccgateway/mailws2/v1/thread/search` 读取邮件

**⚠️ 已知坑:**
- `validate` 返回的 mccgateway URL 可能带 `:443` 端口 (如 `p217-mccgateway.icloud.com.cn:443`)
- tls-client 的 cookie jar 按不带端口的 host 存储 cookie
- 带端口请求时 cookie 无法附加,导致 403
- **解决:** 解析 URL 后剥离端口号

**clientBuildNumber:** 与浏览器一致,当前 `2624Build22`

**IMAP 路径** (`internal/mail/client.go`):
- 标准 IMAP 协议,连接 `imap.mail.me.com:993`
- 需要 App Password

---

## 错误处理

### 会话失效 (401)

```json
{
  "success": false,
  "message": "iCloud 会话失效，请更新 Cookie: HTTP 401"
}
```

**解决:** 更新 `accounts.json` 中的 Cookie

### 临时创建限速 (429)

```json
{
  "success": false,
  "message": "Apple 暂时限制了创建频率，尚未达到总容量；请在建议时间后重试",
  "code": "alias_rate_limited",
  "retry_after_seconds": 1800
}
```

响应同时带有标准 `Retry-After` 请求头。未达到自定义创建间隔或 Apple 主动限速都会返回这一结构；管理页的批量任务会显示时/分/秒倒计时并自动续跑，单次 API 调用方应在倒计时结束后重试。

### 总地址容量已满 (409)

```json
{
  "success": false,
  "message": "Apple 明确拒绝创建：当前账号的隐藏邮件地址总容量已满",
  "code": "alias_capacity_reached"
}
```

只有 Apple 明确返回“总地址数量/容量”语义时才会使用该错误；普通 `limit`、`too many requests` 不再误报为总容量。

### 参数错误 (400)

```json
{
  "success": false,
  "message": "参数错误: account_id 必填"
}
```

---

## 限制

- **创建频率**: Apple 未公开固定额度；管理端默认每 `144` 秒创建一个（约 `25` 个/小时），管理员仍可设置 `0–86400` 秒的创建间隔，Apple 返回 `retryAfter` 时优先采用其建议时间
- **管理目标**: 750 是 CYMail 的管理目标，不是 Apple 官方承诺的硬上限；当前 Apple 网页接口不提供可靠的 `remaining/max` 字段
- **Cookie 有效期**: 约 24 小时，需定期更新
- **邮件读取**: 依赖 IMAP 连接，超时默认 30 秒

## 库存与自助取件 API

以下管理接口都需要管理员登录会话（服务器内部自动化也可使用可选内部密钥）：

- `GET /api/accounts/:id/forwarding`：从 Apple 刷新 `selectedForwardTo`、`forwardToEmails` 和每个别名的 `forwardToEmail`，并返回各目标邮箱的 IMAP 授权状态。
- `PUT /api/accounts/:id/forwarding/default`，请求 `{"email":"receiver@example.com"}`：修改 Apple 当前默认的“转发至”邮箱。
- `POST /api/accounts/:id/forwarding/authorize`，请求 `{"email":"receiver@example.com","imap_host":"imap.example.com","imap_port":993,"username":"receiver@example.com","password":"应用密码或授权码"}`：实际连接并选择 INBOX 验证成功后，才加密保存收件授权。
- `POST /api/inventory/sync`，请求 `{"account_id":"acc_xxx"}`：同步账号下的 HME 别名到库存。
- `GET /api/inventory?status=available&limit=200`：查询库存。
- `GET /api/mailboxes?status=available&limit=200`：统一邮局中的邮箱列表，是库存接口的语义化别名。
- `GET /api/messages?account_id=&mailbox_id=&q=&limit=100`：跨账号、跨邮箱查询统一归档邮件。
- `GET /api/post-office/stats`：查询可用库存、已分配邮箱、邮件总数和有效订单数。
- `POST /api/orders/allocate`，请求 `{"external_id":"SHOP-001","ttl_hours":24}`：原子分配一个可用邮箱，并一次性返回 `email`、长串 `pickup_key`、`pickup_url` 和可直接发货的 `delivery_text`。`ttl_hours` 从收件人首次成功提交取件码时开始计算，签发和等待发货期间不消耗有效期。
- `GET /api/orders?limit=200`：查询订单。
- `POST /api/orders/:id/reissue`，请求 `{"ttl_hours":24}`：为有效订单重新签发长取件码。旧码立即失效，新码和发货文本只在本次响应中明文返回一次；新码同样在首次成功取件时激活有效期。
- `GET /api/orders/:id/messages?limit=100`：后台查看订单邮件。
- `POST /api/mail/collect`：立即执行一次收件归档；可选请求 `{"account_id":"acc_xxx","forward_to":"receiver@example.com"}` 只同步指定收件邮箱。未授权目标会明确失败，不会回退到别的邮箱。

浏览器授权接口：

- `POST /api/accounts/:id/browser-auth/start`：管理端生成五分钟一次性浏览器授权码，返回 `request_id`、`authorization_code` 和 `expires_at`。
- `GET /api/accounts/:id/browser-auth/status?request_id=...`：管理端轮询授权状态，状态为 `pending`、`processing`、`completed` 或 `error`。
- `POST /public/browser-auth/complete`：浏览器扩展提交 `{"authorization_code":"iba_...","cookies":{"name":"value"}}`。该接口不需要管理员登录会话，但必须持有高熵一次性码；码会在首次提交时原子消费，失败后也不能重用。扩展同时采集 account.apple.com Cookie 时，后端会自动引导新接口会话并返回 `apple_account: true`。

新接口（Apple Account 管理）会话：

隐藏邮箱创建支持两套配额独立的 Apple 接口：新接口（Apple Account 管理，约 20 个/小时）和旧接口（iCloud Web，约 5 个/小时），合计约 25 个/小时。创建请求会优先走新接口，限速或会话失效时自动回退旧接口；新接口明确提示总容量已满时不回退。

- `PUT /api/accounts/:id/apple-account`，请求 `{"cookies":"name1=v1; name2=v2"}` 或 `{"cookies":{"name":"value"}}`：导入 account.apple.com 浏览器 Cookie 并引导新接口会话（portal 预热 → gs/ws/token 取 scnt → /account/manage 取 apiKey）。引导成功返回 `apple_account` 状态；失败返回错误且不保存（旧接口不受影响）。
- `DELETE /api/accounts/:id/apple-account`：清除新接口会话，只保留旧接口。
- `GET /api/accounts`：每个账号附带 `apple_account_status`（enabled / last_check_ok / last_status / manage_expires_at / saved_at，不含任何凭证）。
- `POST /api/create`、`POST /api/aliases`：成功响应新增 `interface` 字段（`apple_account` 或 `icloud_web`），标识本次创建使用的接口。

新接口会话通过 TTL 自动刷新（scnt/apiKey 会轮换），无需密码；会话失效时按上述规则回退旧接口，并在 UI 中提示重新采集。

新接口密码登录（免浏览器采集）：

- `PUT /api/accounts/:id/apple-account/login`，请求 `{"password":"...","otp_code":"可选 6 位验证码"}`：用 Apple ID 密码（SRP 协议）直接登录新接口并保存会话，密码不落盘。账号启用双重认证时：未带 `otp_code` 返回 409（提示携带验证码重试），已带则自动提交；受信任设备验证码即可。国区账号自动切换 `.com.cn` 端点。

服务端定时创建调度器：

- `POST /api/scheduler/jobs`，请求 `{"name":"夜间铺库存","account_ids":["acc_xxx"],"interval_seconds":300,"rounds":0,"channels":{"apple_account":true,"icloud_web":true}}`：创建定时任务（interval_seconds≥30；rounds=0 表示无限；channels 至少启用一个）。创建后服务端独立运行，不依赖管理页。
- `GET /api/scheduler/jobs` / `GET /api/scheduler/jobs/:id`：任务列表与详情（含轮次、创建数、事件）。
- `POST /api/scheduler/jobs/:id/pause` / `/resume` / `DELETE /api/scheduler/jobs/:id`：暂停/恢复/删除。暂停中任务不创建邮箱；恢复后从下一轮继续。
- 轮次语义：每轮对每个账号按双接口自动回退创建；Apple 限速的账号按建议时间跳过；总容量已满的账号后续轮次不再尝试；失败账号下一轮重试。任务仅存内存，服务重启后需重新创建。

机器对机器接口（管理员密钥认证，Bearer / X-API-Key）：

- `POST /api/v1/mailboxes/claim`，请求可选 `{"account_ids":["acc_xxx"],"keyword":"shop"}`：原子领取一个可用邮箱并标记为已用。200 返回 `{mailbox_id, email, forward_to, label, account_id, created_at, claimed_at}`；无可用邮箱返回 409。
- `GET /api/v1/mailboxes/{email}/code?keyword=OpenAI&after=RFC3339&wait_ms=0`：按关键字查找该邮箱的验证码邮件并提取 6 位验证码，返回 `{email, code, keyword, message_id, received_at, subject, from, matched_fragment}`；`wait_ms`（0–30000）大于 0 时轮询等待新邮件约 1 秒间隔，超时返回 404。纯只读，不改变邮件状态。

新接口会话保活：服务启动后每 1 分钟扫描一次所有已启用新接口的账号，按 TTL（约 4 分钟）自动刷新会话，无需人工干预。排障时设置环境变量 `CYMAIL_DEBUG_APPLE_ACCOUNT=1`（兼容 `IPM_DEBUG_APPLE_ACCOUNT=1`），每个请求输出一行脱敏日志（方法/路径/状态/指纹），不打印完整 Cookie/scnt/apiKey。

IMAP IDLE 常驻收件（`internal/mail`）：`IdleWatchInbox(ctx, opts, handler)` 提供基于 UIDNEXT 的增量常驻收件，断线自动重连、按批回调、游标精确续传；上层可按账号一个 goroutine 接入，替代或补充 30 秒轮询。

买家接口不使用管理员登录会话或内部密钥：

- `GET /pickup#email=<邮箱>&key=<tok_长密钥>`：自助取件页面会直接打开邮箱工作区。邮箱和密钥位于 URL fragment 中，不进入普通 HTTP 访问日志。
- `POST /public/pickup`，请求 `{"email":"alias@icloud.com","key":"tok_..."}`：验证长密钥并返回该订单的邮箱和邮件。连续失败会按密钥触发临时限流。

原始 `pickup_key` 不写入数据库，数据库只保存 SHA-256 哈希。分配接口只返回一次明文凭据。`delivery_text` 格式为 `邮箱---访问密钥---取件网址`。

邮件不再依赖订单才能入库：所有状态为 `available` 或 `reserved` 的邮箱都会持续收件。订单取件接口通过邮箱 ID 和订单时间窗口查询邮件，保证买家无法读取下单前的历史邮件。

Go 后端只提供 JSON API，不再托管管理端或买家端静态页面。管理前端位于 `web/admin`，取件前端位于 `web/pickup`，两者通过各自 Nginx 反向代理访问 API。取件 Nginx 只代理精确路径 `/public/pickup`，不会代理 `/api/*` 或 `/public/browser-auth/complete`。
