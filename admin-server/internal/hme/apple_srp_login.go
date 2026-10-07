// Package hme - Apple Account 管理接口密码登录 (SRP 协议)
//
// 移植自参考项目 apple_auth_client.go 的 StartAppleAccountManageLogin 流程:
//  1. 预热 account.apple.com 门户并捕获管理接口 scnt
//  2. idmsa 授权 / 设备挑战 / 提交账号
//  3. SRP 密码派生 (s2k / s2k_fo) 与签名
//  4. 需要 2FA 时通过 OTPProvider 获取 6 位验证码并提交
//  5. 换取管理接口 scnt + apiKey
//
// 登录成功后返回的 AppleAccountState 可直接传给 NewAppleAccountClient 使用。
package hme

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"

	"icloud-hme/internal/srp"
)

const (
	// appleAccountManageOAuthClientID 是 account.apple.com 登录 iframe 使用的 OAuth client id。
	appleAccountManageOAuthClientID = "af1139274f266b22b68c2a3e7ad932cb3c0bbe854e13a79af78dcc73136882c3"

	// appleHashcashMaxBits 是 Apple Account 动态验证 (hashcash) 允许的最大难度。
	appleHashcashMaxBits = 24
	// appleHashcashMaxAttempts 是 hashcash 搜索 counter 的上限。
	appleHashcashMaxAttempts = 1 << 24
	// appleSRPLoginRequestTimeout 单次请求超时。
	appleSRPLoginRequestTimeout = 30 * time.Second
)

// appleIDMSAAuthBaseURL 是 idmsa 认证 API 的 Base URL (含 /appleauth/auth 路径)。
// 定义为变量以便单测重定向到 mock 服务; 生产环境固定为 idmsa.apple.com/appleauth/auth。
var appleIDMSAAuthBaseURL = "https://idmsa.apple.com/appleauth/auth"

// appleAccountManagePortalBaseURL 是 account.apple.com 门户的 Base URL。
// 定义为变量以便单测重定向到 mock 服务; 生产环境固定为 account.apple.com。
var appleAccountManagePortalBaseURL = "https://account.apple.com"

// SetAppleSRPLoginBaseURLsForTesting 重定向 SRP 密码登录涉及的三组 Base URL
// (idmsa 认证 / account.apple.com 门户 / appleid 管理 API), 仅供测试注入 mock 服务。
// 返回恢复函数; 生产代码不应调用。
func SetAppleSRPLoginBaseURLsForTesting(idmsaBase, portalBase, manageBase string) func() {
	previousIDMSA := appleIDMSAAuthBaseURL
	previousPortal := appleAccountManagePortalBaseURL
	previousManage := appleAccountManageBaseURL
	appleIDMSAAuthBaseURL = strings.TrimRight(idmsaBase, "/")
	appleAccountManagePortalBaseURL = strings.TrimRight(portalBase, "/")
	appleAccountManageBaseURL = strings.TrimRight(manageBase, "/")
	return func() {
		appleIDMSAAuthBaseURL = previousIDMSA
		appleAccountManagePortalBaseURL = previousPortal
		appleAccountManageBaseURL = previousManage
	}
}

