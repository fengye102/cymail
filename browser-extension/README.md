# CYMail 网页邮箱授权扩展

普通网页不能读取 `icloud.com`、`mail.163.com` 的跨域、HttpOnly Cookie，因此 CYMail 使用一个最小权限的 Manifest V3 扩展完成显式授权。扩展同时支持 Apple 账号授权和 163 网页收件授权。

## 安装

1. 打开 Chrome/Edge 的扩展管理页面并启用开发者模式。
2. 选择“加载已解压的扩展程序”。
3. 选择本目录 `browser-extension/cymail-icloud-auth`。

## Apple 授权

1. 在 CYMail 后台先创建一个待授权账号。
2. 点击“一键打开 iCloud 授权”。管理页通过本机扩展消息桥传递五分钟一次性凭证。
3. 扩展确认接收后，浏览器打开不带任何 CYMail 参数的 Apple 官方 iCloud 页面。
4. 在 Apple 页面自行输入密码和双重验证码。
5. 登录完成后，核对页面右上角 CYMail 浮层中的后台域名并点击“确认授权 CYMail”。扩展弹窗仍可作为备用入口。

扩展不会读取 Apple 密码或验证码。它只会在用户点击确认授权按钮后读取 iCloud 会话 Cookie，并提交到授权请求指定的 CYMail 后台地址。后台验证成功后使用 `ICLOUD_HME_DATA_KEY` 加密保存会话。

## 163 网页收件授权

1. 在 CYMail 后台刷新 Apple 转发设置并选中一个 `@163.com` 收件邮箱。
2. 点击“网页登录 163 并授权”，扩展接收五分钟一次性凭证并打开 `https://mail.163.com/`。
3. 用户只在 163 官方页面输入密码、短信验证码或其他登录信息。
4. 进入收件箱后，在页面右上角 CYMail 浮层点击确认。
5. 后端用只读邮件列表请求验证网页会话，然后加密保存 Cookie 和 SID。

163 取件不使用 IMAP、客户端授权码或应用密码。

本地开发默认允许 `http://127.0.0.1/*` 和 `http://localhost/*` 管理页连接扩展。生产部署时，应把实际 HTTPS 管理域名同时加入 `manifest.json` 的 `host_permissions` 与管理页内容脚本 `matches`，不要放宽到不受控域名。
