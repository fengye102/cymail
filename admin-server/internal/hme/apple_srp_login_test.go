package hme

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- mock: idmsa.apple.com/appleauth/auth ----

type mockIDMSAServer struct {
	mu       sync.Mutex
	requests []string

	// completeStatus 控制 signin/complete 首轮返回: 200 (无 2FA) / 409 (2FA) / 403 (密码错误)。
	completeStatus int
	// securityCodeStatus 控制 securitycode 提交返回状态 (默认 204)。
	securityCodeStatus int

	securityCodes []string // 收到的 2FA 验证码
	hcSeen        bool     // signin/complete 是否带 X-Apple-HC
	hcValue       string
}

func newMockIDMSAServer(completeStatus int) *mockIDMSAServer {
	return &mockIDMSAServer{completeStatus: completeStatus}
}

func (m *mockIDMSAServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+path)
		m.mu.Unlock()

		switch {
		case path == "/authorize/signin":
			// 捕获 hashcash 挑战头, complete 阶段必须带 X-Apple-HC
			w.Header().Set("X-Apple-HC-Bits", "4")
			w.Header().Set("X-Apple-HC-Challenge", "mock-challenge-123")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>signin</html>"))
		case path == "/verify/device/key/challenge":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case path == "/federate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case path == "/signin/init":
			// 固定测试向量: B 为合法小值 (1 <= B < N), mock 不校验 M2 正确性
			b := make([]byte, 256)
			b[255] = 0x02
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"iteration": 1000,
				"salt":      base64.StdEncoding.EncodeToString([]byte("salt-bytes")),
				"protocol":  "s2k",
				"b":         base64.StdEncoding.EncodeToString(b),
				"c":         "auth-mock-c",
			})
		case path == "/signin/complete":
			m.mu.Lock()
			m.hcSeen = m.hcSeen || r.Header.Get("X-Apple-HC") != ""
			m.hcValue = r.Header.Get("X-Apple-HC")
			status := m.completeStatus
			m.mu.Unlock()
			if status == http.StatusForbidden {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid credentials"}}`))
				return
			}
			if status == http.StatusConflict {
				// 2FA: 409 + scnt/session 头
				w.Header().Set("scnt", "scnt-from-409")
				w.Header().Set("X-Apple-ID-Session-Id", "session-from-409")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"message":"2FA required"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case path == "/verify/trusteddevice/securitycode":
			var body struct {
				SecurityCode struct {
					Code string `json:"code"`
				} `json:"securityCode"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.securityCodes = append(m.securityCodes, body.SecurityCode.Code)
			status := m.securityCodeStatus
			m.mu.Unlock()
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
		case path == "/2sv/trust":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	})
}

// ---- mock: appleid.apple.com 管理 API + account.apple.com 门户 ----

type mockManageServer struct {
	mu       sync.Mutex
	requests []string

	scnt   string
	apiKey string
	// manageScntSeen 表示 /account/manage 请求带上了预期 scnt 头。
	manageScntSeen bool
}

func newMockManageServer() *mockManageServer {
	return &mockManageServer{scnt: "scnt-final", apiKey: "api-key-final"}
}

func (m *mockManageServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+path)
		m.mu.Unlock()

		switch {
		case path == "/account/manage/section/privacy":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>privacy</html>"))
		case path == "/bootstrap/portal":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case path == "/account/manage/gs/ws/token":
			w.Header().Set("scnt", m.scnt)
			w.Header().Set("Set-Cookie", "test-manage-cookie=cookie-value; Path=/; HttpOnly")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case path == "/account/manage":
			m.mu.Lock()
			m.manageScntSeen = m.manageScntSeen || r.Header.Get("scnt") == m.scnt
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"apiKey": m.apiKey})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	})
}

func (m *mockManageServer) requestPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.requests))
	copy(out, m.requests)
	return out
}

// setupSRPLoginMocks 起两个 mock 服务 (idmsa 认证 + 门户/管理 API) 并把三组 Base URL 指向它们。
// 返回 mock 以及测试用的门户 Base URL。
func setupSRPLoginMocks(t *testing.T, completeStatus int) (*mockIDMSAServer, *mockManageServer, string) {
	t.Helper()
	idmsa := newMockIDMSAServer(completeStatus)
	idmsaTS := httptest.NewServer(idmsa.handler())
	t.Cleanup(idmsaTS.Close)

	manage := newMockManageServer()
	manageTS := httptest.NewServer(manage.handler())
	t.Cleanup(manageTS.Close)

	restore := SetAppleSRPLoginBaseURLsForTesting(idmsaTS.URL, manageTS.URL, manageTS.URL)
	t.Cleanup(restore)
	return idmsa, manage, manageTS.URL
}

func (m *mockIDMSAServer) requestPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.requests))
	copy(out, m.requests)
	return out
}

// ---- hashcash ----

func TestGenerateAppleHashcash(t *testing.T) {
	token, err := generateAppleHashcash(8, "challenge-abc", time.Now())
	if err != nil {
		t.Fatalf("generateAppleHashcash() error = %v", err)
	}
	if !strings.HasPrefix(token, "1:8:") {
		t.Errorf("token = %q, want prefix 1:8:", token)
	}
	sum := sha1.Sum([]byte(token))
	if got := leadingZeroBits(sum[:]); got < 8 {
		t.Errorf("leadingZeroBits = %d, want >= 8 (token=%q)", got, token)
	}
	// 格式: 1:<bits>:<UTC 时间戳>:<challenge>::
	parts := strings.Split(token, ":")
	if len(parts) != 6 {
		t.Fatalf("token format = %q, want 6 colon parts", token)
	}
	if parts[0] != "1" || parts[1] != "8" || parts[3] != "challenge-abc" || parts[4] != "" || parts[5] == "" {
		t.Errorf("token parts = %v", parts)
	}

	// 错误输入
	if _, err := generateAppleHashcash(0, "challenge", time.Now()); err == nil {
		t.Error("bits=0 should error")
	}
	if _, err := generateAppleHashcash(8, "", time.Now()); err == nil {
		t.Error("empty challenge should error")
	}
	if _, err := generateAppleHashcash(8, "   ", time.Now()); err == nil {
		t.Error("blank challenge should error")
	}
	if _, err := generateAppleHashcash(appleHashcashMaxBits+1, "challenge", time.Now()); err == nil {
		t.Error("bits > max should error")
	}
}

// ---- 域切换 ----

func TestParseAppleDomainRedirect(t *testing.T) {
	domain, ok := parseAppleDomainRedirect(http.StatusFound, []byte(`{"domainToUse":"https://www.icloud.com.cn"}`))
	if !ok || domain != "https://www.icloud.com.cn" {
		t.Errorf("parse 3xx = (%q, %v), want (https://www.icloud.com.cn, true)", domain, ok)
	}
	if _, ok := parseAppleDomainRedirect(http.StatusOK, []byte(`{"domainToUse":"https://www.icloud.com.cn"}`)); ok {
		t.Error("2xx must not be treated as redirect")
	}
	if _, ok := parseAppleDomainRedirect(http.StatusFound, []byte(`not json`)); ok {
		t.Error("non-JSON body must not be treated as redirect")
	}
	if _, ok := parseAppleDomainRedirect(http.StatusFound, []byte(`{"foo":"bar"}`)); ok {
		t.Error("missing domainToUse must not be treated as redirect")
	}
	if _, ok := parseAppleDomainRedirect(http.StatusFound, []byte(`{"domainToUse":""}`)); ok {
		t.Error("empty domainToUse must not be treated as redirect")
	}
}

func TestAppleSRPLoginSwitchDomain(t *testing.T) {
	s := &appleSRPLoginSession{
		idmsaBase:  "https://idmsa.apple.com/appleauth/auth",
		portalBase: "https://account.apple.com",
		manageBase: "https://appleid.apple.com",
	}
	if !s.switchDomain("https://www.icloud.com.cn") {
		t.Fatal("switch to CN should succeed")
	}
	if s.idmsaBase != "https://idmsa.apple.com.cn/appleauth/auth" ||
		s.portalBase != "https://account.apple.com.cn" ||
		s.manageBase != "https://appleid.apple.com.cn" {
		t.Errorf("after switch: %s | %s | %s", s.idmsaBase, s.portalBase, s.manageBase)
	}
	if s.switchDomain("https://www.icloud.com.cn") {
		t.Error("switch to same CN domain should fail")
	}
	if !s.switchDomain("https://www.icloud.com") {
		t.Fatal("switch back to US should succeed")
	}
	if s.idmsaBase != "https://idmsa.apple.com/appleauth/auth" ||
		s.portalBase != "https://account.apple.com" ||
		s.manageBase != "https://appleid.apple.com" {
		t.Errorf("after switch back: %s | %s | %s", s.idmsaBase, s.portalBase, s.manageBase)
	}
	if s.switchDomain("https://example.org") {
		t.Error("unknown domain switch should fail")
	}
	// 测试 mock 端点 (host 不含 apple.com) 不可被切换
	mockSession := &appleSRPLoginSession{
		idmsaBase:  "http://127.0.0.1:12345/appleauth/auth",
		portalBase: "http://127.0.0.1:12345",
		manageBase: "http://127.0.0.1:12345",
	}
	if mockSession.switchDomain("https://www.icloud.com.cn") {
		t.Error("mock base must not switch domain")
	}
}

// ---- 完整登录流程 ----

func TestAppleSRPLoginWith2FA(t *testing.T) {
	idmsa, manage, portalBase := setupSRPLoginMocks(t, http.StatusConflict)

	otpCalls := 0
	state, err := AppleSRPLogin("Test@Example.com", "password123", func() (string, error) {
		otpCalls++
		return "123456", nil
	})
	if err != nil {
		t.Fatalf("AppleSRPLogin() error = %v", err)
	}

	// 状态字段
	if state.Scnt != "scnt-final" {
		t.Errorf("Scnt = %q, want scnt-final", state.Scnt)
	}
	if state.APIKey != "api-key-final" {
		t.Errorf("APIKey = %q, want api-key-final", state.APIKey)
	}
	if state.SessionID != "session-from-409" {
		t.Errorf("SessionID = %q, want session-from-409", state.SessionID)
	}
	if state.Origin != portalBase {
		t.Errorf("Origin = %q, want portal base %q (生产环境为 %q)", state.Origin, portalBase, appleAccountManageOrigin)
	}
	if state.UserAgent != appleAccountManageUserAgent {
		t.Errorf("UserAgent = %q, want %q", state.UserAgent, appleAccountManageUserAgent)
	}
	if state.SavedAt.IsZero() {
		t.Error("SavedAt is zero")
	}
	if state.Cookies["test-manage-cookie"] != "cookie-value" {
		t.Errorf("Cookies = %v, want merged test-manage-cookie", state.Cookies)
	}

	// 2FA: otpProvider 被调用一次且验证码被提交
	if otpCalls != 1 {
		t.Errorf("otpProvider called %d times, want 1", otpCalls)
	}
	if len(idmsa.securityCodes) != 1 || idmsa.securityCodes[0] != "123456" {
		t.Errorf("securityCodes = %v, want [123456]", idmsa.securityCodes)
	}

	// signin/complete 带 X-Apple-HC hashcash, 且难度达标
	if !idmsa.hcSeen {
		t.Error("signin/complete missing X-Apple-HC header")
	} else {
		sum := sha1.Sum([]byte(idmsa.hcValue))
		if got := leadingZeroBits(sum[:]); got < 4 {
			t.Errorf("X-Apple-HC leadingZeroBits = %d, want >= 4", got)
		}
	}

	// idmsa 请求顺序
	wantIDMSA := []string{
		"GET /authorize/signin",
		"POST /verify/device/key/challenge",
		"POST /federate",
		"POST /signin/init",
		"POST /signin/complete",
		"POST /verify/trusteddevice/securitycode",
		"GET /2sv/trust",
	}
	paths := idmsa.requestPaths()
	if len(paths) != len(wantIDMSA) {
		t.Fatalf("idmsa requests = %v, want %v", paths, wantIDMSA)
	}
	for i, want := range wantIDMSA {
		if paths[i] != want {
			t.Errorf("idmsa request[%d] = %s, want %s", i, paths[i], want)
		}
	}

	// 门户预热 + token + manage 请求顺序
	wantManage := []string{
		"GET /account/manage/section/privacy",
		"GET /bootstrap/portal",
		"GET /account/manage/gs/ws/token", // prime 阶段
		"GET /account/manage/gs/ws/token", // finish 阶段
		"GET /account/manage",
	}
	managePaths := manage.requestPaths()
	if len(managePaths) != len(wantManage) {
		t.Fatalf("manage requests = %v, want %v", managePaths, wantManage)
	}
	for i, want := range wantManage {
		if managePaths[i] != want {
			t.Errorf("manage request[%d] = %s, want %s", i, managePaths[i], want)
		}
	}
	if !manage.manageScntSeen {
		t.Error("/account/manage request did not carry scnt header")
	}

	// 返回值可直接交给 NewAppleAccountClient
	client := NewAppleAccountClient(state, false)
	if !client.HasSession() {
		t.Error("NewAppleAccountClient(state).HasSession() = false")
	}
}

func TestAppleSRPLoginWithout2FA(t *testing.T) {
	idmsa, _, _ := setupSRPLoginMocks(t, http.StatusOK)

	otpCalls := 0
	state, err := AppleSRPLogin("test@example.com", "password123", func() (string, error) {
		otpCalls++
		return "000000", nil
	})
	if err != nil {
		t.Fatalf("AppleSRPLogin() error = %v", err)
	}
	if otpCalls != 0 {
		t.Errorf("otpProvider called %d times, want 0 (no 2FA)", otpCalls)
	}
	if state.Scnt != "scnt-final" || state.APIKey != "api-key-final" {
		t.Errorf("state = scnt %q apiKey %q", state.Scnt, state.APIKey)
	}
	for _, req := range idmsa.requestPaths() {
		if strings.Contains(req, "securitycode") || strings.Contains(req, "2sv") {
			t.Errorf("unexpected 2FA request: %s", req)
		}
	}
}

func TestAppleSRPLoginWrongPassword(t *testing.T) {
	setupSRPLoginMocks(t, http.StatusForbidden)

	_, err := AppleSRPLogin("test@example.com", "wrong-password", func() (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("expected error for wrong password")
	}
	if !strings.Contains(err.Error(), "密码错误") {
		t.Errorf("error = %v, want contains 密码错误", err)
	}
}

func TestAppleSRPLogin2FAWithoutProvider(t *testing.T) {
	setupSRPLoginMocks(t, http.StatusConflict)

	_, err := AppleSRPLogin("test@example.com", "password123", nil)
	if err == nil {
		t.Fatal("expected error when otpProvider is nil")
	}
	if !strings.Contains(err.Error(), "双重认证") {
		t.Errorf("error = %v, want contains 双重认证", err)
	}
}

func TestAppleSRPLogin2FAFailed(t *testing.T) {
	idmsa, _, _ := setupSRPLoginMocks(t, http.StatusConflict)
	idmsa.securityCodeStatus = http.StatusBadRequest

	_, err := AppleSRPLogin("test@example.com", "password123", func() (string, error) {
		return "123456", nil
	})
	if err == nil {
		t.Fatal("expected error when securitycode fails")
	}
	if !strings.Contains(err.Error(), "2FA 验证失败") {
		t.Errorf("error = %v, want contains 2FA 验证失败", err)
	}
}

// TestAppleSRPLoginStateDefaults 校验生产默认端点下返回状态中的 Origin / UserAgent。
func TestAppleSRPLoginStateDefaults(t *testing.T) {
	s := &appleSRPLoginSession{
		userAgent:  appleAccountManageUserAgent,
		portalBase: appleAccountManageOrigin,
		scnt:       "scnt-x",
		apiKey:     "key-x",
		cookies:    map[string]string{"aasp": "session-cookie"},
	}
	state := s.buildState()
	if state.Origin != appleAccountManageOrigin {
		t.Errorf("Origin = %q, want %q", state.Origin, appleAccountManageOrigin)
	}
	if state.UserAgent != appleAccountManageUserAgent {
		t.Errorf("UserAgent = %q, want %q", state.UserAgent, appleAccountManageUserAgent)
	}
	if state.Scnt != "scnt-x" || state.APIKey != "key-x" {
		t.Errorf("Scnt/APIKey = %q/%q", state.Scnt, state.APIKey)
	}
	if state.Cookies["aasp"] != "session-cookie" {
		t.Errorf("Cookies = %v", state.Cookies)
	}
}
