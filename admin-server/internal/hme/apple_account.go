// Package hme - Apple Account 管理接口客户端 (新接口)
//
// 与旧 iCloud Web 接口 (maildomainws /v1/hme/*) 配额独立:
//   - 新接口 (appleid.apple.com/account/manage/email/private/*) 约 20 个/小时
//   - 旧接口 (iCloud Web) 约 5 个/小时
//
// 两个接口合计约 25 个/小时。创建时优先新接口, 限速或会话失效后回退旧接口。
//
// 会话来源: 浏览器 account.apple.com 登录 Cookie (扩展采集或手动粘贴)。
// 服务端通过 Bootstrap 从 Cookie 引导出 scnt / apiKey, 并按 TTL 自动刷新。
//
// 中国大陆域支持: 会话状态可携带 Host (如 "appleid.apple.com.cn"),
// 客户端按 Host 解析管理 API Base URL 与门户 Origin。
package hme

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	appleAccountManageOrigin     = "https://account.apple.com"
	appleAccountManageUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	appleAccountManageRequestCtx = "ca"
	appleAccountManageLanguage   = "zh"
	appleAccountManageTimeZone   = "Asia/Shanghai"
	appleAccountManageGMTOffset  = "GMT+08:00"
	appleAccountManagePlatform   = `"macOS"`
	appleAccountManageTZOffset   = 8 * 60 * 60

	// appleAccountHTTPStatusSessionTimeout 是 Apple Account 管理接口的会话超时状态码。
	appleAccountHTTPStatusSessionTimeout = 419

	// appleAccountRequestTimeout 单次请求超时。
	appleAccountRequestTimeout = 30 * time.Second

	// appleAccountTransientRetries 瞬态网络错误的最大重试次数。
	appleAccountTransientRetries = 3
	// appleAccountTransientBackoff 瞬态网络错误重试的基础退避间隔 (800ms/1.6s/2.4s)。
	appleAccountTransientBackoff = 800 * time.Millisecond
	// appleAccountKeepAliveDefaultInterval 是 NeedsKeepAlive 未指定间隔时的默认保活间隔。
	appleAccountKeepAliveDefaultInterval = 4 * time.Minute

	// appleAccountKeepAliveForwardEmailPath 保活检查端点。
	appleAccountKeepAliveForwardEmailPath = "/account/manage/forwardemail"
	// appleAccountKeepAliveJSLogsPath 保活心跳端点 (尽力而为)。
	appleAccountKeepAliveJSLogsPath = "/v2/jslogs"
)

// appleAccountManageBaseURL 是 Apple Account 管理 API 的 Base URL。
// 定义为变量以便单测重定向到 mock 服务; 生产环境固定为 appleid.apple.com。
var appleAccountManageBaseURL = "https://appleid.apple.com"

// ErrAppleAccountAuthFailed 表示 Apple Account 管理态会话已失效, 需要重新采集浏览器 Cookie。
var ErrAppleAccountAuthFailed = errors.New("Apple Account management session expired")

// AppleAccountManageOrigin 返回新接口门户 Origin。
func AppleAccountManageOrigin() string { return appleAccountManageOrigin }

// AppleAccountManageUserAgent 返回新接口默认 User-Agent。
func AppleAccountManageUserAgent() string { return appleAccountManageUserAgent }

// SetAppleAccountBaseURLForTesting 重定向管理 API Base URL (仅供测试注入 mock 服务)。
// 返回恢复函数; 生产代码不应调用。
func SetAppleAccountBaseURLForTesting(rawURL string) func() {
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = rawURL
	return func() { appleAccountManageBaseURL = previous }
}

// AppleAccountManageBaseForHost 返回指定 iCloud / Apple Account 域名对应的管理 API Base URL。
// 中国大陆域 (icloud.com.cn / appleid.apple.com.cn / account.apple.com.cn) 解析为
// https://appleid.apple.com.cn, 其余为 https://appleid.apple.com。
func AppleAccountManageBaseForHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(h, "icloud.com.cn") || strings.Contains(h, "appleid.apple.com.cn") || strings.Contains(h, "account.apple.com.cn") {
		return "https://appleid.apple.com.cn"
	}
	return "https://appleid.apple.com"
}

// AppleAccountManageOriginForHost 返回指定域名对应的门户 Origin。
// 中国大陆域解析为 https://account.apple.com.cn, 其余为 https://account.apple.com。
func AppleAccountManageOriginForHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(h, "icloud.com.cn") || strings.Contains(h, "appleid.apple.com.cn") || strings.Contains(h, "account.apple.com.cn") {
		return "https://account.apple.com.cn"
	}
	return appleAccountManageOrigin
}

// appleAccountManageBaseForState 按会话状态解析管理 API Base URL。
// 测试注入的 override (SetAppleAccountBaseURLForTesting) 优先; 未设置时才按 state.Host 解析。
func appleAccountManageBaseForState(state *AppleAccountState) string {
	baseURL := strings.TrimSpace(appleAccountManageBaseURL)
	if baseURL != "" && strings.TrimRight(baseURL, "/") != "https://appleid.apple.com" {
		return baseURL
	}
	if state == nil {
		return "https://appleid.apple.com"
	}
	return AppleAccountManageBaseForHost(state.Host)
}

