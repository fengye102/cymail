// Package hme 实现了 iCloud Hide My Email 协议客户端。
//
// 基于 Cookie 会话,通过 tls-client 伪装 Chrome TLS 指纹规避 iCloud 风控。
// 对应原 Python 项目 icloud_hme.py 的 ICloudHME 类。
package hme

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// ManagedCapacityTarget is CYMail's requested management target. It is
	// not an assertion about an Apple account quota; Apple remains the source
	// of truth for whether another address may be created.
	ManagedCapacityTarget = 750
	RandomLabelLength     = 10
	// ClientBuildNumber 是 iCloud Web 客户端构建号,从浏览器抓包获取。
	// maildomainws (HME 别名管理) 专用。
	ClientBuildNumber = "2624Build22"
	// ClientMasteringNumber 是 iCloud Web 客户端主版本号。
	ClientMasteringNumber = "2624Build22"
	// DefaultBuildNumber 用于 validate 和 mccgateway (邮件) 等非 HME 端点。
	DefaultBuildNumber = "2624Build13"
	// RequestTimeout 单次请求超时。
	RequestTimeout = 15 * time.Second
	// MaxRetries 最大重试次数。
	MaxRetries = 3
)

var ErrAliasLimitReached = errors.New("iCloud Hide My Email address capacity reached")
var ErrAliasRateLimited = errors.New("iCloud Hide My Email creation temporarily rate limited")
var ErrTrustSessionRequired = errors.New("iCloud trusted web session must be refreshed")

// AliasRateLimitError preserves Apple's retry hint without exposing the raw
// response body, which can contain session-related fields.
type AliasRateLimitError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *AliasRateLimitError) Error() string {
	if e == nil {
		return ErrAliasRateLimited.Error()
	}
	if e.Message != "" {
		return ErrAliasRateLimited.Error() + ": " + e.Message
	}
	return ErrAliasRateLimited.Error()
}

func (e *AliasRateLimitError) Unwrap() error { return ErrAliasRateLimited }

// AliasRetryAfter returns the retry delay carried by an Apple rate-limit
// response. A zero duration means that Apple did not provide a usable hint.
func AliasRetryAfter(err error) time.Duration {
	var rateErr *AliasRateLimitError
	if errors.As(err, &rateErr) && rateErr.RetryAfter > 0 {
		return rateErr.RetryAfter
	}
	return 0
}

const randomLabelAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var retryDelays = []time.Duration{
	1 * time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
}

// AccountInfo 是从 /validate 响应中提取的账号身份信息。
type AccountInfo struct {
	DSID             string `json:"dsid"`
	AppleID          string `json:"appleId"`
	PrimaryEmail     string `json:"primaryEmail"`
	FullName         string `json:"fullName"`
	IsManagedAppleID bool   `json:"isManagedAppleId"`
}

// Alias 是一个 Hide My Email 隐私邮箱别名。
type Alias struct {
	Email          string `json:"email"`
	AnonymousID    string `json:"anonymousId"`
	Label          string `json:"label"`
	Active         bool   `json:"active"`
	CreatedAt      string `json:"createdAt,omitempty"`
	ForwardToEmail string `json:"forwardToEmail,omitempty"`
}

// ForwardingSettings is the complete Hide My Email forwarding snapshot.
type ForwardingSettings struct {
	Aliases           []Alias  `json:"aliases"`
	SelectedForwardTo string   `json:"selected_forward_to"`
	ForwardToEmails   []string `json:"forward_to_emails"`
}

// Client 是 iCloud Hide My Email 客户端。
//
// 一个 Client 对应一个 iCloud 账号。通过传入的 Cookie 维持会话,
// 首次调用业务方法时会自动触发 ValidateSession 解析 HME 服务端点。
type Client struct {
	Cookies     map[string]string
	Host        string // "icloud.com" 或 "icloud.com.cn"
	Proxy       string // HTTP/SOCKS5 代理
	Username    string // iCloud 账号 (用于登录)
	Password    string // iCloud 密码 (用于登录)
	Verbose     bool
	httpc       tls_client.HttpClient
	setupURL    string
	serviceURL  string
	dsid        string // 从 validate 响应提取
	clientID    string // UUID,每次会话生成
	accountInfo *AccountInfo
}