// AppleSRPLogin 使用 Apple ID 账号密码通过 SRP 协议登录 Apple Account 管理接口 (新接口)。
//
// 流程与参考项目 StartAppleAccountManageLogin 一致; 账号启用双重认证时,
// 会调用 otpProvider 获取 6 位验证码并自动提交 (同步阻塞等待回调)。
//
// 登录成功后返回的 AppleAccountState 已包含完整会话:
// Cookies (含 idmsa/appleid/account.apple.com 会话)、Scnt、APIKey (已预取)、
// SessionID、Origin=https://account.apple.com、UserAgent=appleAccountManageUserAgent、SavedAt,
// 可直接传给 NewAppleAccountClient(state, false) 使用。
func AppleSRPLogin(username, password string, otpProvider OTPProvider) (AppleAccountState, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" || strings.TrimSpace(password) == "" {
		return AppleAccountState{}, fmt.Errorf("缺少 Apple ID 或密码")
	}
	session := &appleSRPLoginSession{
		username:   username,
		frameID:    strings.ToLower(uuid.NewString()),
		clientID:   appleAccountManageOAuthClientID,
		userAgent:  appleAccountManageUserAgent,
		idmsaBase:  strings.TrimRight(appleIDMSAAuthBaseURL, "/"),
		portalBase: strings.TrimRight(appleAccountManagePortalBaseURL, "/"),
		manageBase: strings.TrimRight(appleAccountManageBaseURL, "/"),
		cookies:    make(map[string]string),
		httpc:      &http.Client{Timeout: appleSRPLoginRequestTimeout},
	}
	ctx := context.Background()

	// 1. 预热 account.apple.com 门户并捕获管理接口 scnt
	if err := session.primeAppleAccountManageState(ctx); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple Account 门户预热失败: %w", err)
	}
	// 2. idmsa 授权初始化 (捕获 hashcash 挑战头)
	if err := session.authStart(ctx); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple 登录初始化失败: %w", err)
	}
	// 3. 设备密钥挑战
	if err := session.authDeviceKeyChallenge(ctx); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple 设备挑战失败: %w", err)
	}
	// 4. 提交账号
	if err := session.authFederate(ctx); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple 提交账号失败: %w", err)
	}
	// 5. SRP 密码派生与签名 (可能触发 2FA)
	needs2FA, err := session.authSRP(ctx, password)
	if err != nil {
		return AppleAccountState{}, err
	}
	// 6. 2FA: 验证码 + 信任设备
	if needs2FA {
		if err := session.handleTwoFactor(ctx, otpProvider); err != nil {
			return AppleAccountState{}, err
		}
	}
	// 7. 换取管理接口 scnt + apiKey
	return session.finishAppleAccountManage(ctx)
}

// appleSRPLoginSession 保存一次密码登录过程中的状态。
type appleSRPLoginSession struct {
	username  string
	frameID   string
	clientID  string
	userAgent string

	idmsaBase  string // https://idmsa.apple.com/appleauth/auth
	portalBase string // https://account.apple.com
	manageBase string // https://appleid.apple.com

	cookies   map[string]string
	scnt      string
	sessionID string
	apiKey    string

	authAttrs   string
	hcBits      int
	hcChallenge string
	// complete 阶段使用的 hashcash 挑战 (来自 authStart 响应头, 防止被后续响应覆盖)。
	completeHCBits      int
	completeHCChallenge string

	accountCountry string
	httpc          *http.Client
}

func (s *appleSRPLoginSession) frameTag() string { return "auth-" + s.frameID }

// ---- 请求执行与 Cookie/响应头管理 ----

// appleSRPEndpoint 标记请求的目标端点组, 域切换时据此使用切换后的 Base URL。
type appleSRPEndpoint int

const (
	appleSRPEndpointIDMSA appleSRPEndpoint = iota
	appleSRPEndpointPortal
	appleSRPEndpointManage
)

func (s *appleSRPLoginSession) baseFor(endpoint appleSRPEndpoint) string {
	switch endpoint {
	case appleSRPEndpointPortal:
		return s.portalBase
	case appleSRPEndpointManage:
		return s.manageBase
	default:
		return s.idmsaBase
	}
}

// errAppleDomainSwitch 表示响应要求切换 iCloud 域, 端点已切换, 调用方应重试一次。
var errAppleDomainSwitch = errors.New("apple domain switch")

// do 发起一次请求; 若响应为 3xx 且带 domainToUse, 自动切换到对应域端点并重试一次。
func (s *appleSRPLoginSession) do(ctx context.Context, method string, endpoint appleSRPEndpoint, path string, headers map[string]string, body any, out any, allow409 bool) (int, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		status, data, err := s.doOnce(ctx, method, endpoint, path, headers, body, out, allow409)
		if !errors.Is(err, errAppleDomainSwitch) {
			return status, data, err
		}
	}
	return 0, nil, fmt.Errorf("Apple 登录域切换后重试仍失败")
}

