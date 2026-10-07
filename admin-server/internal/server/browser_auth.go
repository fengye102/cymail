package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"icloud-hme/internal/account"
)

const browserAuthTTL = 5 * time.Minute

type browserAuthSession struct {
	RequestID string
	AccountID string
	Kind      string
	Email     string
	Provider  string
	ExpiresAt time.Time
	Status    string
	LastError string
	TokenHash [32]byte
}

func (s *Server) cleanupBrowserAuthLocked(now time.Time) {
	for requestID, session := range s.browserAuthByRequest {
		if now.After(session.ExpiresAt) {
			delete(s.browserAuthByRequest, requestID)
			delete(s.browserAuthByToken, session.TokenHash)
		}
	}
}

func (s *Server) startBrowserAuth(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	if _, ok := s.mgr.GetAccount(accountID); !ok {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fail(c, http.StatusInternalServerError, "无法创建浏览器授权")
		return
	}
	token := "iba_" + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	session := &browserAuthSession{RequestID: uuid.NewString(), AccountID: accountID, Kind: "icloud", ExpiresAt: time.Now().Add(browserAuthTTL), Status: "pending", TokenHash: hash}
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	s.browserAuthByToken[hash] = session
	s.browserAuthByRequest[session.RequestID] = session
	s.browserAuthMu.Unlock()
	ok(c, gin.H{"request_id": session.RequestID, "authorization_code": token, "expires_at": session.ExpiresAt})
}

type startForwardWebAuthReq struct {
	Email string `json:"email" binding:"required"`
}

func (s *Server) startForwardWebAuth(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	var req startForwardWebAuthReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "收件邮箱必填")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	acc, exists := s.mgr.GetAccount(accountID)
	if !exists {
		fail(c, http.StatusNotFound, "账号不存在")
		return
	}
	allowed := false
	for _, candidate := range acc.ForwardToEmails {
		if strings.EqualFold(candidate, email) {
			allowed = true
			break
		}
	}
	if !allowed {
		fail(c, http.StatusBadRequest, "请先刷新 Apple 转发邮箱列表")
		return
	}
	if !strings.HasSuffix(email, "@163.com") {
		fail(c, http.StatusBadRequest, "目前仅支持 163 邮箱网页登录取件")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fail(c, http.StatusInternalServerError, "无法创建 163 网页授权")
		return
	}
	token := "fwa_" + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	session := &browserAuthSession{
		RequestID: uuid.NewString(), AccountID: accountID, Kind: "forwarding", Email: email,
		Provider: "netease_163", ExpiresAt: time.Now().Add(browserAuthTTL), Status: "pending", TokenHash: hash,
	}
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	s.browserAuthByToken[hash] = session
	s.browserAuthByRequest[session.RequestID] = session
	s.browserAuthMu.Unlock()
	ok(c, gin.H{
		"request_id": session.RequestID, "authorization_code": token, "email": email,
		"provider": session.Provider, "target_url": "https://mail.163.com/", "expires_at": session.ExpiresAt,
	})
}

type completeBrowserAuthReq struct {
	AuthorizationCode string            `json:"authorization_code" binding:"required"`
	Cookies           map[string]string `json:"cookies" binding:"required"`
}

func (s *Server) completeBrowserAuth(c *gin.Context) {
	var req completeBrowserAuthReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "authorization_code 和 cookies 必填")
		return
	}
	if len(req.Cookies) == 0 || len(req.Cookies) > 200 {
		fail(c, http.StatusBadRequest, "iCloud Cookie 数量无效")
		return
	}
	hash := sha256.Sum256([]byte(strings.TrimSpace(req.AuthorizationCode)))
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	session := s.browserAuthByToken[hash]
	if session == nil || session.Kind != "icloud" || session.Status != "pending" || time.Now().After(session.ExpiresAt) {
		s.browserAuthMu.Unlock()
		fail(c, http.StatusUnauthorized, "浏览器授权码无效或已过期")
		return
	}
	// Reserve the one-time code before validating the remote iCloud session so
	// concurrent submissions cannot consume the same authorization twice.
	session.Status = "processing"
	accountID := session.AccountID
	s.browserAuthMu.Unlock()

	if err := s.mgr.UpdateCookies(accountID, req.Cookies); err != nil {
		s.browserAuthMu.Lock()
		session.Status = "error"
		session.LastError = err.Error()
		delete(s.browserAuthByToken, hash)
		s.browserAuthMu.Unlock()
		fail(c, http.StatusBadRequest, "iCloud 会话校验失败，请在 CYMail 后台查看详情并重新授权")
		return
	}

	// 顺带引导新接口 (Apple Account 管理) 会话: 若浏览器同时持有
	// account.apple.com 登录 Cookie 则自动启用 (配额约 20 个/小时)。
	// 引导失败不阻塞本次授权, 旧接口 (约 5 个/小时) 照常可用。
	appleAccountEnabled := false
	if _, err := s.mgr.SetAppleAccount(accountID, req.Cookies); err == nil {
		appleAccountEnabled = true
	}

	s.browserAuthMu.Lock()
	session.Status = "completed"
	session.LastError = ""
	delete(s.browserAuthByToken, hash)
	s.browserAuthMu.Unlock()
	ok(c, gin.H{
		"account_id":        accountID,
		"cookies_count":     len(req.Cookies),
		"apple_account":     appleAccountEnabled,
		"apple_account_msg": appleAccountHint(appleAccountEnabled),
	})
}

