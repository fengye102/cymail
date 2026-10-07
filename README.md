# CYMail 部署与使用说明

本交付包包含三个目录：

| 目录 | 用途 |
| --- | --- |
| `browser-extension` | Chrome / Edge 网页授权扩展，负责在用户确认后提交 iCloud 或 163 网页会话 |
| `admin-server` | Go API、管理端网页、PostgreSQL、Caddy HTTPS 配置和一键部署脚本 |
| `pickup-web` | 给收件人使用的取件网页 |

交付包是全新空数据状态，不包含开发电脑中的账号、Cookie、管理员密码、密钥、隐藏邮箱、邮件、订单或日志。服务器首次部署后需要重新创建管理员账号并重新授权邮箱。

## 一、服务器一键部署

### 准备条件

1. 一台 Linux 服务器，建议 Ubuntu 22.04/24.04，至少 2 核 CPU、2 GB 内存和 20 GB 磁盘。
2. 准备两个不同域名，例如：
   - 管理端：`admin.example.com`
   - 取件端：`mail.example.com`
3. 把两个域名的 A/AAAA 记录指向服务器公网 IP。
4. 安全组/防火墙开放 TCP 80、443，建议同时开放 UDP 443。
5. 将整个解压后的项目目录上传到服务器。

进入项目顶层目录后，只需运行这一条命令：

```bash
sudo bash ./admin-server/deploy.sh admin.example.com mail.example.com
```

请把示例域名换成自己的真实域名。脚本会自动：

- 安装 Docker（服务器尚未安装时）；
- 生成随机数据库密码、内部 API 密钥和数据加密密钥；
- 启动 PostgreSQL、CYMail API、管理端、取件端和 Caddy；
- 申请并自动续期 HTTPS 证书；
- 创建全新的空数据库和运行数据卷。

部署完成后访问：

- 管理端：`https://admin.example.com/login.html`
- 取件端：`https://mail.example.com/pickup`

DNS 尚未生效或 80/443 端口未开放时，HTTPS 证书会暂时申请失败。修正网络后执行下面的命令即可重试：

```bash
cd admin-server && sudo docker compose up -d
```

## 二、配置并安装浏览器扩展

生产服务器的后台域名需要先写入扩展白名单。在 Windows PowerShell 中进入本项目顶层目录，执行：

```powershell
powershell -ExecutionPolicy Bypass -File .\browser-extension\configure-domain.ps1 https://admin.example.com
```

然后安装扩展：

1. Chrome 打开 `chrome://extensions/`，Edge 打开 `edge://extensions/`。
2. 开启右上角“开发者模式”。
3. 点击“加载已解压的扩展程序”。
4. 选择本项目的 `browser-extension` 文件夹。
5. 如果以后重新执行域名配置脚本，需要回到扩展管理页点击“重新加载”。

扩展不会读取 Apple 或 163 的密码和验证码。密码、短信验证码和双重验证码只在对应官方网页输入；扩展只会在你点击确认授权后提交网页会话。

## 三、首次使用

1. 打开管理端的 `/login.html`。
2. 首次进入时创建管理员用户名和至少 12 位密码。
3. 在侧边栏进入“iCloud 账号”，添加账号记录。
4. 点击“一键打开 iCloud 授权”，在 Apple 官方页面完成登录，再通过页面浮层或扩展弹窗确认。
5. 进入“转发邮箱”，刷新 Apple 转发目标并选择默认收件邮箱。
6. 如果目标是 163 邮箱，点击“网页登录 163 并授权”，只在 163 官方网页完成登录并确认。
7. 点击“同步邮箱库存”，把 Apple 隐藏邮箱同步到 CYMail。
8. 在“新建邮箱”中可以单个或批量创建，并可将创建间隔设置为 `0–86400` 秒；实际创建数量和频率仍以 Apple 返回结果为准。
9. 在“统一收件箱”同步并查看按隐藏邮箱分类的邮件。
10. 在“取件码与发货”选择邮箱、有效期和外部订单号，然后签发取件码。

邮箱列表默认按创建时间从新到旧排列；点击“创建时间”表头可以切换为最早优先。

## 四、取件端使用

管理端复制的批量发货格式为：

```text
隐藏邮箱-----取件码
```

收件人打开取件端，将完整的一行粘贴进去即可。系统也支持从签发结果中的取件链接直接进入。重新签发后旧取件码会立即失效；后台不会保存可再次查看的明文取件码。

## 五、常用服务器命令

在服务器的 `admin-server` 目录运行：

```bash
# 查看运行状态
sudo docker compose ps

# 查看最近日志（日志中仍不要公开粘贴会话信息）
sudo docker compose logs --tail=200

# 重启全部服务
sudo docker compose restart

# 重新构建并更新
sudo docker compose up -d --build

# 停止服务但保留数据
sudo docker compose down
```

只有明确需要彻底清空服务器数据时才运行下面的命令。它会删除管理员账号、邮箱会话、邮箱库存、邮件和订单，无法恢复：

```bash
sudo docker compose down -v
```

## 六、备份与安全

- `admin-server/.env` 保存服务器随机密钥，权限默认为仅 root 可读，不要上传或发给他人。
- PostgreSQL 数据和加密会话保存在 Docker 卷中，不在本交付包源码目录里。
- 不要把 API 的 8081 端口直接暴露到公网；本配置只通过 Caddy 的 HTTPS 入口访问。
- 定期备份数据库和 Docker 卷，备份文件也必须按敏感数据保护。
- “750”是 CYMail 的库存管理目标，不是 Apple 官方承诺额度。CYMail 不再强制“每小时 5 个”，但 Apple 仍可能按账号状态、时间窗口、订阅或风险控制限制创建数量。
- 停用、恢复和永久删除都会调用 Apple 隐藏邮箱接口；永久删除成功后无法恢复。执行不可恢复操作前请再次核对邮箱地址。

## 七、故障排查

- 页面提示连接失败：运行 `sudo docker compose ps`，确认五个服务均已启动。
- HTTPS 打不开：检查域名解析、TCP 80/443、防火墙和 Caddy 日志。
- 扩展没有响应：确认已经运行域名配置脚本、重新加载扩展，并刷新管理页面。
- iCloud 显示 421：刷新管理页后重新发起一次授权；不要把返回中的信任令牌发给他人。
- 163 收不到完整内容：确认网页授权仍有效，再在统一收件箱重新同步。
- 批量创建被暂停：系统会按照自定义间隔或 Apple 返回的等待时间自动继续，也可以停止任务后调整间隔。