// NewClient 创建一个新的 HME 客户端,底层使用 Chrome TLS 指纹。
//
// proxy 支持格式:
//   - HTTP:  "http://user:pass@host:port"
//   - SOCKS5: "socks5://user:pass@host:port"
func NewClient(cookies map[string]string, host, proxy string, verbose bool) (*Client, error) {
	if host == "" {
		host = "icloud.com"
	}
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
	}

	// 添加代理支持
	if proxy != "" {
		options = append(options, tls_client.WithProxyUrl(proxy))
	}

	httpc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}

	c := &Client{
		Cookies:  cookies,
		Host:     normalizeHost(host),
		Proxy:    proxy,
		Verbose:  verbose,
		httpc:    httpc,
		clientID: uuid.New().String(),
	}

	// 把传入的 Cookie 灌入 jar,后续请求自动携带。
	if len(cookies) > 0 {
		// 设置 Cookie 到所有可能的域名
		domains := []string{
			"https://www.icloud.com",
			"https://www.icloud.com.cn",
			"https://setup.icloud.com",
			"https://setup.icloud.com.cn",
			"https://" + c.Host,
		}

		// 添加 serviceURL 的域名（如果已知）
		if c.serviceURL != "" {
			if u, err := url.Parse(c.serviceURL); err == nil {
				domains = append(domains, u.Scheme+"://"+u.Host)
			}
		}

		for _, domain := range domains {
			u, _ := url.Parse(domain)
			httpCookies := make([]*http.Cookie, 0, len(cookies))
			for k, v := range cookies {
				httpCookies = append(httpCookies, &http.Cookie{
					Name:  k,
					Value: v,
					Path:  "/",
				})
			}
			jar.SetCookies(u, httpCookies)
		}
	}
	return c, nil
}

func normalizeHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
		h = u.Hostname()
	} else if !strings.Contains(h, "://") {
		if u, err := url.Parse("https://" + h); err == nil && u.Hostname() != "" {
			h = u.Hostname()
		}
	}
	if strings.HasSuffix(h, ".icloud.com.cn") || h == "icloud.com.cn" {
		return "icloud.com.cn"
	}
	return "icloud.com"
}

// SetupURL 返回 iCloud setup 端点。
func (c *Client) SetupURL() string {
	if c.setupURL == "" {
		suffix := "setup.icloud.com"
		if c.Host == "icloud.com.cn" {
			suffix = "setup.icloud.com.cn"
		}
		c.setupURL = "https://" + suffix + "/setup/ws/1"
	}
	return c.setupURL
}

// Origin 返回 Web Origin。
func (c *Client) Origin() string {
	return "https://www." + c.Host
}

func (c *Client) log(format string, args ...any) {
	if c.Verbose {
		fmt.Printf("  [iCloud] %s\n", fmt.Sprintf(format, args...))
	}
}