// appleAccountHint 返回新接口启用提示文案。
func appleAccountHint(enabled bool) string {
	if enabled {
		return "已启用 Apple Account 管理接口 (约 20 个/小时)"
	}
	return "未检测到 account.apple.com 会话: 如需提高创建配额 (约 25 个/小时), 请在浏览器打开 https://account.apple.com 登录后重新授权"
}

type completeForwardWebAuthReq struct {
	AuthorizationCode string            `json:"authorization_code" binding:"required"`
	Email             string            `json:"email" binding:"required"`
	Provider          string            `json:"provider" binding:"required"`
	Cookies           map[string]string `json:"cookies" binding:"required"`
	SessionID         string            `json:"session_id" binding:"required"`
}

func (s *Server) completeForwardWebAuth(c *gin.Context) {
	var req completeForwardWebAuthReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "163 网页授权信息不完整")
		return
	}
	if len(req.Cookies) == 0 || len(req.Cookies) > 200 || len(req.SessionID) > 512 {
		fail(c, http.StatusBadRequest, "163 网页会话格式无效")
		return
	}
	hash := sha256.Sum256([]byte(strings.TrimSpace(req.AuthorizationCode)))
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	session := s.browserAuthByToken[hash]
	if session == nil || session.Kind != "forwarding" || session.Status != "pending" || time.Now().After(session.ExpiresAt) {
		s.browserAuthMu.Unlock()
		fail(c, http.StatusUnauthorized, "163 网页授权码无效或已过期")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.Email), session.Email) || req.Provider != session.Provider {
		s.browserAuthMu.Unlock()
		fail(c, http.StatusBadRequest, "当前 163 登录账号与待授权收件邮箱不一致")
		return
	}
	session.Status = "processing"
	accountID, email := session.AccountID, session.Email
	s.browserAuthMu.Unlock()

	err := s.mgr.AuthorizeForwardWebMailbox(accountID, account.ForwardMailbox{
		Email: email, Provider: session.Provider, WebHost: "mail.163.com",
		Cookies: req.Cookies, SessionID: strings.TrimSpace(req.SessionID),
	})
	if err != nil {
		s.browserAuthMu.Lock()
		session.Status = "error"
		session.LastError = err.Error()
		delete(s.browserAuthByToken, hash)
		s.browserAuthMu.Unlock()
		fail(c, http.StatusBadRequest, "163 网页会话校验失败，请确认已进入收件箱后重新授权")
		return
	}
	s.browserAuthMu.Lock()
	session.Status = "completed"
	session.LastError = ""
	delete(s.browserAuthByToken, hash)
	s.browserAuthMu.Unlock()
	ok(c, gin.H{"account_id": accountID, "email": email, "cookies_count": len(req.Cookies)})
}

func (s *Server) browserAuthStatus(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	requestID := strings.TrimSpace(c.Query("request_id"))
	if requestID == "" {
		fail(c, http.StatusBadRequest, "request_id 必填")
		return
	}
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	session := s.browserAuthByRequest[requestID]
	if session == nil || session.AccountID != accountID || session.Kind != "icloud" {
		s.browserAuthMu.Unlock()
		fail(c, http.StatusNotFound, "浏览器授权请求不存在或已过期")
		return
	}
	status, lastError, expiresAt := session.Status, session.LastError, session.ExpiresAt
	s.browserAuthMu.Unlock()
	ok(c, gin.H{"request_id": requestID, "status": status, "error": lastError, "expires_at": expiresAt})
}

func (s *Server) forwardWebAuthStatus(c *gin.Context) {
	accountID := strings.TrimSpace(c.Param("id"))
	requestID := strings.TrimSpace(c.Query("request_id"))
	if requestID == "" {
		fail(c, http.StatusBadRequest, "request_id 必填")
		return
	}
	s.browserAuthMu.Lock()
	s.cleanupBrowserAuthLocked(time.Now())
	session := s.browserAuthByRequest[requestID]
	if session == nil || session.AccountID != accountID || session.Kind != "forwarding" {
		s.browserAuthMu.Unlock()
		fail(c, http.StatusNotFound, "163 网页授权请求不存在或已过期")
		return
	}
	status, lastError, expiresAt, email := session.Status, session.LastError, session.ExpiresAt, session.Email
	s.browserAuthMu.Unlock()
	ok(c, gin.H{"request_id": requestID, "status": status, "error": lastError, "email": email, "expires_at": expiresAt})
}