// appleAccountManagePortalBaseForState 按会话状态解析门户 Base URL (portalRequest 使用)。
// 测试注入的 override 优先; 其次按 state.Host 解析 .cn 门户; 否则维持 state.Origin / 默认值。
func appleAccountManagePortalBaseForState(state *AppleAccountState) string {
	baseURL := strings.TrimSpace(appleAccountManageBaseURL)
	if baseURL != "" && strings.TrimRight(baseURL, "/") != "https://appleid.apple.com" {
		return baseURL
	}
	if state == nil {
		return appleAccountManageOrigin
	}
	host := strings.ToLower(strings.TrimSpace(state.Host))
	if strings.Contains(host, "icloud.com.cn") || strings.Contains(host, "appleid.apple.com.cn") || strings.Contains(host, "account.apple.com.cn") {
		return "https://account.apple.com.cn"
	}
	if strings.TrimSpace(state.Origin) != "" {
		return strings.TrimRight(state.Origin, "/")
	}
	return appleAccountManageOrigin
}

// appleAccountManageOriginForState 返回请求头 (Origin/Referer) 使用的门户 Origin。
// 中国大陆域按 state.Host 解析为 account.apple.com.cn, 否则维持 state.Origin / 默认值。
func appleAccountManageOriginForState(state *AppleAccountState) string {
	host := strings.ToLower(strings.TrimSpace(state.Host))
	if strings.Contains(host, "icloud.com.cn") || strings.Contains(host, "appleid.apple.com.cn") || strings.Contains(host, "account.apple.com.cn") {
		return "https://account.apple.com.cn"
	}
	if state != nil && strings.TrimSpace(state.Origin) != "" {
		return strings.TrimRight(state.Origin, "/")
	}
	return appleAccountManageOrigin
}

// AppleAccountState 是 Apple Account 管理接口的可持久化会话状态。
//
// 仅包含浏览器 Cookie 会话与 Apple 下发的 scnt / apiKey,
// 不含密码。随 accounts.json 一起加密存储 (配置数据密钥时)。
type AppleAccountState struct {
	Cookies         map[string]string `json:"cookies"`
	Scnt            string            `json:"scnt"`
	APIKey          string            `json:"api_key"`
	SessionID       string            `json:"session_id,omitempty"`
	Host            string            `json:"host,omitempty"`
	Origin          string            `json:"origin,omitempty"`
	UserAgent       string            `json:"user_agent,omitempty"`
	SavedAt         time.Time         `json:"saved_at,omitempty"`
	LastCheckedAt   time.Time         `json:"last_checked_at,omitempty"`
	ManageExpiresAt time.Time         `json:"manage_expires_at,omitempty"`
	LastCheckOK     bool              `json:"last_check_ok,omitempty"`
	LastStatus      string            `json:"last_status,omitempty"`
}

// Clone 返回状态的深拷贝。
func (s *AppleAccountState) Clone() AppleAccountState {
	if s == nil {
		return AppleAccountState{}
	}
	cp := *s
	cp.Cookies = make(map[string]string, len(s.Cookies))
	for k, v := range s.Cookies {
		cp.Cookies[k] = v
	}
	return cp
}

// Redacted 返回不含 Cookie / scnt / apiKey 的脱敏视图 (仅状态字段)。
func (s *AppleAccountState) Redacted() *AppleAccountState {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Cookies = nil
	cp.Scnt = ""
	cp.APIKey = ""
	cp.SessionID = ""
	return &cp
}

// AppleAccountClient 是 Apple Account 管理接口客户端。
//
// 使用标准 http.Client + Chrome 系请求头。Apple 对该接口的风控基于
// scnt / X-Apple-Api-Key / X-Apple-I-FD-Client-Info 等请求头, 无需 TLS 指纹。
type AppleAccountClient struct {
	mu      sync.Mutex // 串行化 refresh / create, 防止并发会话竞态
	state   AppleAccountState
	httpc   *http.Client
	verbose bool
}

// NewAppleAccountClient 创建新接口客户端。
// state 可为空 (仅有浏览器 Cookie), 首次业务调用前由 Bootstrap 引导出 scnt / apiKey。
func NewAppleAccountClient(state AppleAccountState, verbose bool) *AppleAccountClient {
	if state.Origin == "" {
		state.Origin = AppleAccountManageOriginForHost(state.Host)
	}
	if state.UserAgent == "" {
		state.UserAgent = appleAccountManageUserAgent
	}
	if state.Cookies == nil {
		state.Cookies = make(map[string]string)
	}
	return &AppleAccountClient{
		state:   state,
		httpc:   &http.Client{Timeout: appleAccountRequestTimeout},
		verbose: verbose,
	}
}

// State 返回当前会话状态副本, 供调用方持久化。
func (c *AppleAccountClient) State() AppleAccountState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Clone()
}

// HasSession 判断是否已保存 Apple Account 会话 (仅看 Cookie)。
func (c *AppleAccountClient) HasSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.state.Cookies) > 0
}

// NeedsRefresh 判断会话是否需要刷新 (scnt/apiKey 缺失、TTL 过期或上次失败)。
func (c *AppleAccountClient) NeedsRefresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.needsRefreshLocked(time.Now())
}

func (c *AppleAccountClient) needsRefreshLocked(now time.Time) bool {
	state := &c.state
	if strings.TrimSpace(state.Scnt) == "" || strings.TrimSpace(state.APIKey) == "" {
		return true
	}
	if state.LastCheckedAt.IsZero() || state.ManageExpiresAt.IsZero() || !state.LastCheckOK {
		return true
	}
	return !now.Before(state.ManageExpiresAt)
}

func (c *AppleAccountClient) log(format string, args ...any) {
	if c.verbose {
		fmt.Printf("  [AppleAccount] %s\n", fmt.Sprintf(format, args...))
	}
}

