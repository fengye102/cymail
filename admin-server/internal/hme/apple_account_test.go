package hme

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockAppleAccountServer 模拟 Apple Account 管理接口。
// 覆盖: 门户预热 / token(scnt) / apiKey / 创建 / 确认 端点。
type mockAppleAccountServer struct {
	mu       sync.Mutex
	requests []string // 记录 (method, path) 序列
	scnt     string
	apiKey   string

	// addStatus / addBody 控制 add 端点的返回 (测试错误分类用)。
	addStatus int
	addBody   string
	// addAuthFailTimes 前 N 次 add 返回 419 (测试刷新重试)。
	addAuthFailTimes int
}

func newMockAppleAccountServer() *mockAppleAccountServer {
	return &mockAppleAccountServer{
		scnt:      "test-scnt-001",
		apiKey:    "test-api-key-001",
		addStatus: http.StatusOK,
	}
}

func (m *mockAppleAccountServer) handler() http.Handler {
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case path == "/account/manage":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(map[string]string{"apiKey": m.apiKey})
			_, _ = w.Write(body)
		case path == "/account/manage/email/private/add":
			m.mu.Lock()
			authFail := m.addAuthFailTimes > 0
			if authFail {
				m.addAuthFailTimes--
			}
			m.mu.Unlock()
			if authFail {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(appleAccountHTTPStatusSessionTimeout)
				_, _ = w.Write([]byte(`{"service_errors":[{"message":"session expired"}]}`))
				return
			}
			if m.addStatus != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(m.addStatus)
				_, _ = w.Write([]byte(m.addBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"emailAddress":"generated123@privaterelay.icloud.com"}`))
		case path == "/account/manage/email/private/add/complete":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"emailAddress":"generated123@privaterelay.icloud.com","label":"test-label","id":"alias-42","active":true}`))
		case strings.HasPrefix(path, "/account/manage/email/private/") && strings.HasSuffix(path, ".em"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"emailAddress":"generated123@privaterelay.icloud.com","label":"test-label","forwardToEmail":"forward@example.com","active":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	})
}

func (m *mockAppleAccountServer) requestPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.requests))
	copy(out, m.requests)
	return out
}

// newMockAppleAccountClient 创建指向 mock 服务的客户端。
func newMockAppleAccountClient(t *testing.T, mock *mockAppleAccountServer, state *AppleAccountState) *AppleAccountClient {
	t.Helper()
	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)

	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	if state == nil {
		state = &AppleAccountState{}
	}
	if state.Origin == "" {
		state.Origin = server.URL
	}
	if state.Cookies == nil {
		state.Cookies = map[string]string{"aasp": "session-cookie", "as_web": "web-cookie"}
	}
	return NewAppleAccountClient(*state, false)
}

func TestAppleAccountBootstrapAndCreate(t *testing.T) {
	mock := newMockAppleAccountServer()
	client := newMockAppleAccountClient(t, mock, nil)

	if err := client.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	state := client.State()
	if state.Scnt != "test-scnt-001" {
		t.Errorf("Bootstrap scnt = %q, want %q", state.Scnt, "test-scnt-001")
	}
	if state.APIKey != "test-api-key-001" {
		t.Errorf("Bootstrap apiKey = %q, want %q", state.APIKey, "test-api-key-001")
	}
	if !state.LastCheckOK {
		t.Error("LastCheckOK = false, want true")
	}
	if state.ManageExpiresAt.IsZero() {
		t.Error("ManageExpiresAt is zero, want TTL parsed")
	}

	result, err := client.CreateAlias("my-label")
	if err != nil {
		t.Fatalf("CreateAlias() error = %v", err)
	}
	if result.Email != "generated123@privaterelay.icloud.com" {
		t.Errorf("CreateAlias email = %q", result.Email)
	}
	if result.Label != "test-label" {
		t.Errorf("CreateAlias label = %q, want test-label", result.Label)
	}

	paths := mock.requestPaths()
	wantOrder := []string{
		"GET /account/manage/section/privacy",
		"GET /bootstrap/portal",
		"GET /account/manage/gs/ws/token",
		"GET /account/manage",
		"POST /account/manage/email/private/add",
		"PUT /account/manage/email/private/add/complete",
		"GET /account/manage/email/private/alias-42.em",
	}
	if len(paths) != len(wantOrder) {
		t.Fatalf("request count = %d, want %d (%v)", len(paths), len(wantOrder), paths)
	}
	for i, want := range wantOrder {
		if paths[i] != want {
			t.Errorf("request[%d] = %s, want %s", i, paths[i], want)
		}
	}
}

