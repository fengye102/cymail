package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

// mockAppleManageServer 模拟 Apple Account 管理 API (仅创建相关端点)。
type mockAppleManageServer struct {
	mu        sync.Mutex
	addStatus int
	addBody   string
	requests  []string
}

func newMockAppleManageServer() *mockAppleManageServer {
	return &mockAppleManageServer{addStatus: http.StatusOK}
}

func (m *mockAppleManageServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+path)
		m.mu.Unlock()
		switch {
		case path == "/account/manage/email/private/add" && r.Method == http.MethodPost:
			m.mu.Lock()
			status, body := m.addStatus, m.addBody
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = w.Write([]byte(`{"emailAddress":"apple-managed-1@privaterelay.icloud.com"}`))
				return
			}
			_, _ = w.Write([]byte(body))
		case path == "/account/manage/email/private/add/complete" && r.Method == http.MethodPut:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"emailAddress":"apple-managed-1@privaterelay.icloud.com","label":"server-label","id":"alias-srv-1","active":true}`))
		case strings.HasPrefix(path, "/account/manage/email/private/") && strings.HasSuffix(path, ".em"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"emailAddress":"apple-managed-1@privaterelay.icloud.com","label":"server-label","active":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	})
}

// bootstrappedAppleState 构造一个会话状态"已就绪"的新接口状态 (跳过引导, 直接可用)。
func bootstrappedAppleState() hme.AppleAccountState {
	return hme.AppleAccountState{
		Cookies:         map[string]string{"aasp": "session", "as_web": "web"},
		Scnt:            "test-scnt-srv",
		APIKey:          "test-api-key-srv",
		Origin:          "https://account.apple.com",
		UserAgent:       hme.AppleAccountManageUserAgent(),
		SavedAt:         time.Now().Add(-time.Hour),
		LastCheckedAt:   time.Now(),
		ManageExpiresAt: time.Now().Add(time.Hour),
		LastCheckOK:     true,
		LastStatus:      "新接口会话正常",
	}
}

func TestCreateAliasPreferAppleAccountSuccess(t *testing.T) {
	mock := newMockAppleManageServer()
	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)
	restore := hme.SetAppleAccountBaseURLForTesting(server.URL)
	t.Cleanup(restore)

	srv := newTestServer(t, Config{APIKey: "key"})
	acc, err := srv.mgr.AddAccount("aa-success", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.mgr.SaveAppleAccountState(acc.ID, bootstrappedAppleState()); err != nil {
		t.Fatal(err)
	}
	oldClient, err := hme.NewClient(map[string]string{"dummy": "x"}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}

	result, iface, err := srv.createAliasPreferAppleAccount(acc.ID, oldClient, "server-label")
	if err != nil {
		t.Fatalf("createAliasPreferAppleAccount() error = %v", err)
	}
	if iface != "apple_account" {
		t.Errorf("iface = %q, want apple_account", iface)
	}
	if result.Email != "apple-managed-1@privaterelay.icloud.com" {
		t.Errorf("email = %q", result.Email)
	}
	if result.Label != "server-label" {
		t.Errorf("label = %q, want server-label", result.Label)
	}
	joined := strings.Join(mock.requests, "|")
	if !strings.Contains(joined, "POST /account/manage/email/private/add") {
		t.Errorf("expected add request, got %v", mock.requests)
	}
	if strings.Contains(joined, "PUT /account/manage/email/private/add/complete") == false {
		t.Errorf("expected complete request, got %v", mock.requests)
	}
}

func TestCreateAliasPreferAppleAccountCapacityNoFallback(t *testing.T) {
	mock := newMockAppleManageServer()
	mock.addStatus = http.StatusBadRequest
	mock.addBody = `{"error":{"message":"reached maximum number of addresses"}}`
	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)
	restore := hme.SetAppleAccountBaseURLForTesting(server.URL)
	t.Cleanup(restore)

	srv := newTestServer(t, Config{APIKey: "key"})
	acc, err := srv.mgr.AddAccount("aa-capacity", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.mgr.SaveAppleAccountState(acc.ID, bootstrappedAppleState()); err != nil {
		t.Fatal(err)
	}
	oldClient, err := hme.NewClient(map[string]string{"dummy": "x"}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, iface, err := srv.createAliasPreferAppleAccount(acc.ID, oldClient, "label")
	if err == nil {
		t.Fatal("err = nil, want capacity reached")
	}
	if !errors.Is(err, hme.ErrAliasLimitReached) {
		t.Errorf("err = %v, want ErrAliasLimitReached (no fallback on capacity)", err)
	}
	if iface != "apple_account" {
		t.Errorf("iface = %q, want apple_account", iface)
	}
	// 容量已满不应再调用旧接口 → add 只调用了一次
	if len(mock.requests) != 1 {
		t.Errorf("expected exactly 1 add attempt, got %v", mock.requests)
	}
}

func TestCreateAliasPreferAppleAccountRateLimitFallsBack(t *testing.T) {
	mock := newMockAppleManageServer()
	mock.addStatus = http.StatusTooManyRequests
	mock.addBody = `{"error":{"message":"too many requests"}}`
	server := httptest.NewServer(mock.handler())
	t.Cleanup(server.Close)
	restore := hme.SetAppleAccountBaseURLForTesting(server.URL)
	t.Cleanup(restore)

	srv := newTestServer(t, Config{APIKey: "key"})
	acc, err := srv.mgr.AddAccount("aa-rate", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.mgr.SaveAppleAccountState(acc.ID, bootstrappedAppleState()); err != nil {
		t.Fatal(err)
	}
	// 旧接口客户端使用无效 Cookie: 回退后会在 validate 阶段快速失败 (401),
	// 断言错误来自旧接口 (而非新接口的限速错误)。
	oldClient, err := hme.NewClient(map[string]string{"dummy": "x"}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}

	_, iface, err := srv.createAliasPreferAppleAccount(acc.ID, oldClient, "label")
	if err == nil {
		t.Fatal("err = nil, want old-interface failure after fallback")
	}
	if errors.Is(err, hme.ErrAliasRateLimited) {
		t.Errorf("err = %v, want NOT ErrAliasRateLimited (should be old-interface error)", err)
	}
	if iface != "icloud_web" {
		t.Errorf("iface = %q, want icloud_web after fallback", iface)
	}
}

func TestAppleAccountStateRedactedHidesSecrets(t *testing.T) {
	state := bootstrappedAppleState()
	redacted := state.Redacted()
	if redacted.Cookies != nil || redacted.Scnt != "" || redacted.APIKey != "" || redacted.SessionID != "" {
		t.Error("Redacted() must hide cookies/scnt/apiKey/sessionID")
	}
	if !redacted.LastCheckOK || redacted.ManageExpiresAt.IsZero() {
		t.Error("Redacted() must keep non-secret status fields")
	}

	acc := &account.Account{AppleAccount: &state}
	status := acc.AppleAccountStatus()
	if status["enabled"] != true {
		t.Errorf("AppleAccountStatus enabled = %v, want true", status["enabled"])
	}
	acc2 := &account.Account{}
	if status2 := acc2.AppleAccountStatus(); status2["enabled"] != false {
		t.Errorf("empty account status = %v, want enabled:false", status2)
	}
}