// buildURL 给 URL 追加 clientBuildNumber / clientMasteringNumber / clientId / dsid 查询参数,
// 这是 iCloud Web API 的强制要求。
func (c *Client) buildURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	// setup.icloud.com (validate) 和 mccgateway 用 DefaultBuildNumber,maildomainws 用 ClientBuildNumber
	host := parsed.Hostname()
	if strings.Contains(host, "maildomainws") {
		q.Set("clientBuildNumber", ClientBuildNumber)
		q.Set("clientMasteringNumber", ClientMasteringNumber)
	} else {
		q.Set("clientBuildNumber", DefaultBuildNumber)
		q.Set("clientMasteringNumber", DefaultBuildNumber)
	}
	if c.clientID != "" {
		q.Set("clientId", c.clientID)
	}
	if c.dsid != "" {
		q.Set("dsid", c.dsid)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// request 执行带重试的 HTTP 请求,返回响应体字符串。
func (c *Client) request(method, rawURL string, body any, timeout time.Duration, maxAttempts int) (string, error) {
	if timeout == 0 {
		timeout = RequestTimeout
	}
	if maxAttempts == 0 {
		maxAttempts = MaxRetries
	}
	fullURL := c.buildURL(rawURL)

	hostName := ""
	if u, err := url.Parse(rawURL); err == nil {
		hostName = u.Hostname()
	}
	contentType := "application/json"
	acceptType := "application/json, text/plain, */*"
	if strings.Contains(hostName, "maildomainws") {
		contentType = "text/plain"
		acceptType = "*/*"
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var reqBody io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				return "", err
			}
			reqBody = bytes.NewReader(buf)
		}

		req, err := http.NewRequest(method, fullURL, reqBody)
		if err != nil {
			return "", err
		}
		req.Header.Set("Origin", c.Origin())
		req.Header.Set("Referer", c.Origin()+"/")
		req.Header.Set("Accept", acceptType)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("sec-ch-ua", `"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`)
		req.Header.Set("sec-ch-ua-mobile", "?0")
		req.Header.Set("sec-ch-ua-platform", `"Windows"`)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

		// 手动添加 Cookie 头（确保跨域也能传递）
		// 浏览器发送的 Cookie 值带双引号,iCloud 严格匹配
		if len(c.Cookies) > 0 {
			cookieParts := make([]string, 0, len(c.Cookies))
			for k, v := range c.Cookies {
				// iCloud stores its WebAuth values in cookie-shaped browser
				// fields, while setup and HME endpoints also require the same
				// values as request headers. Mirror only Apple's well-known
				// fields; ordinary browser cookies remain Cookie-only.
				if isAppleSessionHeader(k) {
					req.Header.Set(k, trimOuterQuotes(v))
				}
				if strings.HasPrefix(v, `"`) {
					cookieParts = append(cookieParts, k+"="+v)
				} else {
					cookieParts = append(cookieParts, k+`="`+v+`"`)
				}
			}
			cookieHeader := strings.Join(cookieParts, "; ")
			req.Header.Set("Cookie", cookieHeader)
			if c.Verbose {
				c.log(">>> URL: %s", fullURL)
				c.log(">>> Cookie count: %d", len(c.Cookies))
				for k, vv := range req.Header {
					if strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization") {
						continue
					}
					for _, v := range vv {
						c.log(">>> %s: %s", k, v[:min(100, len(v))])
					}
				}
			}
		}

		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("连接失败: %w", err)
			if attempt < maxAttempts {
				c.sleepRetry(attempt)
				continue
			}
			return "", lastErr
		}

		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 从 Set-Cookie 响应头更新 Cookie（模拟浏览器行为,iCloud 会刷新 token）
		for _, sc := range resp.Cookies() {
			if sc.Name != "" && sc.Value != "" {
				c.Cookies[sc.Name] = sc.Value
			}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusMisdirectedRequest {
				updated := c.applyTrustChallenge(string(text))
				lastErr = fmt.Errorf("%w: Apple 要求刷新网页信任状态", ErrTrustSessionRequired)
				if updated && attempt < maxAttempts {
					continue
				}
				return "", lastErr
			}
			lastErr = safeAppleHTTPError(resp.StatusCode, string(text))
			// 401/403 说明 Cookie 失效,不重试直接返回。
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return "", lastErr
			}
			if attempt < maxAttempts {
				c.sleepRetry(attempt)
				continue
			}
			return "", lastErr
		}

		return string(text), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("未知错误")
}

func isAppleSessionHeader(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(name, "X-APPLE-WEBAUTH-") ||
		name == "X-APPLE-DS-WEB-SESSION-TOKEN" ||
		name == "X-APPLE-UNIQUE-CLIENT-ID" ||
		name == "X-APPLE-GROUP"
}