func TestAppleAccountCreateRefreshesStaleSession(t *testing.T) {
	mock := newMockAppleAccountServer()
	// 会话已保存但 TTL 过期 → 创建前应自动刷新 (再调 token + manage)。
	state := &AppleAccountState{
		Scnt:            "old-scnt",
		APIKey:          "old-key",
		LastCheckedAt:   time.Now().Add(-2 * time.Hour),
		ManageExpiresAt: time.Now().Add(-10 * time.Minute),
		LastCheckOK:     true,
	}
	client := newMockAppleAccountClient(t, mock, state)

	result, err := client.CreateAlias("")
	if err != nil {
		t.Fatalf("CreateAlias() error = %v", err)
	}
	if result.Email != "generated123@privaterelay.icloud.com" {
		t.Errorf("email = %q", result.Email)
	}

	paths := mock.requestPaths()
	joined := strings.Join(paths, "|")
	if !strings.Contains(joined, "GET /account/manage/gs/ws/token") {
		t.Errorf("expected token refresh before create, got %v", paths)
	}
	// 刷新后应有 scnt + apiKey
	if got := client.State().Scnt; got != "test-scnt-001" {
		t.Errorf("Scnt after refresh = %q", got)
	}
}

func TestAppleAccountCreateRefreshesWhenAuthFailed(t *testing.T) {
	mock := newMockAppleAccountServer()
	mock.addAuthFailTimes = 1 // 首次 add 返回 419 → 应刷新后重试
	state := &AppleAccountState{
		Scnt:            "test-scnt-001",
		APIKey:          "test-api-key-001",
		LastCheckedAt:   time.Now().Add(-time.Minute),
		ManageExpiresAt: time.Now().Add(30 * time.Minute),
		LastCheckOK:     true,
	}
	client := newMockAppleAccountClient(t, mock, state)

	result, err := client.CreateAlias("retry-label")
	if err != nil {
		t.Fatalf("CreateAlias() error = %v", err)
	}
	if result.Email != "generated123@privaterelay.icloud.com" {
		t.Errorf("email = %q", result.Email)
	}
	paths := mock.requestPaths()
	// add(419) → token → manage → add(成功)
	joined := strings.Join(paths, "|")
	if strings.Count(joined, "POST /account/manage/email/private/add") != 2 {
		t.Errorf("expected 2 add attempts after auth-failure retry, got %v", paths)
	}
	if !strings.Contains(joined, "GET /account/manage/gs/ws/token") {
		t.Errorf("expected refresh between retries, got %v", paths)
	}
}

func TestAppleAccountRateLimitClassified(t *testing.T) {
	mock := newMockAppleAccountServer()
	mock.addStatus = http.StatusTooManyRequests
	mock.addBody = `{"error":{"message":"too many requests, rate limit reached"}}`
	state := &AppleAccountState{
		Scnt:            "test-scnt-001",
		APIKey:          "test-api-key-001",
		LastCheckedAt:   time.Now().Add(-time.Minute),
		ManageExpiresAt: time.Now().Add(30 * time.Minute),
		LastCheckOK:     true,
	}
	client := newMockAppleAccountClient(t, mock, state)

	_, err := client.CreateAlias("rate-label")
	if err == nil {
		t.Fatal("CreateAlias() error = nil, want rate limited")
	}
	if !errors.Is(err, ErrAliasRateLimited) {
		t.Errorf("err = %v, want ErrAliasRateLimited", err)
	}
	var rateErr *AliasRateLimitError
	if !errors.As(err, &rateErr) {
		t.Errorf("err = %v, want *AliasRateLimitError", err)
	}
}