// markOK 记录一次成功的会话检查。
func (c *AppleAccountClient) markOK() {
	c.state.LastCheckedAt = time.Now()
	c.state.LastCheckOK = true
	c.state.LastStatus = "新接口会话正常"
}

func (c *AppleAccountClient) markTokenTTL(timeoutMinutes int, now time.Time) {
	if timeoutMinutes > 0 {
		c.state.ManageExpiresAt = now.Add(time.Duration(timeoutMinutes) * time.Minute)
	}
}

func (c *AppleAccountClient) markError(status string) {
	c.state.LastCheckedAt = time.Now()
	c.state.LastCheckOK = false
	c.state.LastStatus = status
}

// Bootstrap 从浏览器 Cookie 引导出完整会话 (scnt + apiKey)。
//
// 步骤: 预热门户 → 请求 gs/ws/token 捕获 scnt → 读取 /account/manage 捕获 apiKey。
// 失败返回 ErrAppleAccountAuthFailed (Cookie 无效或账号无 iCloud+ 权限等)。
func (c *AppleAccountClient) Bootstrap() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log("引导 Apple Account 管理会话...")
	if len(c.state.Cookies) == 0 {
		return ErrAppleAccountAuthFailed
	}
	if err := c.warmPortal(); err != nil {
		c.markError("门户预热失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	if err := c.fetchTokenScnt(); err != nil {
		c.markError("获取管理 token 失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	if err := c.loadAPIKey(); err != nil {
		c.markError("获取管理 api_key 失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	c.markOK()
	c.log("会话就绪 (scnt/apiKey 已获取)")
	return nil
}

// Refresh 刷新会话 (scnt + apiKey), 要求已有 scnt。
//
// 优先使用现有 scnt; 失败时预热门户并以无 scnt 方式重取 (Apple 会通过响应头轮换 scnt)。
func (c *AppleAccountClient) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked()
}

func (c *AppleAccountClient) refreshLocked() error {
	c.log("刷新 Apple Account 管理会话...")
	if strings.TrimSpace(c.state.Scnt) == "" {
		return c.bootstrapLocked()
	}
	previous := c.state.Scnt
	if err := c.fetchTokenScnt(); err != nil {
		// token 端点失败 → 尝试门户预热后无 scnt 重取
		c.state.Scnt = ""
		_ = c.warmPortal()
		if retryErr := c.fetchTokenScnt(); retryErr != nil {
			c.state.Scnt = previous
			c.markError("刷新管理 token 失败: " + firstNonEmpty(err.Error(), "unknown"))
			return retryErr
		}
	}
	if err := c.loadAPIKey(); err != nil {
		// apiKey 端点失败 → 门户预热后无 scnt 重试
		c.state.Scnt = ""
		_ = c.warmPortal()
		if retryErr := c.fetchTokenScnt(); retryErr != nil {
			c.state.Scnt = previous
			c.markError("刷新会话失败: " + firstNonEmpty(err.Error(), "unknown"))
			return retryErr
		}
		if retryErr := c.loadAPIKey(); retryErr != nil {
			c.state.Scnt = previous
			c.markError("获取管理 api_key 失败: " + firstNonEmpty(retryErr.Error(), "unknown"))
			return retryErr
		}
	}
	c.markOK()
	return nil
}

func (c *AppleAccountClient) bootstrapLocked() error {
	if err := c.warmPortal(); err != nil {
		c.markError("门户预热失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	if err := c.fetchTokenScnt(); err != nil {
		c.markError("获取管理 token 失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	if err := c.loadAPIKey(); err != nil {
		c.markError("获取管理 api_key 失败: " + firstNonEmpty(err.Error(), "unknown"))
		return err
	}
	c.markOK()
	return nil
}

// warmPortal 预热门户, 换取会话 Cookie 刷新与 token TTL。
func (c *AppleAccountClient) warmPortal() error {
	c.log("预热门户...")
	_, err := c.portalRequest("/account/manage/section/privacy", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7", false, "document", "navigate")
	if err != nil {
		return err
	}
	data, err := c.portalRequest("/bootstrap/portal", "application/json, text/plain, */*", true, "empty", "cors")
	if err != nil {
		return err
	}
	var portal struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if json.Unmarshal(data, &portal) == nil {
		c.markTokenTTL(portal.TimeOutInterval, time.Now())
	}
	return nil
}

// portalRequest 请求门户页面 (HTML/JSON), 底层瞬态网络错误自动重试。
func (c *AppleAccountClient) portalRequest(path, accept string, jsonContent bool, secFetchDest, secFetchMode string) ([]byte, error) {
	var data []byte
	err := retryAppleAccountTransient(func() error {
		next, err := c.portalRequestOnce(path, accept, jsonContent, secFetchDest, secFetchMode)
		if err != nil {
			return err
		}
		data = next
		return nil
	})
	return data, err
}

func (c *AppleAccountClient) portalRequestOnce(path, accept string, jsonContent bool, secFetchDest, secFetchMode string) ([]byte, error) {
	base := strings.TrimRight(appleAccountManagePortalBaseForState(&c.state), "/")
	rawURL := base + path
	ctx, cancel := context.WithTimeout(context.Background(), appleAccountRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if jsonContent {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Referer", base+"/")
	req.Header.Set("User-Agent", c.state.UserAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", secFetchMode)
	req.Header.Set("Sec-Fetch-Dest", secFetchDest)
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	if cookie := c.cookieHeader(rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if jsonContent {
		req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
		req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
		req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(c.state.UserAgent))
	}
	resp, data, err := c.doAppleAccountRequest(req, http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, c.appleError("打开隐私页面", resp.StatusCode, data)
	}
	c.mergeSetCookies(resp)
	c.updateStateFromHeaders(resp)
	return data, nil
}

// fetchTokenScnt 请求 gs/ws/token, 从响应头捕获最新 scnt 并解析 TTL。
func (c *AppleAccountClient) fetchTokenScnt() error {
	c.log("获取管理 token (scnt)...")
	return retryAppleAccountTransient(func() error {
		return c.fetchTokenScntOnce()
	})
}

func (c *AppleAccountClient) fetchTokenScntOnce() error {
	rawURL := appleAccountManageBaseForState(&c.state) + "/account/manage/gs/ws/token"
	ctx, cancel := context.WithTimeout(context.Background(), appleAccountRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	origin := appleAccountManageOriginForState(&c.state)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.state.UserAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(c.state.UserAgent))
	req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
	req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
	if scnt := strings.TrimSpace(c.state.Scnt); scnt != "" {
		req.Header.Set("scnt", scnt)
	}
	if cookie := c.cookieHeader(rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, data, err := c.doAppleAccountRequest(req, http.MethodGet, "/account/manage/gs/ws/token")
	if err != nil {
		return err
	}
	if scnt := strings.TrimSpace(resp.Header.Get("scnt")); scnt != "" {
		c.state.Scnt = scnt
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.appleError("刷新管理 token", resp.StatusCode, data)
	}
	var token struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if json.Unmarshal(data, &token) == nil {
		c.markTokenTTL(token.TimeOutInterval, time.Now())
	}
	c.mergeSetCookies(resp)
	c.updateStateFromHeaders(resp)
	return nil
}

// loadAPIKey 请求 /account/manage, 从响应 JSON 捕获 apiKey。
func (c *AppleAccountClient) loadAPIKey() error {
	c.log("读取管理入口 (api_key)...")
	return retryAppleAccountTransient(func() error {
		return c.loadAPIKeyOnce()
	})
}

func (c *AppleAccountClient) loadAPIKeyOnce() error {
	rawURL := appleAccountManageBaseForState(&c.state) + "/account/manage"
	ctx, cancel := context.WithTimeout(context.Background(), appleAccountRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	origin := appleAccountManageOriginForState(&c.state)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.state.UserAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	if scnt := strings.TrimSpace(c.state.Scnt); scnt != "" {
		req.Header.Set("scnt", scnt)
	}
	if apiKey := strings.TrimSpace(c.state.APIKey); apiKey != "" {
		req.Header.Set("X-Apple-Api-Key", apiKey)
	}
	req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(c.state.UserAgent))
	req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
	req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
	if cookie := c.cookieHeader(rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, data, err := c.doAppleAccountRequest(req, http.MethodGet, "/account/manage")
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.appleError("读取管理入口", resp.StatusCode, data)
	}
	var manage struct {
		APIKey string `json:"apiKey"`
	}
	if json.Unmarshal(data, &manage) == nil {
		if apiKey := strings.TrimSpace(manage.APIKey); apiKey != "" {
			c.state.APIKey = apiKey
		}
	}
	if strings.TrimSpace(c.state.APIKey) == "" {
		return fmt.Errorf("%w: Apple Account 管理接口未返回 api_key", ErrAppleAccountAuthFailed)
	}
	c.mergeSetCookies(resp)
	c.updateStateFromHeaders(resp)
	return nil
}

// callAPI 请求 Apple Account 管理 API (需 scnt + apiKey), 返回原始响应。
func (c *AppleAccountClient) callAPI(method, path string, body any) (int, []byte, error) {
	var status int
	var data []byte
	err := retryAppleAccountTransient(func() error {
		nextStatus, nextData, err := c.callAPIOnce(method, path, body)
		if err != nil {
			return err
		}
		status = nextStatus
		data = nextData
		return nil
	})
	return status, data, err
}

func (c *AppleAccountClient) callAPIOnce(method, path string, body any) (int, []byte, error) {
	rawURL := appleAccountManageBaseForState(&c.state) + path
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), appleAccountRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}
	origin := appleAccountManageOriginForState(&c.state)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.state.UserAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	if scnt := strings.TrimSpace(c.state.Scnt); scnt != "" {
		req.Header.Set("scnt", scnt)
	}
	if apiKey := strings.TrimSpace(c.state.APIKey); apiKey != "" {
		req.Header.Set("X-Apple-Api-Key", apiKey)
	}
	req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(c.state.UserAgent))
	req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
	req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
	if cookie := c.cookieHeader(rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, data, err := c.doAppleAccountRequest(req, method, path)
	if err != nil {
		return 0, nil, err
	}
	c.mergeSetCookies(resp)
	c.updateStateFromHeaders(resp)
	return resp.StatusCode, data, nil
}

// CreateAlias 通过新接口创建一个隐藏邮箱 (generate + complete 两步)。
//
// 内部先检查会话新鲜度, 过期时自动刷新; 若创建因会话失效失败, 会刷新后重试一次。
// 返回 CreateResult, 与旧接口 CreateAlias 返回值一致, 便于上层统一处理。
func (c *AppleAccountClient) CreateAlias(label string) (*CreateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.needsRefreshLocked(now) {
		c.log("会话需要刷新, 先刷新...")
		var err error
		if strings.TrimSpace(c.state.Scnt) == "" {
			err = c.bootstrapLocked()
		} else {
			err = c.refreshLocked()
		}
		if err != nil {
			return nil, fmt.Errorf("%w: 会话刷新失败: %s", ErrAppleAccountAuthFailed, err.Error())
		}
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Created " + time.Now().Format("2006-01-02 15:04")
	}
	note := "Created by icloud_hme tool"

	result, err := c.createAliasLocked(label, note)
	if err == nil || !errors.Is(err, ErrAppleAccountAuthFailed) {
		return result, err
	}
	// 创建中途会话失效 (scnt 轮换/过期) → 刷新后重试一次
	c.log("创建时会话失效, 刷新后重试...")
	if refreshErr := c.refreshLocked(); refreshErr != nil {
		return nil, err
	}
	return c.createAliasLocked(label, note)
}

// createAliasLocked 执行 add → complete → confirm 三步, 调用方需持有锁。
func (c *AppleAccountClient) createAliasLocked(label, note string) (*CreateResult, error) {
	// 1. 生成候选地址
	stage := "生成候选隐私邮箱"
	status, raw, err := c.callAPI(http.MethodPost, "/account/manage/email/private/add", map[string]any{})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, c.appleError(stage, status, raw)
	}
	var generated struct {
		EmailAddress string `json:"emailAddress"`
	}
	if json.Unmarshal(raw, &generated) != nil || strings.TrimSpace(generated.EmailAddress) == "" {
		return nil, fmt.Errorf("%s失败: Apple Account 未返回候选隐私邮箱 (%s)", stage, appleResponseSnippet(raw))
	}

	// 2. 确认创建
	stage = "确认创建隐私邮箱"
	completeBody := map[string]string{
		"emailAddress": generated.EmailAddress,
		"label":        label,
		"note":         note,
	}
	status, raw, err = c.callAPI(http.MethodPut, "/account/manage/email/private/add/complete", completeBody)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, c.appleError(stage, status, raw)
	}
	var completed struct {
		EmailAddress string `json:"emailAddress"`
		Label        string `json:"label"`
		ID           string `json:"id"`
		Active       bool   `json:"active"`
	}
	if err := json.Unmarshal(raw, &completed); err != nil {
		return nil, fmt.Errorf("%s失败: Apple Account 返回无法解析 (%s)", stage, appleResponseSnippet(raw))
	}

	result := &CreateResult{
		Email:     strings.ToLower(strings.TrimSpace(firstNonEmpty(completed.EmailAddress, generated.EmailAddress))),
		Label:     firstNonEmpty(completed.Label, label),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// 3. 确认详情 (尽力而为, 失败不影响创建结果)
	if strings.TrimSpace(completed.ID) != "" {
		var confirmed struct {
			EmailAddress string `json:"emailAddress"`
			Label        string `json:"label"`
		}
		confirmPath := "/account/manage/email/private/" + url.PathEscape(completed.ID) + ".em"
		status, raw, confirmErr := c.callAPI(http.MethodGet, confirmPath, nil)
		if confirmErr == nil && status >= 200 && status < 300 {
			if json.Unmarshal(raw, &confirmed) == nil {
				if v := strings.TrimSpace(confirmed.EmailAddress); v != "" {
					result.Email = strings.ToLower(v)
				}
				if v := strings.TrimSpace(confirmed.Label); v != "" {
					result.Label = v
				}
			}
		}
	}

	if result.Email == "" {
		return nil, fmt.Errorf("确认创建隐私邮箱失败: Apple Account 创建后未返回隐私邮箱")
	}
	c.markOK()
	c.log("已通过新接口创建: %s", result.Email)
	return result, nil
}

// KeepAlive 保活 Apple Account 管理会话。
//
// 内部: 刷新会话 (scnt/apiKey) → GET /account/manage/forwardemail (失败即返回错误)
// → best-effort POST /v2/jslogs (失败忽略) → 标记会话检查成功。
// 调用方可通过 NeedsKeepAlive 判断是否到期。
func (c *AppleAccountClient) KeepAlive() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log("保活 Apple Account 管理会话...")
	if err := c.refreshLocked(); err != nil {
		return err
	}
	if status, data, err := c.callAPI(http.MethodGet, appleAccountKeepAliveForwardEmailPath, nil); err != nil {
		return err
	} else if status < 200 || status >= 300 {
		return c.appleError("保活检查转发邮箱", status, data)
	}
	if _, _, err := c.callAPI(http.MethodPost, appleAccountKeepAliveJSLogsPath, appleAccountJSLogBody()); err != nil && appleAccountDebugEnabled() {
		fmt.Fprintf(os.Stderr, "APPLE_ACCOUNT_DEBUG keepalive jslogs ignored err=%v\n", err)
	}
	c.markOK()
	return nil
}

// NeedsKeepAlive 判断是否需要进行会话保活。
//
// session 未就绪 (缺少 scnt/apiKey) 返回 false; LastCheckedAt 为 0 返回 true;
// 否则 now 距 LastCheckedAt 超过 interval 时返回 true; LastCheckOK 为 false 时返回 true。
func (c *AppleAccountClient) NeedsKeepAlive(interval time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.state.Scnt) == "" || strings.TrimSpace(c.state.APIKey) == "" {
		return false
	}
	if c.state.LastCheckedAt.IsZero() {
		return true
	}
	if !c.state.LastCheckOK {
		return true
	}
	if interval <= 0 {
		interval = appleAccountKeepAliveDefaultInterval
	}
	return !now.Before(c.state.LastCheckedAt.Add(interval))
}

// appleAccountJSLogBody 生成 /v2/jslogs 心跳请求体 (eventId 使用随机 UUID)。
func appleAccountJSLogBody() []map[string]any {
	return []map[string]any{{
		"eventId":       uuid.NewString(),
		"timestamp":     time.Now().UnixMilli(),
		"eventType":     "custom",
		"componentName": "performance",
		"action":        "memory",
		"metadata": map[string]any{
			"domNodeCount":    0,
			"usedJSHeapSize":  0,
			"totalJSHeapSize": 0,
			"heapUtilization": 0,
			"createdAt":       0,
			"elapsedTime":     0,
			"marks": map[string]any{
				"startTime": 0,
			},
			"eventVersion": 1,
		},
	}}
}

// appleError 将 Apple Account API 错误分类为可识别错误:
//   - 限速 → *AliasRateLimitError (Is ErrAliasRateLimited)
//   - 总容量 → ErrAliasLimitReached
//   - 会话失效 → ErrAppleAccountAuthFailed
//   - 其余 → 普通错误
func (c *AppleAccountClient) appleError(stage string, status int, data []byte) error {
	detail := fmt.Sprintf("%s失败: HTTP %d (%s)", stage, status, appleResponseSnippet(data))
	lower := strings.ToLower(strings.TrimSpace(appleResponseSnippet(data)))

	// 总容量 (文案最具体, 优先判定)
	if strings.Contains(lower, "maximum number of address") ||
		strings.Contains(lower, "maximum address") ||
		strings.Contains(lower, "address capacity") ||
		strings.Contains(lower, "total address") ||
		strings.Contains(lower, "capacity reached") ||
		strings.Contains(lower, "hide my email address limit") {
		return fmt.Errorf("%w: %s", ErrAliasLimitReached, detail)
	}

	// 限速
	retryAfter := parseAppleRetryAfter(data)
	if retryAfter > 0 || strings.Contains(lower, "limit") || strings.Contains(lower, "too many") || strings.Contains(lower, "rate") {
		return &AliasRateLimitError{RetryAfter: retryAfter, Message: detail}
	}

	// 会话失效
	if status == appleAccountHTTPStatusSessionTimeout ||
		(status == http.StatusUnauthorized || status == http.StatusForbidden) && appleAccountBodyLooksAuthExpired(lower) {
		return fmt.Errorf("%w: %s", ErrAppleAccountAuthFailed, detail)
	}
	return errors.New(detail)
}

func parseAppleRetryAfter(data []byte) time.Duration {
	if !json.Valid(data) {
		return 0
	}
	return parseRetryAfter(gjson.Parse(string(data)))
}

func appleAccountBodyLooksAuthExpired(lower string) bool {
	lower = strings.ToLower(strings.TrimSpace(lower))
	if lower == "" {
		return false
	}
	authTokens := []string{
		"authentication_failed",
		"authentication failed",
		"auth_failed",
		"auth failed",
		"gsa_invalid_session",
		"invalid session",
		"scnt_expired",
		"session expired",
		"session has expired",
	}
	for _, token := range authTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return strings.Contains(lower, "authentication") && (strings.Contains(lower, "failed") || strings.Contains(lower, "expired"))
}

func appleResponseSnippet(data []byte) string {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "空响应"
	}
	if len(trimmed) > 240 {
		return trimmed[:240] + "..."
	}
	return trimmed
}

// cookieHeader 将状态中的 Cookie 序列化为请求头 (浏览器采集的会话 Cookie 全量携带)。
func (c *AppleAccountClient) cookieHeader(rawURL string) string {
	if len(c.state.Cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.state.Cookies))
	// 稳定顺序, 保证可测试
	names := make([]string, 0, len(c.state.Cookies))
	for name := range c.state.Cookies {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		value := c.state.Cookies[name]
		if name == "" || value == "" {
			continue
		}
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// mergeSetCookies 将响应 Set-Cookie 合并进会话 (按名称覆盖, 模拟浏览器行为)。
func (c *AppleAccountClient) mergeSetCookies(resp *http.Response) {
	for _, sc := range resp.Cookies() {
		if sc.Name != "" && sc.Value != "" {
			c.state.Cookies[sc.Name] = sc.Value
		}
	}
}

// updateStateFromHeaders 从响应头捕获 scnt / session id。
func (c *AppleAccountClient) updateStateFromHeaders(resp *http.Response) {
	if v := strings.TrimSpace(resp.Header.Get("scnt")); v != "" {
		c.state.Scnt = v
	}
	if v := strings.TrimSpace(resp.Header.Get("X-Apple-ID-Session-Id")); v != "" {
		c.state.SessionID = v
	}
}

// ---- 瞬态网络错误重试 ----

// retryAppleAccountTransient 对瞬态网络错误做最多 appleAccountTransientRetries 次重试,
// 退避 800ms/1.6s/2.4s。HTTP 非 2xx 状态错误与已分类业务错误立即返回, 不重试。
func retryAppleAccountTransient(fn func() error) error {
	var last error
	for attempt := 0; attempt < appleAccountTransientRetries; attempt++ {
		if err := fn(); err != nil {
			if !isAppleAccountTransientNetworkError(err) {
				return err
			}
			last = err
			if attempt == appleAccountTransientRetries-1 {
				break
			}
			timer := time.NewTimer(appleAccountTransientBackoff * time.Duration(attempt+1))
			<-timer.C
			continue
		}
		return nil
	}
	return last
}

// isAppleAccountTransientNetworkError 判断错误是否为可重试的瞬态网络错误。
func isAppleAccountTransientNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"eof",
		"timeout",
		"i/o timeout",
		"no such host",
		"temporary failure in name resolution",
		"network is unreachable",
		"no route to host",
		"connection reset",
		"connection refused",
		"connection aborted",
		"forcibly closed",
		"server closed idle connection",
		"tls handshake timeout",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// ---- 脱敏调试日志 ----

// doAppleAccountRequest 执行单次 HTTP 请求并统一输出脱敏调试日志。
func (c *AppleAccountClient) doAppleAccountRequest(req *http.Request, method, path string) (*http.Response, []byte, error) {
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	c.debugRequest(req, method, path, resp, data)
	return resp, data, nil
}

// appleAccountDebugEnabled 是否开启脱敏调试日志
// (CYMAIL_DEBUG_APPLE_ACCOUNT=1 或 IPM_DEBUG_APPLE_ACCOUNT=1)。
func appleAccountDebugEnabled() bool {
	return os.Getenv("CYMAIL_DEBUG_APPLE_ACCOUNT") == "1" || os.Getenv("IPM_DEBUG_APPLE_ACCOUNT") == "1"
}

// debugRequest 对每个 HTTP 请求输出一行脱敏调试日志。
// 仅打印指纹与响应体摘要, 严禁打印完整 Cookie / scnt / apiKey / 密码。
func (c *AppleAccountClient) debugRequest(req *http.Request, method, path string, resp *http.Response, data []byte) {
	if !appleAccountDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr,
		"APPLE_ACCOUNT_DEBUG method=%s path=%s status=%d req_cookies=%d req_scnt=%s res_scnt=%s session_id=%s set_cookie=%d body=%q\n",
		method,
		path,
		resp.StatusCode,
		len(c.state.Cookies),
		appleAccountDebugFingerprint(req.Header.Get("scnt")),
		appleAccountDebugFingerprint(resp.Header.Get("scnt")),
		appleAccountDebugFingerprint(resp.Header.Get("X-Apple-ID-Session-Id")),
		len(resp.Cookies()),
		appleAccountDebugBody(data),
	)
}

// appleAccountDebugFingerprint 生成敏感值指纹: sha1 前 4 字节 hex + 长度。
func appleAccountDebugFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	sum := sha1.Sum([]byte(value))
	return fmt.Sprintf("%x/%d", sum[:4], len(value))
}

// appleAccountDebugBody 生成响应体摘要 (≤200 字符)。
// JSON 响应优先压缩并对敏感字段脱敏; 非 JSON 直接截断。
func appleAccountDebugBody(data []byte) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(trimmed, &value) == nil {
		redacted := redactAppleAccountDebugJSON(value)
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(redacted); err == nil {
			return appleAccountDebugTrim(bytes.TrimSpace(buf.Bytes()))
		}
	}
	return appleAccountDebugTrim(trimmed)
}

func appleAccountDebugTrim(text []byte) string {
	if len(text) > 200 {
		return string(text[:200]) + "..."
	}
	return string(text)
}

func redactAppleAccountDebugJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if appleAccountDebugSecretKey(key) {
				out[key] = "<redacted>"
				continue
			}
			out[key] = redactAppleAccountDebugJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactAppleAccountDebugJSON(item)
		}
		return out
	default:
		return value
	}
}

func appleAccountDebugSecretKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"apikey", "api_key", "token", "secret", "password", "scnt", "session", "email", "accountname", "appleid", "dsid"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

// ---- X-Apple-I-FD-Client-Info 指纹 (移植自 iCloud-Privacy-Mail 参考实现) ----

func appleAccountFDClientInfo(userAgent string) string {
	info := map[string]string{
		"U": firstNonEmpty(userAgent, appleAccountManageUserAgent),
		"L": appleAccountManageLanguage,
		"Z": appleAccountManageGMTOffset,
		"V": "1.1",
		"F": appleAccountCompressedFingerprint(time.Now()),
	}
	data, _ := json.Marshal(info)
	return string(data)
}

func appleAccountCompressedFingerprint(now time.Time) string {
	raw := appleAccountFingerprintPayload(now.In(time.FixedZone("apple-account", appleAccountManageTZOffset)))
	replaced := raw
	for idx, token := range appleAccountFingerprintDictionary {
		replaced = strings.ReplaceAll(replaced, token, string(rune(idx+1)))
	}
	encoded, ok := appleAccountFingerprintHuffman(replaced)
	if !ok {
		return raw
	}
	checksum := 65535
	for _, b := range []byte(raw) {
		checksum = ((checksum >> 8) | (checksum << 8)) & 0xffff
		checksum ^= int(b) & 0xff
		checksum ^= (checksum & 0xff) >> 4
		checksum ^= (checksum << 12) & 0xffff
		checksum ^= ((checksum & 0xff) << 5) & 0xffff
	}
	return encoded +
		string(appleAccountFingerprintAlphabet[(checksum>>12)&63]) +
		string(appleAccountFingerprintAlphabet[(checksum>>6)&63]) +
		string(appleAccountFingerprintAlphabet[checksum&63])
}