func trimOuterQuotes(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func (c *Client) applyTrustChallenge(body string) bool {
	if !gjson.Valid(body) {
		return false
	}
	current := trimOuterQuotes(c.Cookies["X-APPLE-WEBAUTH-HSA-TRUST"])
	for _, item := range gjson.Get(body, "trustTokens").Array() {
		token := strings.TrimSpace(item.String())
		if !validTrustToken(token) || token == current {
			continue
		}
		c.Cookies["X-APPLE-WEBAUTH-HSA-TRUST"] = token
		return true
	}
	return false
}

func validTrustToken(token string) bool {
	if len(token) < 32 || len(token) > 8192 {
		return false
	}
	for _, r := range token {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("+/_=-", r) {
			continue
		}
		return false
	}
	return true
}

// HTTPStatusError 携带上游 HTTP 状态码的结构化错误。调用方应基于
// StatusCode 判断错误类别（如 401/403 会话失效），而不是对错误文本
// 做子串匹配。
type HTTPStatusError struct {
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, http.StatusText(e.StatusCode))
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}

// statusErrf 构造携带状态码的错误，文本格式由调用方保持与旧实现一致。
func statusErrf(code int, format string, args ...any) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: code, Message: fmt.Sprintf(format, args...)}
}

func safeAppleHTTPError(status int, body string) error {
	message := ""
	if gjson.Valid(body) {
		message = firstNonEmpty(
			gjson.Get(body, "error.errorMessage").String(),
			gjson.Get(body, "error.message").String(),
			gjson.Get(body, "error.reason").String(),
			gjson.Get(body, "message").String(),
		)
	}
	if message == "" {
		message = http.StatusText(status)
	}
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
	if len(message) > 200 {
		message = message[:200]
	}
	return &HTTPStatusError{StatusCode: status, Message: message}
}

func (c *Client) sleepRetry(attempt int) {
	idx := attempt - 1
	if idx >= len(retryDelays) {
		idx = len(retryDelays) - 1
	}
	time.Sleep(retryDelays[idx])
}