func (s *appleSRPLoginSession) doOnce(ctx context.Context, method string, endpoint appleSRPEndpoint, path string, headers map[string]string, body any, out any, allow409 bool) (int, []byte, error) {
	rawURL := strings.TrimRight(s.baseFor(endpoint), "/") + path
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie := srpLoginCookieHeader(s.cookies); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Apple 登录网络错误: %w", err)
	}
	defer resp.Body.Close()
	s.mergeSetCookies(resp)
	s.updateStateFromHeaders(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("Apple 登录响应读取失败: %w", err)
	}
	if domainToUse, ok := parseAppleDomainRedirect(resp.StatusCode, data); ok {
		if s.switchDomain(domainToUse) {
			return resp.StatusCode, data, errAppleDomainSwitch
		}
		return resp.StatusCode, nil, fmt.Errorf("Apple 要求切换 iCloud 域, 但不支持的域: %s", domainToUse)
	}
	if resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, nil, fmt.Errorf("Apple ID 或密码错误, 或当前账号被限制登录")
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return resp.StatusCode, nil, fmt.Errorf("需要先在 appleid.apple.com 同意隐私条款")
	}
	// signin/complete 允许 409 (2FA) 与 401 (凭据无效) 由调用方按状态处理。
	if allow409 && (resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnauthorized) {
		return resp.StatusCode, data, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, fmt.Errorf("Apple 协议 HTTP %d: %s", resp.StatusCode, trimAppleBody(data))
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("Apple 协议返回无法解析")
		}
	}
	return resp.StatusCode, data, nil
}

// srpLoginCookieHeader 将 Cookie map 序列化为请求头 (按名称排序, 保证确定性)。
func srpLoginCookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || strings.TrimSpace(cookies[name]) == "" {
			continue
		}
		parts = append(parts, name+"="+cookies[name])
	}
	return strings.Join(parts, "; ")
}

// mergeSetCookies 将响应 Set-Cookie 合并进会话 (按名称覆盖, 模拟浏览器行为)。
func (s *appleSRPLoginSession) mergeSetCookies(resp *http.Response) {
	for _, sc := range resp.Cookies() {
		if sc.Name != "" && sc.Value != "" {
			s.cookies[sc.Name] = sc.Value
		}
	}
}

// updateStateFromHeaders 从响应头捕获 scnt / session id / hashcash 挑战等。
func (s *appleSRPLoginSession) updateStateFromHeaders(header http.Header) {
	if v := strings.TrimSpace(header.Get("scnt")); v != "" {
		s.scnt = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-ID-Session-Id")); v != "" {
		s.sessionID = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-Auth-Attributes")); v != "" {
		s.authAttrs = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-ID-Account-Country")); v != "" {
		s.accountCountry = v
	}
	if v := strings.TrimSpace(header.Get("X-Apple-HC-Bits")); v != "" {
		if bits, err := strconv.Atoi(v); err == nil && bits > 0 {
			s.hcBits = bits
		}
	}
	if v := strings.TrimSpace(header.Get("X-Apple-HC-Challenge")); v != "" {
		s.hcChallenge = v
	}
}

// ---- 域切换 (基本版: 解析 3xx 的 domainToUse 并自动切换端点) ----