func appleAccountFingerprintPayload(now time.Time) string {
	values := []string{
		"TF1", "020",
	}
	for i := 0; i < 39; i++ {
		values = append(values, "")
	}
	values = append(values,
		"true",
		"true",
		strconv.FormatInt(now.UnixMilli(), 10),
		"-6",
		"6/7/2005, 9:33:44 PM",
		"", "", "", "", "", "",
		strconv.FormatInt(now.UnixMilli(), 10),
		"0",
		appleAccountUSLocaleString(now),
	)
	for i := 0; i < 34; i++ {
		values = append(values, "")
	}
	values = append(values, "5.6.1-0", "")

	var b strings.Builder
	for _, value := range values {
		b.WriteString(appleAccountJSEscape(value))
		b.WriteByte(';')
	}
	return b.String()
}

func appleAccountUSLocaleString(t time.Time) string {
	hour := t.Hour()
	ampm := "AM"
	if hour >= 12 {
		ampm = "PM"
	}
	hour12 := hour % 12
	if hour12 == 0 {
		hour12 = 12
	}
	return fmt.Sprintf("%d/%d/%d, %d:%02d:%02d %s", int(t.Month()), t.Day(), t.Year(), hour12, t.Minute(), t.Second(), ampm)
}

func appleAccountJSEscape(value string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '@' || r == '*' || r == '_' || r == '+' || r == '-' || r == '.' || r == '/' {
			b.WriteRune(r)
			continue
		}
		if r <= 0xff {
			b.WriteByte('%')
			b.WriteByte(hex[(r>>4)&0xf])
			b.WriteByte(hex[r&0xf])
			continue
		}
		b.WriteString("%u")
		b.WriteByte(hex[(r>>12)&0xf])
		b.WriteByte(hex[(r>>8)&0xf])
		b.WriteByte(hex[(r>>4)&0xf])
		b.WriteByte(hex[r&0xf])
	}
	return b.String()
}

