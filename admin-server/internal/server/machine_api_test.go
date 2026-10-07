package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	"icloud-hme/internal/store"
)

const machineTestAPIKey = "test-machine-key"

// newMachineTestServer 构造带内存库存的测试服务 (参考 server_test.go 的
// newTestServer / TestAllocateBatchUsesSelectedMailboxes 的种子方式)。
func newMachineTestServer(t *testing.T) (*Server, *store.Memory) {
	t.Helper()
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	srv := NewWithConfig(mgr, Config{APIKey: machineTestAPIKey, Fulfillment: fulfillment.New(mgr, st)})
	return srv, st
}

// machineTestRouter 复刻 register() 中 /api 组的中间件与路由接线,
// 验证机器接口在真实认证/限流中间件下的行为。
func machineTestRouter(srv *Server) http.Handler {
	engine := gin.New()
	api := engine.Group("/api")
	api.Use(srv.secureHeaders(), srv.limitRequestBody(), srv.authenticate())
	srv.registerMachineAPI(api)
	return engine
}

func machineRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

// machineDecodeData 解析 {success, message, data} 响应并把 data 解码到 out。
func machineDecodeData(t *testing.T, resp *httptest.ResponseRecorder, out any) {
	t.Helper()
	var payload struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", resp.Body.String(), err)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(payload.Data, out); err != nil {
		t.Fatalf("decode data %q: %v", string(payload.Data), err)
	}
}

func machineSeedMailbox(t *testing.T, st *store.Memory, mailbox store.Mailbox) {
	t.Helper()
	if mailbox.Address == "" {
		t.Fatal("mailbox address required")
	}
	if mailbox.Status == "" {
		mailbox.Status = "available"
	}
	if err := st.UpsertMailboxes(context.Background(), []store.Mailbox{mailbox}); err != nil {
		t.Fatal(err)
	}
}

func machineSeedMessage(t *testing.T, st *store.Memory, message store.Message) {
	t.Helper()
	if err := st.SaveMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
}

func TestMachineAPIAuthentication(t *testing.T) {
	srv, _ := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/mailboxes/claim"},
		{http.MethodGet, "/api/v1/mailboxes/a@icloud.com/code"},
	} {
		resp := machineRequest(handler, request.method, request.path, "", "")
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without key: status = %d, want 401", request.method, request.path, resp.Code)
		}
		if got := resp.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s %s Cache-Control = %q, want no-store", request.method, request.path, got)
		}
		// X-API-Key 同样可以访问
		keyedReq := httptest.NewRequest(request.method, request.path, nil)
		keyedReq.Header.Set("X-API-Key", machineTestAPIKey)
		keyed := httptest.NewRecorder()
		handler.ServeHTTP(keyed, keyedReq)
		if keyed.Code == http.StatusUnauthorized {
			t.Fatalf("%s %s with X-API-Key should not be 401", request.method, request.path)
		}
	}
}

func TestMachineClaimAllocatesOnce(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	createdAt := time.Now().Add(-time.Hour)
	machineSeedMailbox(t, st, store.Mailbox{
		ID: "mailbox-1", AccountID: "acc-a", Address: "claim-a@icloud.com",
		ForwardToEmail: "real@example.com", Label: "自动取号", Status: "available", CreatedAt: createdAt,
	})

	// 第一次领取成功
	first := machineRequest(handler, http.MethodPost, "/api/v1/mailboxes/claim", machineTestAPIKey, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first claim status = %d, body=%s", first.Code, first.Body.String())
	}
	var data struct {
		MailboxID string    `json:"mailbox_id"`
		Email     string    `json:"email"`
		ForwardTo string    `json:"forward_to"`
		Label     string    `json:"label"`
		AccountID string    `json:"account_id"`
		CreatedAt time.Time `json:"created_at"`
		ClaimedAt time.Time `json:"claimed_at"`
	}
	machineDecodeData(t, first, &data)
	if data.MailboxID != "mailbox-1" || data.Email != "claim-a@icloud.com" ||
		data.ForwardTo != "real@example.com" || data.Label != "自动取号" || data.AccountID != "acc-a" {
		t.Fatalf("unexpected claim payload: %s", first.Body.String())
	}
	if !data.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at = %v, want %v", data.CreatedAt, createdAt)
	}
	if data.ClaimedAt.IsZero() {
		t.Fatalf("claimed_at missing: %s", first.Body.String())
	}

	// 第二次领取: 无可用邮箱 → 409
	second := machineRequest(handler, http.MethodPost, "/api/v1/mailboxes/claim", machineTestAPIKey, "")
	if second.Code != http.StatusConflict {
		t.Fatalf("second claim status = %d, want 409; body=%s", second.Code, second.Body.String())
	}

	// 邮箱已被标记 reserved (参考项目 "标记 used" 语义)
	reserved, err := st.ListMailboxes(context.Background(), "reserved", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(reserved) != 1 || reserved[0].ID != "mailbox-1" {
		t.Fatalf("expected mailbox-1 reserved, got %+v", reserved)
	}
}