// ValidateSession 校验 iCloud 会话,解析 HME 服务端点和账号身份。
//
// 必须在调用 ListAliases / Generate / Reserve / Delete 之前完成。
// 失败通常意味着 Cookie 过期或未订阅 iCloud+。
func (c *Client) ValidateSession() error {
	c.log("校验 iCloud 会话...")
	c.log("使用的 Cookie 数量: %d", len(c.Cookies))
	if len(c.Cookies) > 0 {
		for k := range c.Cookies {
			c.log("Cookie: %s", k)
		}
	}

	body, err := c.request("POST", c.SetupURL()+"/validate", nil, 20*time.Second, MaxRetries)
	if err != nil {
		c.log("校验失败: %v", err)
		return err
	}
	if !gjson.Valid(body) {
		return fmt.Errorf("invalid JSON response")
	}
	data := gjson.Parse(body)
	serviceURL := data.Get("webservices.premiummailsettings.url").String()
	if serviceURL == "" {
		return fmt.Errorf(
			"iCloud 会话校验失败 — 可能原因:\n" +
				"  1. 未开通 iCloud+ 订阅 (Hide My Email 需要 iCloud+)\n" +
				"  2. Cookie 已过期,请在 Chrome 重新登录 icloud.com\n" +
				"  3. 网络问题",
		)
	}
	c.serviceURL = strings.TrimRight(serviceURL, "/")
	// 剥离 :443 端口——tls-client cookie jar 按无端口 host 存储 cookie,带端口会丢失 cookie → 401
	if strings.HasSuffix(c.serviceURL, ":443") {
		c.serviceURL = strings.TrimSuffix(c.serviceURL, ":443")
	}

	// 获取 serviceURL 后，再次设置 Cookie 到该域名
	if len(c.Cookies) > 0 {
		u, _ := url.Parse(c.serviceURL)
		httpCookies := make([]*http.Cookie, 0, len(c.Cookies))
		for k, v := range c.Cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:  k,
				Value: v,
				Path:  "/",
			})
		}
		c.httpc.GetCookies(u) // 触发 cookie jar 初始化
		// 注意：需要手动设置 cookie，但 tls-client 的 CookieJar 不支持直接设置
		// 我们需要在请求时手动添加 Cookie 头
	}

	dsInfo := data.Get("dsInfo")
	c.dsid = dsInfo.Get("dsid").String()
	info := &AccountInfo{
		DSID:             c.dsid,
		AppleID:          firstNonEmpty(dsInfo.Get("appleId").String(), dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleIdEmail").String()),
		PrimaryEmail:     firstNonEmpty(dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleId").String()),
		FullName:         firstNonEmpty(dsInfo.Get("fullName").String(), dsInfo.Get("name").String()),
		IsManagedAppleID: dsInfo.Get("isManagedAppleId").Bool(),
	}
	if info.AppleID == "" {
		for _, name := range []string{"aosappleid", "appleId", "dsid"} {
			if v, ok := c.Cookies[name]; ok && v != "" {
				info.AppleID = v
				break
			}
		}
	}
	c.accountInfo = info
	c.log("会话有效 → %s", nonEmpty(info.AppleID, "未知账号"))
	return nil
}

// AccountInfo 返回已校验的账号身份(校验前为 nil)。
func (c *Client) AccountInfo() *AccountInfo { return c.accountInfo }

func (c *Client) resolveService() error {
	if c.serviceURL == "" {
		return c.ValidateSession()
	}
	return nil
}

// ListAliases 列出当前账号所有 Hide My Email 别名。
func (c *Client) ListAliases() ([]Alias, error) {
	settings, err := c.ListForwardingSettings()
	if err != nil {
		return nil, err
	}
	return settings.Aliases, nil
}

// ListForwardingSettings returns aliases together with Apple's forwarding choices.
func (c *Client) ListForwardingSettings() (*ForwardingSettings, error) {
	if err := c.resolveService(); err != nil {
		return nil, err
	}
	c.log("获取别名列表...")
	body, err := c.request("GET", c.serviceURL+"/v2/hme/list", nil, 0, MaxRetries)
	if err != nil {
		return nil, err
	}
	settings := parseForwardingSettings(body)
	c.log("loaded %d aliases and %d forwarding addresses", len(settings.Aliases), len(settings.ForwardToEmails))
	return settings, nil
}

// UpdateForwardTo changes Apple's default forwarding destination.
func (c *Client) UpdateForwardTo(email string) error {
	if err := c.resolveService(); err != nil {
		return err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return fmt.Errorf("invalid forwarding email")
	}
	body, err := c.request("POST", c.serviceURL+"/v1/hme/updateForwardTo", map[string]string{"forwardToEmail": email}, 0, 2)
	if err != nil {
		return err
	}
	if !gjson.Get(body, "success").Bool() {
		return fmt.Errorf("update forwarding address failed: %s", nonEmpty(gjson.Get(body, "error.errorMessage").String(), "unknown"))
	}
	return nil
}

// NewRandomLabel creates a cryptographically random, case-insensitively
// unique label containing both ASCII letters and digits.
func NewRandomLabel(existing []Alias) (string, error) {
	used := make(map[string]struct{}, len(existing))
	for _, alias := range existing {
		if label := strings.ToLower(strings.TrimSpace(alias.Label)); label != "" {
			used[label] = struct{}{}
		}
	}
	for attempt := 0; attempt < 32; attempt++ {
		buf := make([]byte, RandomLabelLength)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate random label: %w", err)
		}
		hasLetter, hasDigit := false, false
		for i, value := range buf {
			ch := randomLabelAlphabet[int(value)%len(randomLabelAlphabet)]
			buf[i] = ch
			hasLetter = hasLetter || ch >= 'a' && ch <= 'z'
			hasDigit = hasDigit || ch >= '0' && ch <= '9'
		}
		if !hasLetter || !hasDigit {
			continue
		}
		label := string(buf)
		if _, exists := used[label]; !exists {
			return label, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique random label")
}

// ParseAliasCreatedAt accepts the timestamp shapes observed in Apple's HME
// responses (Unix seconds/milliseconds/microseconds/nanoseconds and ISO text).
func ParseAliasCreatedAt(value string) (time.Time, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, false
	}
	if number, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var parsed time.Time
		switch {
		case number >= 1_000_000_000_000_000_000:
			parsed = time.Unix(0, number)
		case number >= 1_000_000_000_000_000:
			parsed = time.Unix(0, number*int64(time.Microsecond))
		case number >= 1_000_000_000_000:
			parsed = time.UnixMilli(number)
		default:
			parsed = time.Unix(number, 0)
		}
		if year := parsed.UTC().Year(); year >= 1970 && year <= 3000 {
			return parsed.UTC(), true
		}
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func normalizeAliasCreatedAt(value string) string {
	parsed, ok := ParseAliasCreatedAt(value)
	if !ok {
		return ""
	}
	return parsed.Format(time.RFC3339)
}

func parseRetryAfter(parsed gjson.Result) time.Duration {
	for _, path := range []string{"error.retryAfter", "error.retry_after", "error.retryAfterSeconds", "retryAfter", "retry_after", "retryAfterSeconds"} {
		value := parsed.Get(path)
		if !value.Exists() {
			continue
		}
		raw := strings.TrimSpace(value.String())
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
			return duration
		}
		if retryAt, err := time.Parse(time.RFC3339, raw); err == nil {
			if duration := time.Until(retryAt); duration > 0 {
				return duration
			}
		}
	}
	return 0
}

func operationError(operation, body string) error {
	parsed := gjson.Parse(body)
	message := firstNonEmpty(
		parsed.Get("error.errorMessage").String(),
		parsed.Get("error.message").String(),
		parsed.Get("error.reason").String(),
		parsed.Get("message").String(),
		"unknown error",
	)
	code := strings.ToLower(firstNonEmpty(parsed.Get("error.errorCode").String(), parsed.Get("error.code").String()))
	combined := strings.ToLower(code + " " + message)
	retryAfter := parseRetryAfter(parsed)
	rateLimited := retryAfter > 0 ||
		strings.Contains(combined, "too many request") ||
		strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "rate_limit") ||
		strings.Contains(combined, "time limit") ||
		strings.Contains(combined, "time_limit") ||
		strings.Contains(combined, "throttl") ||
		strings.Contains(combined, "right now") ||
		strings.Contains(combined, "try again") ||
		strings.Contains(combined, "temporarily")
	if rateLimited {
		return &AliasRateLimitError{RetryAfter: retryAfter, Message: message}
	}

	// Only explicit total-address wording is a capacity error. Generic words
	// such as "limit" and "too many" describe Apple's temporary throttling too.
	capacityReached := strings.Contains(combined, "maximum number of address") ||
		strings.Contains(combined, "maximum address") ||
		strings.Contains(combined, "address capacity") ||
		strings.Contains(combined, "total address") ||
		strings.Contains(combined, "capacity reached") ||
		strings.Contains(combined, "hide my email address limit") ||
		strings.Contains(combined, "limit of hide my email address")
	if capacityReached {
		return fmt.Errorf("%w: %s", ErrAliasLimitReached, message)
	}
	return fmt.Errorf("%s failed: %s", operation, message)
}