func TestAppleAccountCapacityClassified(t *testing.T) {
	mock := newMockAppleAccountServer()
	mock.addStatus = http.StatusBadRequest
	mock.addBody = `{"error":{"message":"You have reached the maximum number of addresses"}}`
	state := &AppleAccountState{
		Scnt:            "test-scnt-001",
		APIKey:          "test-api-key-001",
		LastCheckedAt:   time.Now().Add(-time.Minute),
		ManageExpiresAt: time.Now().Add(30 * time.Minute),
		LastCheckOK:     true,
	}
	client := newMockAppleAccountClient(t, mock, state)

	_, err := client.CreateAlias("capacity-label")
	if err == nil {
		t.Fatal("CreateAlias() error = nil, want capacity reached")
	}
	if !errors.Is(err, ErrAliasLimitReached) {
		t.Errorf("err = %v, want ErrAliasLimitReached", err)
	}
}

func TestAppleAccountAuthFailedOnSessionExpired(t *testing.T) {
	// 让 token 端点返回 419: 覆盖 appleAccountHTTPStatusSessionTimeout 分支
	state := &AppleAccountState{
		Scnt:            "test-scnt-001",
		APIKey:          "test-api-key-001",
		LastCheckedAt:   time.Now().Add(-time.Minute),
		ManageExpiresAt: time.Now().Add(-time.Minute), // 过期 → 触发刷新 → token 419
		LastCheckOK:     true,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(appleAccountHTTPStatusSessionTimeout)
		_, _ = w.Write([]byte(`{"service_errors":[{"message":"session has expired"}]}`))
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	state.Origin = server.URL // 门户预热也指向 mock, 避免真实外网请求
	client := NewAppleAccountClient(*state, false)

	_, err := client.CreateAlias("auth-label")
	if err == nil {
		t.Fatal("CreateAlias() error = nil, want auth failed")
	}
	if !errors.Is(err, ErrAppleAccountAuthFailed) {
		t.Errorf("err = %v, want ErrAppleAccountAuthFailed", err)
	}
}

func TestAppleAccountBootstrapInvalidCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"service_errors":[{"message":"authentication_failed"}]}`))
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	client := NewAppleAccountClient(AppleAccountState{
		Cookies: map[string]string{"aasp": "invalid"},
		Origin:  server.URL,
	}, false)

	err := client.Bootstrap()
	if err == nil {
		t.Fatal("Bootstrap() error = nil, want auth failed")
	}
	if !errors.Is(err, ErrAppleAccountAuthFailed) {
		t.Errorf("err = %v, want ErrAppleAccountAuthFailed", err)
	}
	state := client.State()
	if state.LastCheckOK {
		t.Error("LastCheckOK = true after failed bootstrap")
	}
}

func TestAppleAccountFingerprint(t *testing.T) {
	// 指纹必须是稳定的可打印字符串, 且包含必需字段。
	a := appleAccountFDClientInfo(appleAccountManageUserAgent)
	time.Sleep(5 * time.Millisecond)
	b := appleAccountFDClientInfo(appleAccountManageUserAgent)
	var parsed map[string]string
	if err := json.Unmarshal([]byte(a), &parsed); err != nil {
		t.Fatalf("FD client info not JSON: %v", err)
	}
	if parsed["U"] != appleAccountManageUserAgent || parsed["L"] != appleAccountManageLanguage || parsed["Z"] != appleAccountManageGMTOffset || parsed["V"] != "1.1" {
		t.Errorf("FD client info fields wrong: %v", parsed)
	}
	if parsed["F"] == "" {
		t.Error("F (fingerprint) is empty")
	}
	// F 字段仅含指纹字母表字符
	for _, r := range parsed["F"] {
		if !strings.ContainsRune(appleAccountFingerprintAlphabet, r) {
			t.Errorf("fingerprint contains invalid char %q", r)
			break
		}
	}
	if a == b {
		t.Error("two fingerprints identical — expected timestamp variation")
	}
}

func TestAppleAccountHostResolution(t *testing.T) {
	tests := []struct {
		host   string
		base   string
		origin string
	}{
		{"appleid.apple.com.cn", "https://appleid.apple.com.cn", "https://account.apple.com.cn"},
		{"account.apple.com.cn", "https://appleid.apple.com.cn", "https://account.apple.com.cn"},
		{"icloud.com.cn", "https://appleid.apple.com.cn", "https://account.apple.com.cn"},
		{"www.icloud.com.cn", "https://appleid.apple.com.cn", "https://account.apple.com.cn"},
		{"https://appleid.apple.com.cn", "https://appleid.apple.com.cn", "https://account.apple.com.cn"},
		{"icloud.com", "https://appleid.apple.com", "https://account.apple.com"},
		{"appleid.apple.com", "https://appleid.apple.com", "https://account.apple.com"},
		{"", "https://appleid.apple.com", "https://account.apple.com"},
	}
	for _, tc := range tests {
		if got := AppleAccountManageBaseForHost(tc.host); got != tc.base {
			t.Errorf("AppleAccountManageBaseForHost(%q) = %q, want %q", tc.host, got, tc.base)
		}
		if got := AppleAccountManageOriginForHost(tc.host); got != tc.origin {
			t.Errorf("AppleAccountManageOriginForHost(%q) = %q, want %q", tc.host, got, tc.origin)
		}
	}
	// Origin 为空时 NewAppleAccountClient 应按 Host 默认 .cn 门户
	client := NewAppleAccountClient(AppleAccountState{Host: "appleid.apple.com.cn"}, false)
	if got := client.State().Origin; got != "https://account.apple.com.cn" {
		t.Errorf("NewAppleAccountClient default Origin = %q, want https://account.apple.com.cn", got)
	}
}

// appleAccountCaptureRecord 记录一次被拦截请求的原始目标与关键头。
type appleAccountCaptureRecord struct {
	URL     string
	Origin  string
	Referer string
}

// appleAccountCaptureTransport 记录请求原始 URL 后改写为 mock 服务地址。
type appleAccountCaptureTransport struct {
	target  *url.URL
	records *[]appleAccountCaptureRecord
}

func (t *appleAccountCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*t.records = append(*t.records, appleAccountCaptureRecord{
		URL:     req.URL.String(),
		Origin:  req.Header.Get("Origin"),
		Referer: req.Header.Get("Referer"),
	})
	next := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	next.URL = &u
	next.Host = ""
	return http.DefaultTransport.RoundTrip(next)
}

// TestAppleAccountCNHostRouting 验证 .cn 域路由:
// portal 请求 → account.apple.com.cn; token/manage 请求 → appleid.apple.com.cn。
func TestAppleAccountCNHostRouting(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/account/manage/section/privacy":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>privacy</html>"))
		case r.URL.Path == "/bootstrap/portal":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case r.URL.Path == "/account/manage/gs/ws/token":
			w.Header().Set("scnt", "cn-scnt")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case r.URL.Path == "/account/manage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiKey":"cn-api-key"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	}))
	t.Cleanup(mock.Close)

	// 显式传 .com Origin, 验证按 Host 解析 .cn 门户 (Host 优先于 Origin)
	client := NewAppleAccountClient(AppleAccountState{
		Cookies: map[string]string{"aasp": "cn-session"},
		Host:    "appleid.apple.com.cn",
		Origin:  "https://account.apple.com",
	}, false)
	var records []appleAccountCaptureRecord
	target, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatal(err)
	}
	client.httpc = &http.Client{Timeout: appleAccountRequestTimeout, Transport: &appleAccountCaptureTransport{target: target, records: &records}}

	if err := client.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	state := client.State()
	if state.Scnt != "cn-scnt" {
		t.Errorf("Scnt = %q, want cn-scnt", state.Scnt)
	}
	if state.APIKey != "cn-api-key" {
		t.Errorf("APIKey = %q, want cn-api-key", state.APIKey)
	}

	var portal, token, manage bool
	for _, rec := range records {
		u, err := url.Parse(rec.URL)
		if err != nil {
			t.Fatalf("captured url %q: %v", rec.URL, err)
		}
		switch u.Path {
		case "/account/manage/section/privacy", "/bootstrap/portal":
			portal = true
			if u.Host != "account.apple.com.cn" {
				t.Errorf("portal request host = %q, want account.apple.com.cn (url=%s)", u.Host, rec.URL)
			}
		case "/account/manage/gs/ws/token":
			token = true
			if u.Host != "appleid.apple.com.cn" {
				t.Errorf("token request host = %q, want appleid.apple.com.cn (url=%s)", u.Host, rec.URL)
			}
			if rec.Origin != "https://account.apple.com.cn" {
				t.Errorf("token request Origin = %q, want https://account.apple.com.cn", rec.Origin)
			}
			if rec.Referer != "https://account.apple.com.cn/" {
				t.Errorf("token request Referer = %q, want https://account.apple.com.cn/", rec.Referer)
			}
		case "/account/manage":
			manage = true
			if u.Host != "appleid.apple.com.cn" {
				t.Errorf("manage request host = %q, want appleid.apple.com.cn (url=%s)", u.Host, rec.URL)
			}
		}
	}
	if !portal || !token || !manage {
		t.Errorf("missing requests: portal=%v token=%v manage=%v (records=%v)", portal, token, manage, records)
	}
}

// TestAppleAccountTransientNetworkRetrySucceeds 验证瞬态网络错误重试:
// 前两次连接被重置 (EOF), 第三次成功 → 请求次数=3 且成功。
func TestAppleAccountTransientNetworkRetrySucceeds(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		count := attempts
		mu.Unlock()
		if count <= 2 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiKey":"retried-key"}`))
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	client := NewAppleAccountClient(AppleAccountState{
		Cookies: map[string]string{"aasp": "session"},
		Origin:  server.URL,
	}, false)

	start := time.Now()
	if err := client.loadAPIKey(); err != nil {
		t.Fatalf("loadAPIKey() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 2*appleAccountTransientBackoff {
		t.Errorf("retry backoff too short: elapsed=%v, want >= 2.4s total backoff", elapsed)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if state := client.State(); state.APIKey != "retried-key" {
		t.Errorf("APIKey = %q, want retried-key", state.APIKey)
	}
}

// TestAppleAccountTransientHTTPStatusNotRetried 验证 HTTP 非 2xx 状态错误不重试。
func TestAppleAccountTransientHTTPStatusNotRetried(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"server error"}}`))
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	client := NewAppleAccountClient(AppleAccountState{
		Cookies: map[string]string{"aasp": "session"},
		Origin:  server.URL,
	}, false)

	if err := client.loadAPIKey(); err == nil {
		t.Fatal("loadAPIKey() error = nil, want HTTP error")
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Errorf("attempts = %d, want 1 (HTTP errors must not be retried)", got)
	}
}

// TestAppleAccountKeepAliveSuccess 验证 KeepAlive 成功路径:
// 刷新 token/apiKey → forwardemail → jslogs (best-effort) → LastCheckOK。
func TestAppleAccountKeepAliveSuccess(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			w.Header().Set("scnt", "keep-scnt")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case "/account/manage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiKey":"keep-key"}`))
		case "/account/manage/forwardemail":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"forwardToEmail":"keep@example.com"}`))
		case "/v2/jslogs":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	client := NewAppleAccountClient(AppleAccountState{
		Cookies:         map[string]string{"aasp": "session"},
		Scnt:            "keep-scnt",
		APIKey:          "keep-key",
		Origin:          server.URL,
		LastCheckedAt:   time.Now().Add(-time.Hour),
		ManageExpiresAt: time.Now().Add(-time.Minute), // 过期 → KeepAlive 内 refreshLocked 会刷新
		LastCheckOK:     true,
	}, false)

	before := time.Now()
	if err := client.KeepAlive(); err != nil {
		t.Fatalf("KeepAlive() error = %v", err)
	}
	state := client.State()
	if !state.LastCheckOK {
		t.Error("LastCheckOK = false after successful keepalive")
	}
	if state.LastCheckedAt.Before(before) {
		t.Error("LastCheckedAt not updated by keepalive")
	}
	if state.ManageExpiresAt.IsZero() {
		t.Error("ManageExpiresAt not refreshed by keepalive")
	}
	mu.Lock()
	joined := strings.Join(paths, "|")
	mu.Unlock()
	for _, want := range []string{
		"GET /account/manage/gs/ws/token",
		"GET /account/manage",
		"GET /account/manage/forwardemail",
		"POST /v2/jslogs",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing request %s in %v", want, paths)
		}
	}
}

// TestAppleAccountKeepAliveForwardEmailError 验证 forwardemail 非 2xx 时 KeepAlive 返回错误。
func TestAppleAccountKeepAliveForwardEmailError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			w.Header().Set("scnt", "keep-scnt")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"timeOutInterval":30}`))
		case "/account/manage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiKey":"keep-key"}`))
		case "/account/manage/forwardemail":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"server error"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	}))
	t.Cleanup(server.Close)
	previous := appleAccountManageBaseURL
	appleAccountManageBaseURL = server.URL
	t.Cleanup(func() { appleAccountManageBaseURL = previous })

	client := NewAppleAccountClient(AppleAccountState{
		Cookies:         map[string]string{"aasp": "session"},
		Scnt:            "keep-scnt",
		APIKey:          "keep-key",
		Origin:          server.URL,
		LastCheckedAt:   time.Now().Add(-time.Hour),
		ManageExpiresAt: time.Now().Add(-time.Minute),
		LastCheckOK:     true,
	}, false)

	if err := client.KeepAlive(); err == nil {
		t.Fatal("KeepAlive() error = nil, want forwardemail failure")
	}
}

func TestAppleAccountNeedsKeepAlive(t *testing.T) {
	base := AppleAccountState{
		Cookies:         map[string]string{"aasp": "s"},
		Scnt:            "scnt",
		APIKey:          "key",
		LastCheckedAt:   time.Now().Add(-5 * time.Minute),
		LastCheckOK:     true,
		ManageExpiresAt: time.Now().Add(time.Hour),
	}
	interval := 10 * time.Minute
	now := time.Now()

	tests := []struct {
		name string
		mut  func(*AppleAccountState)
		iv   time.Duration
		want bool
	}{
		{"fresh within interval", func(s *AppleAccountState) { s.LastCheckedAt = now.Add(-time.Minute) }, interval, false},
		{"over interval", func(s *AppleAccountState) { s.LastCheckedAt = now.Add(-11 * time.Minute) }, interval, true},
		{"exactly interval", func(s *AppleAccountState) { s.LastCheckedAt = now.Add(-interval) }, interval, true},
		{"zero last checked", func(s *AppleAccountState) { s.LastCheckedAt = time.Time{} }, interval, true},
		{"last check failed", func(s *AppleAccountState) { s.LastCheckOK = false; s.LastCheckedAt = now.Add(-time.Minute) }, interval, true},
		{"missing scnt", func(s *AppleAccountState) { s.Scnt = "" }, interval, false},
		{"missing api key", func(s *AppleAccountState) { s.APIKey = "" }, interval, false},
		{"zero interval defaults", func(s *AppleAccountState) { s.LastCheckedAt = now.Add(-5 * time.Minute) }, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := base
			tc.mut(&state)
			client := NewAppleAccountClient(state, false)
			if got := client.NeedsKeepAlive(tc.iv, now); got != tc.want {
				t.Errorf("NeedsKeepAlive() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppleAccountDebugFingerprint(t *testing.T) {
	if got := appleAccountDebugFingerprint(""); got != "-" {
		t.Errorf("empty fingerprint = %q, want -", got)
	}
	fp := appleAccountDebugFingerprint("secret-scnt-value")
	parts := strings.Split(fp, "/")
	if len(parts) != 2 || len(parts[0]) != 8 {
		t.Fatalf("fingerprint format = %q, want 8hex/len", fp)
	}
	if parts[1] != "17" {
		t.Errorf("fingerprint length = %q, want 17", parts[1])
	}
	// 不同值指纹不同
	if appleAccountDebugFingerprint("a") == appleAccountDebugFingerprint("b") {
		t.Error("fingerprints of different values must differ")
	}
	// 响应体摘要脱敏: JSON 压缩 + 敏感字段 redacted + ≤200 字符
	body := appleAccountDebugBody([]byte(`{"apiKey":"SECRET-KEY","email":"a@b.com","ok":true,  "nested":{"token":"t"}}`))
	if strings.Contains(body, "SECRET-KEY") || strings.Contains(body, "a@b.com") || strings.Contains(body, `"t"`) {
		t.Errorf("debug body must redact secrets, got %q", body)
	}
	if !strings.Contains(body, "<redacted>") {
		t.Errorf("debug body should contain redacted markers, got %q", body)
	}
	if len(body) > 200 {
		t.Errorf("debug body too long: %d", len(body))
	}
}
