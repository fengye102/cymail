package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/fulfillment"
	mailclient "icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// writeTestAccounts 直接写 accounts.json (明文), 绕过需要真实网络连接的
// SetAppPassword/UpdateCookies, 用于构造带 IMAP 凭证的测试账号。
func writeTestAccounts(t *testing.T, dir string, accounts map[string]map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(map[string]any{"accounts": accounts}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustTestManager(t *testing.T, dir string) *account.Manager {
	t.Helper()
	mgr, err := account.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// TestStartIdleMailWatchersEligibilityAndIdempotent 覆盖启动逻辑:
//  1. 只有 有 IMAP 凭证 + 库存有该账号收件目标 的账号才启动 watcher;
//  2. watcher 拿到的是全量凭证副本 (AppPassword 可用, 可真实连接);
//  3. 重复调用 startIdleMailWatchers 不会重复启动。
func TestStartIdleMailWatchersEligibilityAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeTestAccounts(t, dir, map[string]map[string]any{
		"acc_ready": {
			"id": "acc_ready", "name": "ready", "status": "active",
			"icloud_email": "ready@icloud.com", "app_password": "secret-1",
		},
		"acc_no_password": {
			"id": "acc_no_password", "name": "no-password", "status": "active",
			"icloud_email": "np@icloud.com",
		},
		"acc_no_email": {
			"id": "acc_no_email", "name": "no-email", "status": "active",
			"app_password": "secret-2",
		},
		"acc_no_target": {
			"id": "acc_no_target", "name": "no-target", "status": "active",
			"icloud_email": "nt@icloud.com", "app_password": "secret-3",
		},
	})
	mgr := mustTestManager(t, dir)
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "mbox-ready", AccountID: "acc_ready", Address: "alias1@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
		{ID: "mbox-np", AccountID: "acc_no_password", Address: "alias2@icloud.com", ForwardToEmail: "dst@example.com", Status: "reserved"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(mgr, Config{Fulfillment: fulfillment.New(mgr, st)})
	launched := make(chan *account.Account, 8)
	srv.idleWatchRunFn = func(ctx context.Context, s *Server, accountID string, acc *account.Account) {
		launched <- acc
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.startIdleMailWatchers(watchCtx)

	// 只应有 acc_ready; 且传入的是含凭证的全量副本。
	select {
	case acc := <-launched:
		if acc.ID != "acc_ready" || acc.ICloudEmail != "ready@icloud.com" || acc.AppPassword != "secret-1" {
			t.Fatalf("unexpected launched account: %#v", acc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected watcher for acc_ready, got none")
	}
	select {
	case acc := <-launched:
		t.Fatalf("unexpected extra watcher launched: %#v", acc)
	case <-time.After(300 * time.Millisecond):
	}

	// 幂等: 再次调用 (含 backgroundOnce 语义之外的自身 once) 不重复启动。
	srv.startIdleMailWatchers(watchCtx)
	srv.startIdleMailWatchers(watchCtx)
	select {
	case acc := <-launched:
		t.Fatalf("startIdleMailWatchers not idempotent, launched again: %#v", acc)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestScanIdleWatchersPicksUpLaterTargets 覆盖补扫: 账号先有凭证但无收件目标时
// 不启动, 库存出现目标后再扫描会补开 watcher。
func TestScanIdleWatchersPicksUpLaterTargets(t *testing.T) {
	dir := t.TempDir()
	writeTestAccounts(t, dir, map[string]map[string]any{
		"acc_late": {
			"id": "acc_late", "name": "late", "status": "active",
			"icloud_email": "late@icloud.com", "app_password": "secret",
		},
	})
	mgr := mustTestManager(t, dir)
	st := store.NewMemory()
	srv := NewWithConfig(mgr, Config{Fulfillment: fulfillment.New(mgr, st)})
	launched := make(chan string, 2)
	srv.idleWatchRunFn = func(ctx context.Context, s *Server, accountID string, acc *account.Account) {
		launched <- accountID
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.scanIdleWatchers(ctx)
	select {
	case id := <-launched:
		t.Fatalf("watcher launched without collect targets: %s", id)
	case <-time.After(300 * time.Millisecond):
	}

	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "mbox-late", AccountID: "acc_late", Address: "late@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.scanIdleWatchers(ctx)
	select {
	case id := <-launched:
		if id != "acc_late" {
			t.Fatalf("unexpected watcher: %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected watcher for acc_late after targets appeared")
	}
	// 再次扫描 (幂等) 不重复启动。
	srv.scanIdleWatchers(ctx)
	select {
	case id := <-launched:
		t.Fatalf("scan not idempotent, launched again: %s", id)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestStartBackgroundLoopsStartsIdleWatchers 验证 startBackgroundLoops 已挂接
// startIdleMailWatchers (Run/RunTLS 的入口)。
func TestStartBackgroundLoopsStartsIdleWatchers(t *testing.T) {
	dir := t.TempDir()
	writeTestAccounts(t, dir, map[string]map[string]any{
		"acc_ready": {
			"id": "acc_ready", "name": "ready", "status": "active",
			"icloud_email": "ready@icloud.com", "app_password": "secret-1",
		},
	})
	mgr := mustTestManager(t, dir)
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "mbox-ready", AccountID: "acc_ready", Address: "alias1@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(mgr, Config{Fulfillment: fulfillment.New(mgr, st)})
	launched := make(chan string, 2)
	srv.idleWatchRunFn = func(ctx context.Context, s *Server, accountID string, acc *account.Account) {
		launched <- accountID
	}
	srv.startBackgroundLoops()
	select {
	case id := <-launched:
		if id != "acc_ready" {
			t.Fatalf("unexpected watcher: %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startBackgroundLoops did not start idle watchers")
	}
}

// TestIdleHandleBatchFiltersDedupesAndSaves 覆盖回调入库:
//   - 只入库属于该账号目标集合的邮件 (按收件人过滤, 兼容 "Name <addr>" 形式);
//   - 一封邮件可匹配多个目标, 每个目标各存一行;
//   - 同一批 (重叠回扫/重试重放) 再次回调不重复入库;
//   - 转换复用 fulfillment.ConvertIMAPMessage: OTP 提取、日期解析与入库格式一致。
func TestIdleHandleBatchFiltersDedupesAndSaves(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "mbox-a", AccountID: "acc-1", Address: "alias-a@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
		{ID: "mbox-b", AccountID: "acc-1", Address: "alias-b@icloud.com", ForwardToEmail: "dst@example.com", Status: "reserved"},
		{ID: "mbox-other", AccountID: "acc-2", Address: "other@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	mgr := mustTestManager(t, t.TempDir())
	srv := NewWithConfig(mgr, Config{Fulfillment: fulfillment.New(mgr, st)})
	state := newIdleAccountState()

	batch := []mailclient.Message{
		{ID: "101", From: "no-reply@openai.com", To: "alias-a@icloud.com", Subject: "Your code", Preview: "verification 123456", Date: "2026-01-02T03:04:05Z"},
		{ID: "102", From: "chatgpt@openai.com", To: "ChatGPT <alias-b@icloud.com>", Subject: "Login", Preview: "code 888888", Date: "2026-01-02T03:05:00Z"},
		{ID: "103", From: "x@y.com", To: "other@icloud.com", Subject: "other account", Preview: "nope"},                 // 属于 acc-2, 本批 (acc-1) 不应入库
		{ID: "104", From: "z@y.com", To: "alias-a@icloud.com, alias-b@icloud.com", Subject: "multi", Preview: "no otp"}, // 同时匹配两个目标
		{ID: "105", From: "spam@y.com", To: "unrelated@example.com", Subject: "spam", Preview: "spam"},                  // 不属于任何目标
	}
	if err := srv.idleHandleBatch(ctx, "acc-1", state, batch); err != nil {
		t.Fatal(err)
	}
	messages, err := st.ListMessages(ctx, "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("saved %d messages, want 4 (101→mbox-a, 102→mbox-b, 104→mbox-a+mbox-b)", len(messages))
	}
	byKey := make(map[string]store.Message, len(messages))
	for _, m := range messages {
		byKey[m.MailboxID+"\x00"+m.ProviderMessageID] = m
	}
	if got := byKey["mbox-a\x00101"]; got.MailboxID != "mbox-a" || got.OTPCode != "123456" || got.Sender != "no-reply@openai.com" {
		t.Fatalf("101 row wrong: %#v", got)
	}
	if got := byKey["mbox-b\x00102"]; got.OTPCode != "888888" || got.Recipient != "ChatGPT <alias-b@icloud.com>" {
		t.Fatalf("102 row wrong: %#v", got)
	}
	if got := byKey["mbox-a\x00104"]; got.MailboxID != "mbox-a" {
		t.Fatalf("104 mbox-a row missing: %#v", got)
	}
	if got := byKey["mbox-b\x00104"]; got.MailboxID != "mbox-b" {
		t.Fatalf("104 mbox-b row missing: %#v", got)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !byKey["mbox-a\x00101"].ReceivedAt.Equal(want) {
		t.Fatalf("ReceivedAt = %v, want %v", byKey["mbox-a\x00101"].ReceivedAt, want)
	}
	if _, ok := byKey["mbox-other\x00103"]; ok {
		t.Fatal("message for acc-2 leaked into acc-1 batch")
	}

	// 同一批再次回调 (重叠回扫/重试重放): 全部去重, 不新增行。
	if err := srv.idleHandleBatch(ctx, "acc-1", state, batch); err != nil {
		t.Fatal(err)
	}
	messages, err = st.ListMessages(ctx, "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("after replay saved %d messages, want 4 (dedup failed)", len(messages))
	}
}

// failingSaveStore 包装 memory store, SaveMessage 恒失败, 验证回调错误上抛。
type failingSaveStore struct {
	store.Store
}

func (f *failingSaveStore) SaveMessage(context.Context, store.Message) error {
	return errors.New("store down")
}

func TestIdleHandleBatchSaveFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	st := &failingSaveStore{Store: store.NewMemory()}
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "mbox-a", AccountID: "acc-1", Address: "alias-a@icloud.com", ForwardToEmail: "dst@example.com", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	mgr := mustTestManager(t, t.TempDir())
	srv := NewWithConfig(mgr, Config{Fulfillment: fulfillment.New(mgr, st)})
	state := newIdleAccountState()
	err := srv.idleHandleBatch(ctx, "acc-1", state, []mailclient.Message{
		{ID: "101", To: "alias-a@icloud.com", Subject: "s"},
	})
	if err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("expected store error to abort the batch, got %v", err)
	}
}
