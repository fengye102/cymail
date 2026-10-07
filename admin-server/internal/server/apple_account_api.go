// Package server - Apple Account 新接口的密码登录与会话保活。
//
// 密码登录 (SRP) 由 internal/hme 的 AppleSRPLogin 实现, 这里负责接入
// 账号管理与 HTTP API; 保活循环在服务启动后每 1 分钟扫描一次所有
// 已启用新接口的账号, 按 TTL 需要时刷新会话。
package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/hme"
)

// appleAccountKeepAliveInterval 是会话保活的检查间隔。
const appleAccountKeepAliveInterval = 4 * time.Minute

// appleAccountKeepAliveScanInterval 是保活循环的扫描周期。
const appleAccountKeepAliveScanInterval = 1 * time.Minute

type appleAccountLoginReq struct {
	Password string `json:"password" binding:"required"`
	OTPCode  string `json:"otp_code"` // 可选 2FA 验证码 (受信任设备)
}

// loginAppleAccount 用 Apple ID 密码登录新接口 (Apple Account 管理态)。
//
// 密码不落盘, 只保存 Cookie/scnt/apiKey 会话。账号启用双重认证时:
// 未带 otp_code 返回 409 提示携带验证码重试; 已带则直接提交。
func (s *Server) loginAppleAccount(c *gin.Context) {
	id := c.Param("id")
	acc, exists := s.mgr.GetAccount(id)
	if !exists {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	var req appleAccountLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: password 必填 — "+err.Error())
		return
	}
	username := firstNonEmpty(acc.ICloudEmail, acc.RealEmail)
	if username == "" {
		fail(c, http.StatusBadRequest, "账号未设置 Apple ID 邮箱, 请先完成网页授权或补充账号信息")
		return
	}

	var otpProvider hme.OTPProvider
	if code := strings.TrimSpace(req.OTPCode); code != "" {
		otpProvider = func() (string, error) { return code, nil }
	}

	state, err := hme.AppleSRPLogin(username, req.Password, otpProvider)
	if err != nil {
		if strings.Contains(err.Error(), "双重认证") || strings.Contains(err.Error(), "2FA") {
			fail(c, http.StatusConflict, "账号启用双重认证: 请查看受信任设备上的 6 位验证码, 携带 otp_code 重新提交 — "+err.Error())
			return
		}
		if strings.Contains(err.Error(), "密码") {
			fail(c, http.StatusUnauthorized, err.Error())
			return
		}
		fail(c, http.StatusBadGateway, "新接口登录失败: "+err.Error())
		return
	}
	if err := s.mgr.SaveAppleAccountState(id, state); err != nil {
		fail(c, http.StatusInternalServerError, "保存新接口会话失败: "+err.Error())
		return
	}
	ok(c, gin.H{
		"id":            id,
		"apple_account": state.Redacted(),
	})
}

// startAppleAccountKeepAlive 启动新接口会话保活后台循环 (生产环境由 Run/RunTLS 调用)。
func (s *Server) startAppleAccountKeepAlive(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(appleAccountKeepAliveScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.keepAliveAppleAccountsOnce()
			}
		}
	}()
}

// keepAliveAppleAccountsOnce 对所有已启用新接口的账号做一次按需保活。
func (s *Server) keepAliveAppleAccountsOnce() {
	for id, state := range s.mgr.AppleAccountStates() {
		client := hme.NewAppleAccountClient(state, false)
		if !client.NeedsKeepAlive(appleAccountKeepAliveInterval, time.Now()) {
			continue
		}
		err := client.KeepAlive()
		_ = s.mgr.SaveAppleAccountState(id, client.State())
		if err != nil {
			// 保活失败不阻塞其他账号; 下一次扫描会再次尝试。
			continue
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