// Generate 生成一个候选别名(尚未保留,需再调用 Reserve)。
func (c *Client) Generate() (string, error) {
	if err := c.resolveService(); err != nil {
		return "", err
	}
	c.log("生成候选别名...")
	body, err := c.request("POST", c.serviceURL+"/v1/hme/generate", map[string]string{"langCode": "en-us"}, 0, 2)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return "", operationError("generate", body)
	}
	hme := parsed.Get("result.hme").String()
	if hme == "" {
		// 某些响应把 hme 包在嵌套对象里
		hme = parsed.Get("result.hme.hme").String()
		if hme == "" {
			hme = parsed.Get("result.hme.email").String()
		}
	}
	c.log("候选: %s", hme)
	return hme, nil
}

// Reserve 保留/确认候选别名,使其正式生效。
func (c *Client) Reserve(hme, label string) (string, error) {
	if err := c.resolveService(); err != nil {
		return "", err
	}
	if label == "" {
		label = "Created " + time.Now().Format("2006-01-02 15:04")
	}
	c.log("保留别名 %s ...", hme)
	payload := map[string]string{
		"hme":   hme,
		"label": label,
		"note":  "Created by icloud_hme tool",
	}
	body, err := c.request("POST", c.serviceURL+"/v1/hme/reserve", payload, 0, 2)
	if err != nil {
		return "", err
	}
	parsed := gjson.Parse(body)
	if !parsed.Get("success").Bool() {
		return "", operationError("reserve", body)
	}
	alias := hme
	resultHme := parsed.Get("result.hme")
	if resultHme.IsObject() {
		if v := resultHme.Get("hme").String(); v != "" {
			alias = v
		}
	}
	c.log("已保留: %s", alias)
	return alias, nil
}

