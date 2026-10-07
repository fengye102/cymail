package fulfillment

import (
	"context"
	"testing"
	"time"

	mailclient "icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestConvertIMAPMessage 验证 IMAP 增量回调邮件被转换为与 CollectFiltered 一致
// 的入库格式: ProviderMessageID 取 IMAP UID, OTP 提取与日期解析复用 convertMessage。
func TestConvertIMAPMessage(t *testing.T) {
	target := store.CollectTarget{MailboxID: "mbox-1", AccountID: "acc-1", MailboxAddress: "alias@icloud.com", ForwardToEmail: "dst@example.com"}
	msg := mailclient.Message{
		ID: "412", From: "no-reply@openai.com", To: "alias@icloud.com",
		Subject: "Your code", Preview: "verification 654321", BodyHTML: "<p>x</p>",
		ContentType: "text/html", Date: "2026-02-03T04:05:06Z",
	}
	got := ConvertIMAPMessage(target, msg)
	if got.MailboxID != "mbox-1" || got.AccountID != "acc-1" || got.ProviderMessageID != "412" {
		t.Fatalf("identity fields wrong: %#v", got)
	}
	if got.Sender != msg.From || got.Recipient != msg.To || got.Subject != msg.Subject ||
		got.BodyText != msg.Preview || got.BodyHTML != msg.BodyHTML || got.ContentType != msg.ContentType {
		t.Fatalf("conversion fields wrong: %#v", got)
	}
	if got.OTPCode != "654321" {
		t.Fatalf("OTP not extracted: %q", got.OTPCode)
	}
	want := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if !got.ReceivedAt.Equal(want) {
		t.Fatalf("ReceivedAt = %v, want %v", got.ReceivedAt, want)
	}
}

// TestServiceSaveMessageIdempotent 验证 SaveMessage 按 (account_id,
// provider_message_id, mailbox_id) 幂等: 同一封重复入库只保留一行。
func TestServiceSaveMessageIdempotent(t *testing.T) {
	st := store.NewMemory()
	svc := New(nil, st)
	msg := store.Message{MailboxID: "mbox-1", AccountID: "acc-1", ProviderMessageID: "101", Sender: "s", Recipient: "r", Subject: "sub", ReceivedAt: time.Now()}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := svc.SaveMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.ListMessages(ctx, "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("SaveMessage not idempotent: %d rows for one message", len(all))
	}
}

// TestServiceCollectTargets 验证 CollectTargets 只返回 available/reserved 邮箱,
// 与 CollectFiltered 的收集范围一致。
func TestServiceCollectTargets(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "m1", AccountID: "a1", Address: "x@icloud.com", Status: "available"},
		{ID: "m2", AccountID: "a1", Address: "y@icloud.com", Status: "reserved"},
		{ID: "m3", AccountID: "a1", Address: "z@icloud.com", Status: "disabled"},
		{ID: "m4", AccountID: "a1", Address: "w@icloud.com", Status: "retired"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, st)
	targets, err := svc.CollectTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (available + reserved only)", len(targets))
	}
}
