package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileStoreRecoversOrdersMessagesAndPickupHashes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fulfillment.json")
	tokenHash := sha256.Sum256([]byte("bearer-key-that-must-not-be-stored"))
	codeHash := sha256.Sum256([]byte("short-code-that-must-not-be-stored"))

	first, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.UpsertMailboxes(ctx, []Mailbox{{
		ID: "mailbox-restart", AccountID: "account-restart", Address: "Restart@iCloud.com",
		ForwardToEmail: "destination@example.com", Status: "available",
	}}); err != nil {
		t.Fatal(err)
	}
	order, err := first.AllocateMailbox(ctx, AllocateParams{
		MailboxID: "mailbox-restart", ExternalID: "order-restart",
		PickupTokenHash: tokenHash[:], PickupCodeHash: codeHash[:], ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHTML := `<html><body><div style="color:red">complete message</div></body></html>`
	if err := first.SaveMessage(ctx, Message{
		MailboxID: order.MailboxID, AccountID: "account-restart", ProviderMessageID: "provider-restart",
		Sender: "sender@example.com", Recipient: order.MailboxAddress, Subject: "Restart",
		BodyText: "complete message", BodyHTML: wantHTML, ContentType: "text/html", ReceivedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "bearer-key-that-must-not-be-stored") || strings.Contains(string(raw), "short-code-that-must-not-be-stored") {
		t.Fatal("file store contains a plaintext pickup credential")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %o, want 600", got)
		}
	}

	second, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pickup, err := second.ActivatePickup(ctx, tokenHash[:], codeHash[:])
	if err != nil {
		t.Fatal(err)
	}
	if pickup.OrderID != order.ID || pickup.MailboxAddress != "restart@icloud.com" {
		t.Fatalf("unexpected recovered pickup: %#v", pickup)
	}
	if pickup.StartsAt.IsZero() || !pickup.ExpiresAt.After(pickup.StartsAt) {
		t.Fatalf("pickup was not activated on first use: %#v", pickup)
	}
	messages, err := second.ListMessagesByOrder(ctx, order.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].BodyHTML != wantHTML || messages[0].ContentType != "text/html" {
		t.Fatalf("unexpected recovered messages: %#v", messages)
	}
	orders, err := second.ListOrders(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].ExternalID != "order-restart" {
		t.Fatalf("unexpected recovered orders: %#v", orders)
	}
	if orders[0].ActivatedAt == nil || orders[0].ExpiresAt.IsZero() {
		t.Fatalf("activation was not persisted: %#v", orders[0])
	}
}

func TestFileStoreDirectedAllocationIsAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fulfillment.json")
	st, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{
		{ID: "mailbox-target", Address: "target@icloud.com", Status: "available", CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "mailbox-other", Address: "other@icloud.com", Status: "available", CreatedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	const contenders = 12
	var successes atomic.Int32
	var wg sync.WaitGroup
	errorsSeen := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.AllocateMailbox(ctx, AllocateParams{MailboxID: "mailbox-target", ExpiresAt: time.Now().Add(time.Hour)})
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrNoInventory) {
				errorsSeen <- err
			}
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("unexpected allocation error: %v", err)
	}
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful directed allocations = %d, want 1", got)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := reopened.ListOrders(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].MailboxID != "mailbox-target" {
		t.Fatalf("unexpected persisted allocation: %#v", orders)
	}
	available, err := reopened.ListMailboxes(ctx, "available", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != "mailbox-other" {
		t.Fatalf("non-target mailbox was consumed: %#v", available)
	}
}

func TestFileStorePruneAndDeleteMailboxPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fulfillment.json")
	st, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{
		{ID: "keep", AccountID: "account-1", Address: "keep@icloud.com", AnonymousID: "anon-keep", Status: "available"},
		{ID: "ghost", AccountID: "account-1", Address: "ghost@icloud.com", AnonymousID: "anon-ghost", Status: "available"},
		{ID: "reserved", AccountID: "account-1", Address: "held@icloud.com", AnonymousID: "anon-held", Status: "reserved"},
	}); err != nil {
		t.Fatal(err)
	}

	pruned, err := st.PruneMailboxes(ctx, "account-1", []string{"keep@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	deleted, err := st.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-keep")
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := reopened.ListMailboxes(ctx, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "reserved" {
		t.Fatalf("prune/delete did not persist: %#v", remaining)
	}
}

func TestFileStoreDailyBackupCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "fulfillment.json")
	st, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{{
		ID: "mailbox-backup", AccountID: "account-backup", Address: "backup@icloud.com", Status: "available",
	}}); err != nil {
		t.Fatal(err)
	}

	backupsDir := filepath.Join(dir, "backups")
	today := time.Now().Format("20060102")
	target := filepath.Join(backupsDir, "fulfillment-"+today+".json")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("daily store backup missing after mutation: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("daily store backup is empty")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mailbox-backup") {
		t.Fatal("store backup does not contain committed data")
	}

	// 同一天再次变更：不产生第二份今日备份
	if err := st.UpsertMailboxes(ctx, []Mailbox{{
		ID: "mailbox-backup-2", AccountID: "account-backup", Address: "backup2@icloud.com", Status: "available",
	}}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(backupsDir, "fulfillment-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("store backup files after second mutation = %d, want 1 (idempotent)", len(matches))
	}
}

func TestFileStoreDailyBackupPrunesOldCopies(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "fulfillment.json")
	st, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{{
		ID: "mailbox-backup", AccountID: "account-backup", Address: "backup@icloud.com", Status: "available",
	}}); err != nil {
		t.Fatal(err)
	}
	backupsDir := filepath.Join(dir, "backups")

	// 制造 12 份历史备份，日期严格早于今天
	now := time.Now()
	var names []string
	for i := 0; i < 12; i++ {
		day := now.AddDate(0, 0, -(i + 2))
		name := "fulfillment-" + day.Format("20060102") + ".json"
		if err := os.WriteFile(filepath.Join(backupsDir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	// 再次变更触发清理：保留最近 7 份
	if err := st.UpsertMailboxes(ctx, []Mailbox{{
		ID: "mailbox-backup-3", AccountID: "account-backup", Address: "backup3@icloud.com", Status: "available",
	}}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(backupsDir, "fulfillment-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 7 {
		t.Fatalf("store backup files after prune = %d, want 7", len(matches))
	}
	for _, name := range names[6:] {
		if _, err := os.Stat(filepath.Join(backupsDir, name)); !os.IsNotExist(err) {
			t.Fatalf("old store backup %s should have been pruned", name)
		}
	}
}