// CreateResult 是 CreateAlias 的返回结果。
type CreateResult struct {
	Email     string `json:"email"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
}

// CreateAlias 一步完成「生成 + 保留」,创建一个新别名。
//
// 由于 generate / reserve 偶发失败,内部会重试 maxRetries 次,
// 每次重试会重置 serviceURL 强制重新校验会话。
func (c *Client) CreateAlias(label string, maxRetries int) (*CreateResult, error) {
	if maxRetries <= 0 {
		maxRetries = 5
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			c.serviceURL = ""
			c.setupURL = ""
			c.log("重试 %d/%d ...", attempt+1, maxRetries)
		}
		hme, err := c.Generate()
		if err != nil {
			lastErr = err
			c.log("%s", lastErr.Error())
			if errors.Is(err, ErrAliasLimitReached) || errors.Is(err, ErrAliasRateLimited) {
				break
			}
			if attempt < maxRetries-1 {
				time.Sleep(time.Second)
				continue
			}
			break
		}
		email, err := c.Reserve(hme, label)
		if err != nil {
			lastErr = err
			c.log("reserve 失败: %s", lastErr.Error())
			if errors.Is(err, ErrAliasLimitReached) || errors.Is(err, ErrAliasRateLimited) {
				break
			}
			if attempt < maxRetries-1 {
				time.Sleep(time.Second)
				continue
			}
			break
		}
		return &CreateResult{
			Email:     email,
			Label:     label,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("创建别名失败: %w", lastErr)
	}
	return nil, fmt.Errorf("创建别名失败,已重试 %d 次", maxRetries)
}

// DeactivateHME 停用别名(可恢复)。
func (c *Client) DeactivateHME(anonymousID string) (bool, error) {
	if err := c.resolveService(); err != nil {
		return false, err
	}
	c.log("停用 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.request("POST", c.serviceURL+"/v1/hme/deactivate", payload, 0, 2)
	if err != nil {
		return false, err
	}
	if !gjson.Get(body, "success").Bool() {
		return false, operationError("deactivate", body)
	}
	return true, nil
}

// ReactivateHME 激活已停用的别名。
func (c *Client) ReactivateHME(anonymousID string) (bool, error) {
	if err := c.resolveService(); err != nil {
		return false, err
	}
	c.log("激活 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	body, err := c.request("POST", c.serviceURL+"/v1/hme/reactivate", payload, 0, 2)
	if err != nil {
		return false, err
	}
	if !gjson.Get(body, "success").Bool() {
		return false, operationError("reactivate", body)
	}
	return true, nil
}

// Delete 删除别名。若直接删除失败会先停用再删。
func (c *Client) Delete(anonymousID string) error {
	if err := c.resolveService(); err != nil {
		return err
	}
	c.log("删除 %s ...", anonymousID)
	payload := map[string]string{"anonymousId": anonymousID}
	doDelete := func() (string, error) {
		return c.request("POST", c.serviceURL+"/v1/hme/delete", payload, 0, 2)
	}
	body, err := doDelete()
	if err != nil || !gjson.Get(body, "success").Bool() {
		c.log("直接删除失败,尝试先停用...")
		deactivateBody, deactivateErr := c.request("POST", c.serviceURL+"/v1/hme/deactivate", payload, 0, 2)
		if deactivateErr != nil {
			return fmt.Errorf("deactivate before delete: %w", deactivateErr)
		}
		if !gjson.Get(deactivateBody, "success").Bool() {
			return operationError("deactivate before delete", deactivateBody)
		}
		body, err = doDelete()
		if err != nil {
			return err
		}
		if !gjson.Get(body, "success").Bool() {
			return operationError("delete", body)
		}
	}
	c.log("已删除")
	return nil
}

// ---- 别名列表解析 (对应 ICloudHME._parse_alias_list) ----

// parseAliasList 解析 iCloud 返回的别名列表 JSON。
// 容错:优先取 result.hmeEmails,找不到则递归查找第一个对象数组。
func parseAliasList(body string) []Alias {
	return parseForwardingSettings(body).Aliases
}

func parseForwardingSettings(body string) *ForwardingSettings {
	settings := &ForwardingSettings{Aliases: []Alias{}, ForwardToEmails: []string{}}
	if !gjson.Valid(body) {
		return settings
	}
	root := gjson.Parse(body)
	settings.SelectedForwardTo = strings.ToLower(strings.TrimSpace(root.Get("result.selectedForwardTo").String()))
	seenForward := make(map[string]bool)
	root.Get("result.forwardToEmails").ForEach(func(_, item gjson.Result) bool {
		email := item.String()
		if item.IsObject() {
			email = firstNonEmpty(item.Get("email").String(), item.Get("emailAddress").String(), item.Get("address").String())
		}
		email = strings.ToLower(strings.TrimSpace(email))
		if email != "" && strings.Contains(email, "@") && !seenForward[email] {
			seenForward[email] = true
			settings.ForwardToEmails = append(settings.ForwardToEmails, email)
		}
		return true
	})

	arr := root.Get("result.hmeEmails")
	if !arr.IsArray() {
		arr = findFirstDictArray(root)
	}
	if !arr.IsArray() {
		return settings
	}

	var aliases []Alias
	arr.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		meta := item.Get("metaData")
		if !meta.IsObject() {
			meta = item.Get("hmeMetadata")
		}
		if !meta.IsObject() {
			meta = item.Get("metadata")
		}
		email := strings.TrimSpace(strings.ToLower(firstNonEmpty(
			item.Get("hme").String(),
			item.Get("email").String(),
			item.Get("alias").String(),
			item.Get("address").String(),
			meta.Get("hme").String(),
		)))
		if email == "" || !strings.Contains(email, "@") {
			return true
		}
		state := strings.ToLower(firstNonEmpty(item.Get("state").String(), item.Get("status").String()))
		active := state != "inactive" && state != "deleted"
		if item.Get("active").Exists() {
			active = item.Get("active").Bool() && active
		}
		if item.Get("isActive").Exists() {
			active = item.Get("isActive").Bool() && active
		}
		aliases = append(aliases, Alias{
			Email:       email,
			AnonymousID: firstNonEmpty(item.Get("anonymousId").String(), item.Get("id").String()),
			Label:       firstNonEmpty(item.Get("label").String(), meta.Get("label").String()),
			Active:      active,
			CreatedAt: normalizeAliasCreatedAt(firstNonEmpty(
				item.Get("createTimestamp").String(), item.Get("createdAt").String(), item.Get("createdTimestamp").String(),
				meta.Get("createTimestamp").String(), meta.Get("createdAt").String(), meta.Get("createdTimestamp").String(),
			)),
			ForwardToEmail: strings.ToLower(strings.TrimSpace(firstNonEmpty(item.Get("forwardToEmail").String(), meta.Get("forwardToEmail").String(), settings.SelectedForwardTo))),
		})
		return true
	})

	// 活跃的排前面,再按邮箱字母序。
	sort.SliceStable(aliases, func(i, j int) bool {
		if aliases[i].Active != aliases[j].Active {
			return aliases[i].Active
		}
		return aliases[i].Email < aliases[j].Email
	})
	settings.Aliases = aliases
	if settings.SelectedForwardTo != "" && !seenForward[settings.SelectedForwardTo] {
		settings.ForwardToEmails = append(settings.ForwardToEmails, settings.SelectedForwardTo)
	}
	return settings
}

// findFirstDictArray 递归查找第一个「对象数组」。
func findFirstDictArray(v gjson.Result) gjson.Result {
	if v.IsArray() {
		if len(v.Array()) > 0 && v.Array()[0].IsObject() {
			return v
		}
	}
	if v.IsObject() {
		var found gjson.Result
		v.ForEach(func(_, val gjson.Result) bool {
			if r := findFirstDictArray(val); r.IsArray() && len(r.Array()) > 0 {
				found = r
				return false
			}
			return true
		})
		return found
	}
	return gjson.Result{}
}

// ---- 小工具 ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