func appleAccountFingerprintHuffman(value string) (string, bool) {
	var b strings.Builder
	bitBuffer := 0
	bitCount := 0
	push := func(width, code int) {
		bitBuffer = (bitBuffer << width) | code
		bitCount += width
		for bitCount >= 6 {
			idx := (bitBuffer >> (bitCount - 6)) & 63
			b.WriteByte(appleAccountFingerprintAlphabet[idx])
			bitCount -= 6
			bitBuffer ^= idx << bitCount
		}
	}
	push(6, (len(value)&7)<<3)
	push(6, (len(value)&56)|1)
	for _, r := range value {
		code, ok := appleAccountFingerprintCodes[int(r)]
		if !ok {
			return "", false
		}
		push(code.width, code.value)
	}
	code := appleAccountFingerprintCodes[0]
	push(code.width, code.value)
	if bitCount > 0 {
		push(6-bitCount, 0)
	}
	return b.String(), true
}

type appleAccountFingerprintCode struct {
	width int
	value int
}

var appleAccountFingerprintDictionary = []string{
	"%20", ";;;", "%3B", "%2C", "und", "fin", "ed;", "%28", "%29", "%3A", "/53", "ike", "Web", "0;", ".0", "e;", "on", "il", "ck", "01", "in", "Mo", "fa", "00", "32", "la", ".1", "ri", "it", "%u", "le",
}

