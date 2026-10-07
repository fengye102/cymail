package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	"icloud-hme/internal/store"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewWithConfig(mgr, cfg)
}

func TestAPIAuthentication(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-api-key"})
	tests := []struct {
		name   string
		header string
		value  string
		status int
	}{
		{"missing", "", "", http.StatusUnauthorized},
		{"wrong", "Authorization", "Bearer wrong", http.StatusUnauthorized},
		{"bearer", "Authorization", "Bearer test-api-key", http.StatusOK},
		{"custom-header", "X-API-Key", "test-api-key", http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
			if test.header != "" {
				req.Header.Set(test.header, test.value)
			}
			resp := httptest.NewRecorder()
			srv.Handler().ServeHTTP(resp, req)
			if resp.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", resp.Code, test.status, resp.Body.String())
			}
			if got := resp.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestInboxQueryLimits(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "key"})
	req := httptest.NewRequest(http.MethodGet, "/api/inbox?account_id=missing&limit=201", nil)
	req.Header.Set("Authorization", "Bearer key")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

func TestRequestBodyLimit(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "key", MaxBodyBytes: 32})
	body := strings.NewReader(`{"name":"` + strings.Repeat("x", 128) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", body)
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

func TestAliasRateLimitResponseIsStructured(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	failAliasRateLimited(context, 90*time.Second, "temporarily limited")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want 90", got)
	}
	var payload apiResp
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != "alias_rate_limited" || payload.RetryAfterSeconds != 90 {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestCreateAliasRejectsInvalidCustomInterval(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-api-key"})
	body := strings.NewReader(`{"account_id":"account-a","interval_seconds":86401}`)
	req := httptest.NewRequest(http.MethodPost, "/api/create", body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

func TestBackendDoesNotServeFrontend(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-api-key"})
	for _, path := range []string{"/admin", "/pickup", "/app.js", "/styles.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		srv.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d, want 404", path, resp.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestBrowserAuthorizationStartAndStatus(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-api-key"})

	create := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"name":"browser-auth-test"}`))
	create.Header.Set("Authorization", "Bearer test-api-key")
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	srv.Handler().ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create account status=%d body=%s", created.Code, created.Body.String())
	}
	var createdPayload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdPayload); err != nil {
		t.Fatal(err)
	}

	start := httptest.NewRequest(http.MethodPost, "/api/accounts/"+createdPayload.Data.ID+"/browser-auth/start", strings.NewReader(`{}`))
	start.Header.Set("Authorization", "Bearer test-api-key")
	start.Header.Set("Content-Type", "application/json")
	started := httptest.NewRecorder()
	srv.Handler().ServeHTTP(started, start)
	if started.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", started.Code, started.Body.String())
	}
	var startPayload struct {
		Data struct {
			RequestID         string `json:"request_id"`
			AuthorizationCode string `json:"authorization_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &startPayload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(startPayload.Data.AuthorizationCode, "iba_") || startPayload.Data.RequestID == "" {
		t.Fatalf("invalid browser authorization payload: %s", started.Body.String())
	}

	status := httptest.NewRequest(http.MethodGet, "/api/accounts/"+createdPayload.Data.ID+"/browser-auth/status?request_id="+startPayload.Data.RequestID, nil)
	status.Header.Set("Authorization", "Bearer test-api-key")
	checked := httptest.NewRecorder()
	srv.Handler().ServeHTTP(checked, status)
	if checked.Code != http.StatusOK || !strings.Contains(checked.Body.String(), `"status":"pending"`) {
		t.Fatalf("status=%d body=%s", checked.Code, checked.Body.String())
	}
}

func TestBrowserAuthorizationCompletionRejectsUnknownCode(t *testing.T) {
	srv := newTestServer(t, Config{APIKey: "test-api-key"})
	req := httptest.NewRequest(http.MethodPost, "/public/browser-auth/complete", strings.NewReader(`{"authorization_code":"iba_unknown","cookies":{"session":"secret"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
}

func TestAllocateBatchUsesSelectedMailboxes(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	if err := st.UpsertMailboxes(context.Background(), []store.Mailbox{
		{ID: "mailbox-a", Address: "a@icloud.com", Status: "available", CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "mailbox-b", Address: "b@icloud.com", Status: "available", CreatedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(mgr, Config{APIKey: "key", Fulfillment: fulfillment.New(mgr, st)})
	req := httptest.NewRequest(http.MethodPost, "/api/orders/allocate-batch", strings.NewReader(`{"mailbox_ids":["mailbox-b"],"ttl_hours":24}`))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.Code)
	}
	var payload struct {
		Data struct {
			Count      int `json:"count"`
			Deliveries []struct {
				Email string `json:"email"`
			} `json:"deliveries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.Count != 1 || len(payload.Data.Deliveries) != 1 || payload.Data.Deliveries[0].Email != "b@icloud.com" {
		t.Fatalf("batch did not allocate selected mailbox")
	}
}

// TestDeleteAliasInventoryCleanup covers the exact store-cleanup path
// DELETE /api/aliases/:id (deleteAlias) runs after the iCloud-side delete
// succeeds: the matching available row must disappear from the inventory while
// reserved/disabled/retired rows and other accounts stay untouched.
func TestDeleteAliasInventoryCleanup(t *testing.T) {
	ctx := context.Background()
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "target", AccountID: "account-a", Address: "gone@icloud.com", AnonymousID: "anon-deleted", Status: "available"},
		{ID: "reserved", AccountID: "account-a", Address: "held@icloud.com", AnonymousID: "anon-held", Status: "reserved"},
		{ID: "disabled", AccountID: "account-a", Address: "off@icloud.com", AnonymousID: "anon-off", Status: "disabled"},
		{ID: "other-account", AccountID: "account-b", Address: "other@icloud.com", AnonymousID: "anon-deleted", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(mgr, Config{APIKey: "key", Fulfillment: fulfillment.New(mgr, st)})

	if err := srv.removeAliasFromInventory(ctx, "account-a", "anon-deleted"); err != nil {
		t.Fatal(err)
	}

	remaining, err := st.ListMailboxes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]store.Mailbox, len(remaining))
	for _, mailbox := range remaining {
		byID[mailbox.ID] = mailbox
	}
	if _, ok := byID["target"]; ok {
		t.Fatal("deleted alias row still present in inventory")
	}
	if _, ok := byID["other-account"]; !ok {
		t.Fatal("other account row was removed")
	}
	for _, id := range []string{"reserved", "disabled"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("protected row %s was removed", id)
		}
	}

	// Second call (e.g. retry after partial failure) is a no-op, not an error.
	if err := srv.removeAliasFromInventory(ctx, "account-a", "anon-deleted"); err != nil {
		t.Fatalf("repeat cleanup errored: %v", err)
	}
}

func TestDeleteAliasInventoryCleanupSkipsWithoutStore(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(mgr, Config{APIKey: "key"})
	if srv.fulfillment != nil {
		t.Fatal("test server unexpectedly has a fulfillment service")
	}
	if err := srv.removeAliasFromInventory(context.Background(), "account-a", "anon-deleted"); err != nil {
		t.Fatalf("cleanup without a store should be a no-op, got: %v", err)
	}
}