func TestMachineClaimFiltersAccountAndKeyword(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	now := time.Now()
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-a1", AccountID: "acc-a", Address: "a1@icloud.com", Label: "shop", Status: "available", CreatedAt: now.Add(-3 * time.Hour)})
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-a2", AccountID: "acc-a", Address: "a2@icloud.com", Label: "dev", Status: "available", CreatedAt: now.Add(-2 * time.Hour)})
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-b1", AccountID: "acc-b", Address: "b1@icloud.com", Label: "shop", Status: "available", CreatedAt: now.Add(-time.Hour)})

	claim := func(body string) (int, string) {
		resp := machineRequest(handler, http.MethodPost, "/api/v1/mailboxes/claim", machineTestAPIKey, body)
		var data struct {
			Email string `json:"email"`
		}
		if resp.Code == http.StatusOK {
			machineDecodeData(t, resp, &data)
		}
		return resp.Code, data.Email
	}

	// account_ids 过滤
	if code, email := claim(`{"account_ids":["acc-b"]}`); code != http.StatusOK || email != "b1@icloud.com" {
		t.Fatalf("account_ids filter: status=%d email=%q", code, email)
	}
	// account_ids + keyword 过滤 (忽略大小写)
	if code, email := claim(`{"account_ids":["acc-a"],"keyword":"SHOP"}`); code != http.StatusOK || email != "a1@icloud.com" {
		t.Fatalf("account_ids+keyword filter: status=%d email=%q", code, email)
	}
	// 剩余邮箱按 keyword 过滤
	if code, email := claim(`{"keyword":"dev"}`); code != http.StatusOK || email != "a2@icloud.com" {
		t.Fatalf("keyword filter: status=%d email=%q", code, email)
	}
	// 全部领完 → 409
	if code, _ := claim(`{"keyword":"nothing"}`); code != http.StatusConflict {
		t.Fatalf("exhausted claim: status=%d, want 409", code)
	}
	// 非法 JSON → 400
	resp := machineRequest(handler, http.MethodPost, "/api/v1/mailboxes/claim", machineTestAPIKey, `{not-json`)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", resp.Code)
	}
}

func TestMachineCodeExtractsVerificationCode(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	now := time.Now()
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-code", AccountID: "acc-a", Address: "code@icloud.com", Status: "available", CreatedAt: now.Add(-time.Hour)})
	machineSeedMessage(t, st, store.Message{
		ID: "msg-1", MailboxID: "mailbox-code", AccountID: "acc-a", ProviderMessageID: "provider-1",
		Sender: "Shop <noreply@shop.example>", Recipient: "code@icloud.com",
		Subject: "Login verification", BodyText: "Your verification code is 123456. It expires in 10 minutes.",
		ReceivedAt: now.Add(-2 * time.Minute),
	})

	resp := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/code@icloud.com/code", machineTestAPIKey, "")
	if resp.Code != http.StatusOK {
		t.Fatalf("code status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var data struct {
		Email      string    `json:"email"`
		Code       string    `json:"code"`
		Keyword    string    `json:"keyword"`
		MessageID  string    `json:"message_id"`
		ReceivedAt time.Time `json:"received_at"`
		Subject    string    `json:"subject"`
		From       string    `json:"from"`
		Fragment   string    `json:"matched_fragment"`
	}
	machineDecodeData(t, resp, &data)
	if data.Code != "123456" {
		t.Fatalf("code = %q, want 123456", data.Code)
	}
	if data.Email != "code@icloud.com" || data.MessageID != "msg-1" ||
		data.Subject != "Login verification" || data.From != "Shop <noreply@shop.example>" {
		t.Fatalf("unexpected code payload: %s", resp.Body.String())
	}
	if data.ReceivedAt.IsZero() {
		t.Fatalf("received_at missing: %s", resp.Body.String())
	}
	if !strings.Contains(strings.ToLower(data.Fragment), "code is 123456") {
		t.Fatalf("matched_fragment = %q, want to contain %q", data.Fragment, "code is 123456")
	}

	// 邮箱不区分大小写
	upper := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/CODE@ICLOUD.COM/code", machineTestAPIKey, "")
	if upper.Code != http.StatusOK {
		t.Fatalf("case-insensitive email status = %d, body=%s", upper.Code, upper.Body.String())
	}

	// 库存外的邮箱 → 404
	missing := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/nobody@icloud.com/code", machineTestAPIKey, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown mailbox status = %d, want 404", missing.Code)
	}
}