const appleAccountFingerprintAlphabet = ".0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"

var appleAccountFingerprintCodes = map[int]appleAccountFingerprintCode{
	1: {4, 15}, 110: {8, 239}, 74: {8, 238}, 57: {7, 118}, 56: {7, 117}, 71: {8, 233},
	25: {8, 232}, 101: {5, 28}, 104: {7, 111}, 4: {7, 110}, 105: {6, 54}, 5: {7, 107},
	109: {7, 106}, 103: {9, 423}, 82: {9, 422}, 26: {8, 210}, 6: {7, 104}, 46: {6, 51},
	97: {6, 50}, 111: {6, 49}, 7: {7, 97}, 45: {7, 96}, 59: {5, 23}, 15: {7, 91},
	11: {8, 181}, 72: {8, 180}, 27: {8, 179}, 28: {8, 178}, 16: {7, 88}, 88: {10, 703},
	113: {11, 1405}, 89: {12, 2809}, 107: {13, 5617}, 90: {14, 11233}, 42: {15, 22465},
	64: {16, 44929}, 0: {16, 44928}, 81: {9, 350}, 29: {8, 174}, 118: {8, 173}, 30: {8, 172},
	98: {8, 171}, 12: {8, 170}, 99: {7, 84}, 117: {6, 41}, 112: {6, 40}, 102: {9, 319},
	68: {9, 318}, 31: {8, 158}, 100: {7, 78}, 84: {6, 38}, 55: {6, 37}, 17: {7, 73},
	8: {7, 72}, 9: {7, 71}, 77: {7, 70}, 18: {7, 69}, 65: {7, 68}, 48: {6, 33},
	116: {6, 32}, 10: {7, 63}, 121: {8, 125}, 78: {8, 124}, 80: {7, 61}, 69: {7, 60},
	119: {7, 59}, 13: {8, 117}, 79: {8, 116}, 19: {7, 57}, 67: {7, 56}, 114: {6, 27},
	83: {6, 26}, 115: {6, 25}, 14: {6, 24}, 122: {8, 95}, 95: {8, 94}, 76: {7, 46},
	24: {7, 45}, 37: {7, 44}, 50: {5, 10}, 51: {5, 9}, 108: {6, 17}, 22: {7, 33},
	120: {8, 65}, 66: {8, 64}, 21: {7, 31}, 106: {7, 30}, 47: {6, 14}, 53: {5, 6},
	49: {5, 5}, 86: {8, 39}, 85: {8, 38}, 23: {7, 18}, 75: {7, 17}, 20: {7, 16},
	2: {5, 3}, 73: {8, 23}, 43: {9, 45}, 87: {9, 44}, 70: {7, 10}, 3: {6, 4},
	52: {5, 1}, 54: {5, 0},
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