// parseAppleDomainRedirect 解析 3xx 响应中的 domainToUse。
func parseAppleDomainRedirect(status int, data []byte) (string, bool) {
	if status < 300 || status >= 400 {
		return "", false
	}
	var payload struct {
		DomainToUse string `json:"domainToUse"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &payload); err != nil {
		return "", false
	}
	return strings.TrimSpace(payload.DomainToUse), strings.TrimSpace(payload.DomainToUse) != ""
}

// switchDomain 将三个端点 Base URL 在 .com 与 .com.cn 之间切换。
// 当前端点不含可切换的域 (例如测试 mock) 或已是目标域时返回 false。
func (s *appleSRPLoginSession) switchDomain(domainToUse string) bool {
	domainToUse = strings.ToLower(strings.TrimSpace(domainToUse))
	toCN := strings.Contains(domainToUse, "apple.com.cn") || strings.Contains(domainToUse, "icloud.com.cn")
	toUS := !toCN && (strings.Contains(domainToUse, "apple.com") || strings.Contains(domainToUse, "icloud.com"))
	if !toCN && !toUS {
		return false
	}
	current := s.idmsaBase + "|" + s.portalBase + "|" + s.manageBase
	hasCN := strings.Contains(current, "apple.com.cn")
	if toCN == hasCN {
		// 已处于目标域, 无需切换
		return false
	}
	from, to := "apple.com", "apple.com.cn"
	if !toCN {
		from, to = "apple.com.cn", "apple.com"
	}
	if !strings.Contains(current, from) {
		return false
	}
	s.idmsaBase = strings.Replace(s.idmsaBase, from, to, 1)
	s.portalBase = strings.Replace(s.portalBase, from, to, 1)
	s.manageBase = strings.Replace(s.manageBase, from, to, 1)
	return true
}

// ---- 登录流程各步骤 ----

// primeAppleAccountManageState 预热 account.apple.com 门户并捕获管理接口 scnt。
func (s *appleSRPLoginSession) primeAppleAccountManageState(ctx context.Context) error {
	// 1. 预热门户 HTML 页
	privacyAccept := "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointPortal, "/account/manage/section/privacy", s.portalHeaders(privacyAccept, false), nil, nil, false); err != nil {
		return err
	}
	// 2. bootstrap/portal (JSON, 带 X-Apple-I-* 指纹头)
	var portal struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointPortal, "/bootstrap/portal", s.portalHeaders("application/json, text/plain, */*", true), nil, &portal, false); err != nil {
		return err
	}
	// 3. gs/ws/token: 从响应头捕获 scnt (此时无 scnt 可带)
	var token struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointManage, "/account/manage/gs/ws/token", s.manageHeaders(false), nil, &token, false); err != nil {
		if strings.TrimSpace(s.scnt) == "" {
			return err
		}
	}
	return nil
}

// authStart 初始化 idmsa 授权 (iframe 页面), 并记住 complete 阶段的 hashcash 挑战。
func (s *appleSRPLoginSession) authStart(ctx context.Context) error {
	frameTag := s.frameTag()
	u, err := url.Parse(s.idmsaBase + "/authorize/signin")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("frame_id", frameTag)
	q.Set("skVersion", "7")
	q.Set("iframeId", frameTag)
	q.Set("client_id", s.clientID)
	q.Set("redirect_uri", s.portalBase)
	q.Set("response_type", "code")
	q.Set("response_mode", "web_message")
	q.Set("state", frameTag)
	q.Set("authVersion", "8.0.2")
	u.RawQuery = q.Encode()
	headers := map[string]string{
		"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Referer":        s.portalBase + "/",
		"Sec-Fetch-Dest": "iframe",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Site": "same-site",
	}
	applyAppleSRPLoginBrowserHints(headers)
	authPath := strings.TrimPrefix(u.RequestURI(), "/appleauth/auth")
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointIDMSA, authPath, headers, nil, nil, false); err != nil {
		return err
	}
	// 记住 complete 阶段使用的 hashcash 挑战 (来自 authStart 响应头 X-Apple-HC-*)。
	if s.completeHCBits <= 0 && s.completeHCChallenge == "" {
		if s.hcBits > 0 && strings.TrimSpace(s.hcChallenge) != "" {
			s.completeHCBits = s.hcBits
			s.completeHCChallenge = s.hcChallenge
		}
	}
	return nil
}

// authDeviceKeyChallenge 提交设备密钥挑战 (去掉 scnt/session 相关头)。
func (s *appleSRPLoginSession) authDeviceKeyChallenge(ctx context.Context) error {
	body := map[string]bool{"passkeyAutofill": false}
	headers := s.srpHeaders()
	delete(headers, "scnt")
	delete(headers, "X-Apple-ID-Session-Id")
	delete(headers, "X-Apple-App-Id")
	_, _, err := s.do(ctx, http.MethodPost, appleSRPEndpointIDMSA, "/verify/device/key/challenge", headers, body, nil, false)
	return err
}

// authFederate 提交账号名。
func (s *appleSRPLoginSession) authFederate(ctx context.Context) error {
	body := map[string]any{"accountName": s.username, "rememberMe": true}
	_, _, err := s.do(ctx, http.MethodPost, appleSRPEndpointIDMSA, "/federate?isRememberMeEnabled=true", s.srpHeaders(), body, nil, false)
	return err
}

// authSRP 执行 SRP 密码派生与签名。
// 返回 needs2FA=true 表示 signin/complete 返回 409, 需要二次验证。
func (s *appleSRPLoginSession) authSRP(ctx context.Context, password string) (bool, error) {
	params := srp.GetParams(2048)
	params.NoUserNameInX = true
	srpClient := srp.NewSRPClient(params, nil)

	var initResp struct {
		Iteration int    `json:"iteration"`
		Salt      string `json:"salt"`
		Protocol  string `json:"protocol"`
		B         string `json:"b"`
		C         string `json:"c"`
	}
	initBody := map[string]any{
		"a":           base64.StdEncoding.EncodeToString(srpClient.GetABytes()),
		"accountName": s.username,
		"protocols":   []string{"s2k", "s2k_fo"},
	}
	if _, _, err := s.do(ctx, http.MethodPost, appleSRPEndpointIDMSA, "/signin/init", s.srpHeaders(), initBody, &initResp, false); err != nil {
		return false, err
	}
	serverB, err := base64.StdEncoding.DecodeString(initResp.B)
	if err != nil {
		return false, fmt.Errorf("解析 Apple SRP B 失败: %w", err)
	}
	salt, err := base64.StdEncoding.DecodeString(initResp.Salt)
	if err != nil {
		return false, fmt.Errorf("解析 Apple SRP salt 失败: %w", err)
	}
	derived, err := deriveAppleSRPPassword(password, salt, initResp.Iteration, initResp.Protocol)
	if err != nil {
		return false, err
	}
	srpClient.ProcessClientChanllenge([]byte(s.username), derived, salt, serverB)

	completeBody := map[string]any{
		"accountName": s.username,
		"m1":          base64.StdEncoding.EncodeToString(srpClient.M1),
		"m2":          base64.StdEncoding.EncodeToString(srpClient.M2),
		"c":           initResp.C,
		"rememberMe":  true,
	}
	headers := s.srpHeaders()
	hc, err := generateAppleHashcash(s.completeHCBits, s.completeHCChallenge, time.Now())
	if err != nil {
		return false, err
	}
	headers["X-Apple-HC"] = hc
	status, _, err := s.do(ctx, http.MethodPost, appleSRPEndpointIDMSA, "/signin/complete?isRememberMeEnabled=true", headers, completeBody, nil, true)
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized {
		return false, fmt.Errorf("Apple ID 或密码错误, 请检查后重试")
	}
	return status == http.StatusConflict, nil
}

// deriveAppleSRPPassword 按 Apple SRP 协议派生密码密钥。
// 注意: s2k 使用原始 SHA-256 摘要, s2k_fo 使用摘要的十六进制字符串, 两者推导结果不同。
func deriveAppleSRPPassword(password string, salt []byte, iterations int, protocol string) ([]byte, error) {
	passHash := sha256.Sum256([]byte(password))
	var input []byte
	switch protocol {
	case "s2k":
		input = passHash[:]
	case "s2k_fo":
		input = []byte(hex.EncodeToString(passHash[:]))
	default:
		return nil, fmt.Errorf("不支持的 Apple SRP 协议 %q", protocol)
	}
	return pbkdf2.Key(input, salt, iterations, 32, sha256.New), nil
}

// generateAppleHashcash 生成 Apple Account 动态验证 (hashcash) token。
// 验证方法: 对 token 做 SHA-1, 前导零比特数 >= bits。
func generateAppleHashcash(bits int, challenge string, now time.Time) (string, error) {
	challenge = strings.TrimSpace(challenge)
	if bits <= 0 || challenge == "" {
		return "", fmt.Errorf("Apple Account 缺少动态验证挑战, 请重新登录")
	}
	if bits > appleHashcashMaxBits {
		return "", fmt.Errorf("Apple Account 动态验证难度过高, 请稍后重试")
	}
	prefix := fmt.Sprintf("1:%d:%s:%s::", bits, now.UTC().Format("20060102150405"), challenge)
	for counter := int64(0); counter < appleHashcashMaxAttempts; counter++ {
		value := prefix + strconv.FormatInt(counter, 36)
		sum := sha1.Sum([]byte(value))
		if leadingZeroBits(sum[:]) >= bits {
			return value, nil
		}
	}
	return "", fmt.Errorf("Apple Account 动态验证生成失败, 请稍后重试")
}

// leadingZeroBits 返回数据开头连续为 0 的比特数。
func leadingZeroBits(data []byte) int {
	total := 0
	for _, b := range data {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) != 0 {
				return total
			}
			total++
		}
	}
	return total
}

// handleTwoFactor 通过 otpProvider 获取验证码, 提交并信任设备。
func (s *appleSRPLoginSession) handleTwoFactor(ctx context.Context, otpProvider OTPProvider) error {
	if otpProvider == nil {
		// 显式请求 Apple 向受信任设备发送 2FA 验证码
		s.do(ctx, http.MethodGet, appleSRPEndpointIDMSA, "/verify/trusteddevice/challenge", s.twoFactorHeaders(), nil, nil, false)
		return fmt.Errorf("账号启用了双重认证, 需要提供 otpProvider 获取 6 位验证码")
	}
	code, err := otpProvider()
	if err != nil {
		return fmt.Errorf("获取 2FA 验证码失败: %w", err)
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return fmt.Errorf("2FA 验证码必须是 6 位")
	}
	body := map[string]any{"securityCode": map[string]string{"code": code}}
	status, _, err := s.do(ctx, http.MethodPost, appleSRPEndpointIDMSA, "/verify/trusteddevice/securitycode", s.twoFactorHeaders(), body, nil, false)
	if err != nil {
		if status >= 400 {
			return fmt.Errorf("Apple 2FA 验证失败: HTTP %d", status)
		}
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("Apple 2FA 验证失败: HTTP %d", status)
	}
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointIDMSA, "/2sv/trust", s.srpHeaders(), nil, nil, false); err != nil {
		return fmt.Errorf("Apple 2SV 信任设备失败: %w", err)
	}
	return nil
}

// finishAppleAccountManage 换取管理接口 scnt + apiKey 并组装返回状态。
func (s *appleSRPLoginSession) finishAppleAccountManage(ctx context.Context) (AppleAccountState, error) {
	// 1. gs/ws/token: 刷新 scnt
	var token struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointManage, "/account/manage/gs/ws/token", s.manageHeaders(false), nil, &token, false); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple 登录换取 token 失败: %w", err)
	}
	if strings.TrimSpace(s.scnt) == "" {
		return AppleAccountState{}, fmt.Errorf("Apple 登录未返回 scnt, 无法完成登录")
	}
	// 2. /account/manage: 预取 apiKey
	var manage struct {
		APIKey string `json:"apiKey"`
	}
	if _, _, err := s.do(ctx, http.MethodGet, appleSRPEndpointManage, "/account/manage", s.manageHeaders(true), nil, &manage, false); err != nil {
		return AppleAccountState{}, fmt.Errorf("Apple 登录预取 api_key 失败: %w", err)
	}
	if strings.TrimSpace(manage.APIKey) == "" {
		return AppleAccountState{}, fmt.Errorf("Apple Account 管理接口未返回 api_key")
	}
	s.apiKey = strings.TrimSpace(manage.APIKey)
	return s.buildState(), nil
}

func (s *appleSRPLoginSession) buildState() AppleAccountState {
	return AppleAccountState{
		Cookies:   cloneStringMap(s.cookies),
		Scnt:      s.scnt,
		APIKey:    s.apiKey,
		SessionID: s.sessionID,
		Origin:    s.portalBase,
		UserAgent: s.userAgent,
		SavedAt:   time.Now(),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

// ---- 请求头构造 ----

// srpHeaders 构造 idmsa SRP 阶段请求头 (对应参考实现 srpHeaders 的 manage 分支)。
func (s *appleSRPLoginSession) srpHeaders() map[string]string {
	frameTag := s.frameTag()
	origin := strings.TrimSuffix(s.idmsaBase, "/appleauth/auth")
	headers := map[string]string{
		"Accept":                           "application/json, text/javascript, */*; q=0.01",
		"Content-Type":                     "application/json",
		"Origin":                           origin,
		"Referer":                          origin + "/",
		"X-Apple-Widget-Key":               s.clientID,
		"X-Apple-OAuth-Client-Id":          s.clientID,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       s.portalBase,
		"X-Apple-OAuth-Response-Mode":      "web_message",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-OAuth-State":              frameTag,
		"X-Apple-Frame-Id":                 frameTag,
		"X-Requested-With":                 "XMLHttpRequest",
		"X-Apple-I-FD-Client-Info":         appleAccountFDClientInfo(s.userAgent),
		"X-Apple-Domain-Id":                "11",
		"X-Apple-Privacy-Consent":          "true",
		"X-Apple-Privacy-Consent-Accepted": "true",
	}
	applyAppleSRPLoginBrowserHints(headers)
	headers["Sec-Fetch-Dest"] = "empty"
	headers["Sec-Fetch-Mode"] = "cors"
	headers["Sec-Fetch-Site"] = "same-origin"
	if s.authAttrs != "" {
		headers["X-Apple-Auth-Attributes"] = s.authAttrs
	}
	if s.scnt != "" {
		headers["scnt"] = s.scnt
	}
	if s.sessionID != "" {
		headers["X-Apple-ID-Session-Id"] = s.sessionID
	}
	return headers
}

// twoFactorHeaders 构造 2FA 阶段请求头。
func (s *appleSRPLoginSession) twoFactorHeaders() map[string]string {
	headers := s.srpHeaders()
	headers["Accept"] = "application/json, text/plain, */*"
	headers["X-Apple-App-Id"] = s.clientID
	delete(headers, "X-Requested-With")
	return headers
}

// portalHeaders 构造 account.apple.com 门户请求头。
func (s *appleSRPLoginSession) portalHeaders(accept string, jsonContent bool) map[string]string {
	headers := map[string]string{
		"Accept":             accept,
		"Referer":            s.portalBase + "/",
		"User-Agent":         s.userAgent,
		"Accept-Language":    appleAccountManageLanguage + ",en;q=0.9",
		"Sec-Fetch-Site":     "same-origin",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Dest":     "empty",
		"Sec-CH-UA-Platform": appleAccountManagePlatform,
		"Sec-CH-UA":          `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`,
		"Sec-CH-UA-Mobile":   "?0",
	}
	if jsonContent {
		headers["Content-Type"] = "application/json"
		headers["X-Apple-I-Request-Context"] = appleAccountManageRequestCtx
		headers["X-Apple-I-TimeZone"] = appleAccountManageTimeZone
		headers["X-Apple-I-FD-Client-Info"] = appleAccountFDClientInfo(s.userAgent)
	}
	return headers
}

// manageHeaders 构造 appleid.apple.com 管理 API 请求头 (带 scnt + X-Apple-I-* 全套)。
func (s *appleSRPLoginSession) manageHeaders(withScnt bool) map[string]string {
	headers := map[string]string{
		"Accept":                    "application/json, text/plain, */*",
		"Content-Type":              "application/json",
		"Origin":                    s.portalBase,
		"Referer":                   s.portalBase + "/",
		"User-Agent":                s.userAgent,
		"Accept-Language":           appleAccountManageLanguage + ",en;q=0.9",
		"Sec-Fetch-Site":            "same-site",
		"Sec-Fetch-Mode":            "cors",
		"Sec-Fetch-Dest":            "empty",
		"Sec-CH-UA-Platform":        appleAccountManagePlatform,
		"Sec-CH-UA":                 `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`,
		"Sec-CH-UA-Mobile":          "?0",
		"X-Apple-I-FD-Client-Info":  appleAccountFDClientInfo(s.userAgent),
		"X-Apple-I-Request-Context": appleAccountManageRequestCtx,
		"X-Apple-I-TimeZone":        appleAccountManageTimeZone,
	}
	if withScnt && s.scnt != "" {
		headers["scnt"] = s.scnt
	}
	if s.apiKey != "" {
		headers["X-Apple-Api-Key"] = s.apiKey
	}
	return headers
}

// applyAppleSRPLoginBrowserHints 设置浏览器指纹提示头。
func applyAppleSRPLoginBrowserHints(headers map[string]string) {
	headers["Accept-Language"] = appleAccountManageLanguage + ",en;q=0.9"
	headers["Sec-CH-UA"] = `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`
	headers["Sec-CH-UA-Mobile"] = "?0"
	headers["Sec-CH-UA-Platform"] = appleAccountManagePlatform
}

// trimAppleBody 截断错误响应体。
func trimAppleBody(data []byte) string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "空响应"
	}
	if len(text) > 240 {
		return text[:240] + "..."
	}
	return text
}