func TestMachineCodeKeywordFilter(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	now := time.Now()
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-kw", AccountID: "acc-a", Address: "kw@icloud.com", Status: "available", CreatedAt: now.Add(-time.Hour)})
	machineSeedMessage(t, st, store.Message{
		ID: "msg-shop", MailboxID: "mailbox-kw", AccountID: "acc-a", ProviderMessageID: "provider-shop",
		Sender: "Shop <noreply@shop.example>", Recipient: "kw@icloud.com",
		Subject: "Shop order", BodyText: "Your verification code is 111111.",
		ReceivedAt: now.Add(-2 * time.Minute),
	})
	machineSeedMessage(t, st, store.Message{
		ID: "msg-news", MailboxID: "mailbox-kw", AccountID: "acc-a", ProviderMessageID: "provider-news",
		Sender: "News <noreply@news.example>", Recipient: "kw@icloud.com",
		Subject: "Weekly newsletter", BodyText: "Your verification code is 222222.",
		ReceivedAt: now.Add(-time.Minute),
	})

	get := func(query string) (int, string) {
		resp := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/kw@icloud.com/code"+query, machineTestAPIKey, "")
		var data struct {
			Code string `json:"code"`
		}
		if resp.Code == http.StatusOK {
			machineDecodeData(t, resp, &data)
		}
		return resp.Code, data.Code
	}

	if code, got := get("?keyword=newsletter"); code != http.StatusOK || got != "222222" {
		t.Fatalf("keyword=newsletter: status=%d code=%q", code, got)
	}
	if code, got := get("?keyword=SHOP"); code != http.StatusOK || got != "111111" {
		t.Fatalf("keyword=SHOP: status=%d code=%q", code, got)
	}
	if code, _ := get("?keyword=nomatch"); code != http.StatusNotFound {
		t.Fatalf("keyword=nomatch: status=%d, want 404", code)
	}
}

func TestMachineCodeAfterFilter(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	now := time.Now()
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-after", AccountID: "acc-a", Address: "after@icloud.com", Status: "available", CreatedAt: now.Add(-time.Hour)})
	machineSeedMessage(t, st, store.Message{
		ID: "msg-old", MailboxID: "mailbox-after", AccountID: "acc-a", ProviderMessageID: "provider-old",
		Sender: "Old <noreply@old.example>", Recipient: "after@icloud.com",
		Subject: "Old code", BodyText: "Your verification code is 111111.",
		ReceivedAt: now.Add(-10 * time.Minute),
	})
	machineSeedMessage(t, st, store.Message{
		ID: "msg-new", MailboxID: "mailbox-after", AccountID: "acc-a", ProviderMessageID: "provider-new",
		Sender: "New <noreply@new.example>", Recipient: "after@icloud.com",
		Subject: "New code", BodyText: "Your verification code is 222222.",
		ReceivedAt: now.Add(-time.Minute),
	})

	get := func(query string) (int, string) {
		resp := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/after@icloud.com/code"+query, machineTestAPIKey, "")
		var data struct {
			Code string `json:"code"`
		}
		if resp.Code == http.StatusOK {
			machineDecodeData(t, resp, &data)
		}
		return resp.Code, data.Code
	}

	after := url.QueryEscape(now.Add(-5 * time.Minute).Format(time.RFC3339))
	if code, got := get("?after=" + after); code != http.StatusOK || got != "222222" {
		t.Fatalf("after filter: status=%d code=%q", code, got)
	}
	if code, _ := get("?after=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339))); code != http.StatusNotFound {
		t.Fatalf("after in future: status=%d, want 404", code)
	}
	if code, _ := get("?after=not-a-time"); code != http.StatusBadRequest {
		t.Fatalf("invalid after: status=%d, want 400", code)
	}
	if code, _ := get("?wait_ms=99999"); code != http.StatusBadRequest {
		t.Fatalf("invalid wait_ms: status=%d, want 400", code)
	}
}

func TestMachineCodeWaitTimeout(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-timeout", AccountID: "acc-a", Address: "timeout@icloud.com", Status: "available", CreatedAt: time.Now().Add(-time.Hour)})

	started := time.Now()
	resp := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/timeout@icloud.com/code?wait_ms=1100", machineTestAPIKey, "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("timeout status = %d, want 404; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "暂未收到验证码邮件") {
		t.Fatalf("timeout message missing: %s", resp.Body.String())
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
}

func TestMachineCodeWaitSeesNewMessage(t *testing.T) {
	srv, st := newMachineTestServer(t)
	handler := machineTestRouter(srv)
	machineSeedMailbox(t, st, store.Mailbox{ID: "mailbox-wait", AccountID: "acc-a", Address: "wait@icloud.com", Status: "available", CreatedAt: time.Now().Add(-time.Hour)})

	type outcome struct {
		code int
		body string
	}
	done := make(chan outcome, 1)
	go func() {
		resp := machineRequest(handler, http.MethodGet, "/api/v1/mailboxes/wait@icloud.com/code?wait_ms=3000", machineTestAPIKey, "")
		done <- outcome{resp.Code, resp.Body.String()}
	}()

	// 轮询期间新邮件入库, 应在超时前被取到
	time.Sleep(400 * time.Millisecond)
	machineSeedMessage(t, st, store.Message{
		ID: "msg-wait", MailboxID: "mailbox-wait", AccountID: "acc-a", ProviderMessageID: "provider-wait",
		Sender: "noreply@x.example", Recipient: "wait@icloud.com",
		Subject: "Your code", BodyText: "Your verification code is 987654.",
		ReceivedAt: time.Now(),
	})

	select {
	case got := <-done:
		if got.code != http.StatusOK || !strings.Contains(got.body, "987654") {
			t.Fatalf("wait result status=%d body=%s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after message arrived")
	}
}
