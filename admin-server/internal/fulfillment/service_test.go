package fulfillment

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

type fakeStore struct {
	params      store.AllocateParams
	messages    []store.Message
	activatedAt *time.Time
	expiresAt   time.Time

	pruneErr      error
	pruneAccount  string
	pruneKeep     []string
	deleteAccount string
	deleteAnon    string
	deleted       bool
}

func (f *fakeStore) Migrate(context.Context) error                          { return nil }
func (f *fakeStore) UpsertMailboxes(context.Context, []store.Mailbox) error { return nil }
func (f *fakeStore) PruneMailboxes(_ context.Context, accountID string, keepAddresses []string) (int, error) {
	f.pruneAccount = accountID
	f.pruneKeep = append([]string(nil), keepAddresses...)
	return 0, f.pruneErr
}
func (f *fakeStore) DeleteMailboxByAnonymousID(_ context.Context, accountID, anonymousID string) (bool, error) {
	f.deleteAccount = accountID
	f.deleteAnon = anonymousID
	return f.deleted, nil
}
func (f *fakeStore) ListMailboxes(context.Context, string, int) ([]store.Mailbox, error) {
	return nil, nil
}
func (f *fakeStore) AllocateMailbox(_ context.Context, params store.AllocateParams) (*store.Order, error) {
	f.params = params
	return &store.Order{ID: "order-1", MailboxID: "mailbox-1", MailboxAddress: "buyer@icloud.com", Status: "active", ExpiresAt: params.ExpiresAt}, nil
}
func (f *fakeStore) ListOrders(context.Context, int) ([]store.Order, error) { return nil, nil }
func (f *fakeStore) ReissueOrderPickup(_ context.Context, orderID string, tokenHash, codeHash []byte, expiresAt time.Time) (*store.Order, error) {
	f.params.PickupTokenHash = append([]byte(nil), tokenHash...)
	f.params.PickupCodeHash = append([]byte(nil), codeHash...)
	f.params.ValidFor = time.Until(expiresAt)
	f.activatedAt = nil
	f.expiresAt = time.Time{}
	return &store.Order{ID: orderID, MailboxID: "mailbox-1", MailboxAddress: "buyer@icloud.com", Status: "active", ValidForSeconds: int64(f.params.ValidFor / time.Second)}, nil
}
func (f *fakeStore) ActivatePickup(_ context.Context, tokenHash, codeHash []byte) (*store.Pickup, error) {
	if string(tokenHash) != string(f.params.PickupTokenHash) || string(codeHash) != string(f.params.PickupCodeHash) {
		return nil, errors.New("not found")
	}
	if f.activatedAt == nil {
		now := time.Now()
		validFor := f.params.ValidFor
		if validFor <= 0 {
			validFor = 24 * time.Hour
		}
		f.activatedAt = &now
		f.expiresAt = now.Add(validFor)
	}
	return &store.Pickup{OrderID: "order-1", MailboxID: "mailbox-1", MailboxAddress: "buyer@icloud.com", PickupCodeHash: f.params.PickupCodeHash, StartsAt: *f.activatedAt, ExpiresAt: f.expiresAt}, nil
}
func (f *fakeStore) SaveMessage(context.Context, store.Message) error { return nil }
func (f *fakeStore) ListMessages(context.Context, string, string, string, int) ([]store.Message, error) {
	return f.messages, nil
}
func (f *fakeStore) ListMessagesByOrder(context.Context, string, int) ([]store.Message, error) {
	return f.messages, nil
}
func (f *fakeStore) ListCollectTargets(context.Context) ([]store.CollectTarget, error) {
	return nil, nil
}
func (f *fakeStore) Stats(context.Context) (*store.Stats, error) { return &store.Stats{}, nil }
func (f *fakeStore) ExpireOrders(context.Context) error          { return nil }
func (f *fakeStore) Close()                                      {}

func TestAllocateAndPickup(t *testing.T) {
	st := &fakeStore{messages: []store.Message{{ID: "message-1", OTPCode: "482911"}}}
	svc := New(nil, st)
	order, token, code, err := svc.Allocate(context.Background(), "shop-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if order.MailboxAddress == "" || len(token) < 40 || len(code) != 6 {
		t.Fatalf("invalid delivery data: %#v token=%q code=%q", order, token, code)
	}
	pickupOrder, messages, err := svc.Pickup(context.Background(), token, code)
	if err != nil {
		t.Fatal(err)
	}
	if pickupOrder.MailboxAddress != order.MailboxAddress || len(messages) != 1 {
		t.Fatalf("unexpected pickup: %#v %#v", pickupOrder, messages)
	}
	if _, _, err := svc.Pickup(context.Background(), token, "000000"); !errors.Is(err, ErrInvalidPickup) {
		t.Fatalf("wrong code error = %v", err)
	}
}

func TestAllocateAndPickupWithLongKey(t *testing.T) {
	st := &fakeStore{messages: []store.Message{{ID: "message-1", OTPCode: "482911"}}}
	svc := New(nil, st)
	order, key, err := svc.AllocateWithPickupKey(context.Background(), "shop-key-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if order.MailboxAddress == "" || len(key) < 60 || key[:4] != "tok_" {
		t.Fatalf("invalid key delivery data: %#v key=%q", order, key)
	}
	if !order.ExpiresAt.IsZero() || order.ActivatedAt != nil {
		t.Fatalf("pickup validity started at issuance: %#v", order)
	}
	issuedAt := time.Now()
	pickupOrder, messages, err := svc.PickupKey(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if pickupOrder.MailboxAddress != order.MailboxAddress || len(messages) != 1 {
		t.Fatalf("unexpected key pickup: %#v %#v", pickupOrder, messages)
	}
	if pickupOrder.CreatedAt.Before(issuedAt) || pickupOrder.ExpiresAt.Sub(pickupOrder.CreatedAt) < 59*time.Minute {
		t.Fatalf("pickup window did not start on first use: %#v", pickupOrder)
	}
	if _, _, err := svc.PickupKey(context.Background(), "tok_invalid"); !errors.Is(err, ErrInvalidPickup) {
		t.Fatalf("wrong key error = %v", err)
	}
}

func TestAllocateSpecificMailboxWithLongKey(t *testing.T) {
	st := &fakeStore{}
	svc := New(nil, st)
	_, _, err := svc.AllocateMailboxWithPickupKey(context.Background(), "mailbox-selected", "shop-selected", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st.params.MailboxID != "mailbox-selected" {
		t.Fatalf("mailbox id = %q, want mailbox-selected", st.params.MailboxID)
	}
}

func TestReissuePickupKeyInvalidatesPreviousKey(t *testing.T) {
	st := &fakeStore{messages: []store.Message{{ID: "message-1"}}}
	svc := New(nil, st)
	_, oldKey, err := svc.AllocateWithPickupKey(context.Background(), "shop-key-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	order, newKey, err := svc.ReissuePickupKey(context.Background(), "order-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if newKey == oldKey || order.MailboxAddress == "" {
		t.Fatalf("invalid reissue result: %#v key=%q", order, newKey)
	}
	if _, _, err := svc.PickupKey(context.Background(), oldKey); !errors.Is(err, ErrInvalidPickup) {
		t.Fatalf("old key should be invalid, got %v", err)
	}
	if _, _, err := svc.PickupKey(context.Background(), newKey); err != nil {
		t.Fatalf("new key should work: %v", err)
	}
}

func TestExtractOTP(t *testing.T) {
	for input, want := range map[string]string{
		"Your verification code is 123456": "123456",
		"验证码：8391，请勿泄露":                    "8391",
		"Login 772244 expires soon":        "772244",
	} {
		if got := extractOTP(input); got != want {
			t.Fatalf("extractOTP(%q)=%q want %q", input, got, want)
		}
	}
}

func TestSyncAccountAliasesPrunesGhostRows(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	// Seed inventory as it would look before the fix: ghosts (deleted on
	// iCloud but still available in store) mixed with live and protected rows.
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "ghost-1", AccountID: "account-1", Address: "ghost1@icloud.com", AnonymousID: "anon-ghost-1", Status: "available"},
		{ID: "ghost-2", AccountID: "account-1", Address: "Ghost2@icloud.com", AnonymousID: "anon-ghost-2", Status: "available"},
		{ID: "live-1", AccountID: "account-1", Address: "live1@icloud.com", AnonymousID: "anon-live-1", Status: "available"},
		{ID: "live-2", AccountID: "account-1", Address: "LIVE2@icloud.com", AnonymousID: "anon-live-2", Status: "available"},
		{ID: "reserved-1", AccountID: "account-1", Address: "reserved1@icloud.com", AnonymousID: "anon-reserved-1", Status: "reserved"},
		{ID: "disabled-1", AccountID: "account-1", Address: "disabled1@icloud.com", AnonymousID: "anon-disabled-1", Status: "disabled"},
		{ID: "retired-1", AccountID: "account-1", Address: "retired1@icloud.com", AnonymousID: "anon-retired-1", Status: "retired"},
		{ID: "other-account", AccountID: "account-2", Address: "other@icloud.com", AnonymousID: "anon-other", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(nil, st)
	count, err := svc.syncAccountAliases(ctx, "account-1", &hme.ForwardingSettings{
		SelectedForwardTo: "dst@example.com",
		Aliases: []hme.Alias{
			{Email: "live1@icloud.com", AnonymousID: "anon-live-1", Active: true, CreatedAt: "2024-01-01T00:00:00Z"},
			{Email: "LIVE2@icloud.com", AnonymousID: "anon-live-2", Active: true, CreatedAt: "2024-01-02T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("synced count = %d, want 2", count)
	}

	all, err := st.ListMailboxes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	remaining := make(map[string]string, len(all))
	for _, mailbox := range all {
		remaining[mailbox.ID] = mailbox.Status
	}
	for _, ghost := range []string{"ghost-1", "ghost-2"} {
		if _, ok := remaining[ghost]; ok {
			t.Fatalf("ghost row %s was not pruned", ghost)
		}
	}
	for id, wantStatus := range map[string]string{
		"live-1":        "available",
		"live-2":        "available",
		"reserved-1":    "reserved",
		"disabled-1":    "disabled",
		"retired-1":     "retired",
		"other-account": "available",
	} {
		if got, ok := remaining[id]; !ok || got != wantStatus {
			t.Fatalf("row %s status = %q, want %q (must be preserved)", id, got, wantStatus)
		}
	}
}

func TestSyncAccountAliasesPruneFailureDoesNotFailSync(t *testing.T) {
	st := &fakeStore{pruneErr: errors.New("store down")}
	svc := New(nil, st)
	count, err := svc.syncAccountAliases(context.Background(), "account-1", &hme.ForwardingSettings{
		SelectedForwardTo: "dst@example.com",
		Aliases: []hme.Alias{
			{Email: "live1@icloud.com", AnonymousID: "anon-live-1", Active: true},
			{Email: "off1@icloud.com", AnonymousID: "anon-off-1", Active: false},
		},
	})
	if err != nil {
		t.Fatalf("prune failure must not fail the sync: %v", err)
	}
	if count != 2 {
		t.Fatalf("synced count = %d, want 2", count)
	}
	if st.pruneAccount != "account-1" {
		t.Fatalf("prune account = %q, want account-1", st.pruneAccount)
	}
	if len(st.pruneKeep) != 2 || st.pruneKeep[0] != "live1@icloud.com" || st.pruneKeep[1] != "off1@icloud.com" {
		t.Fatalf("prune keep addresses = %#v, want live+off aliases", st.pruneKeep)
	}
}

func TestServiceDeleteMailboxByAnonymousIDRemovesRow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.UpsertMailboxes(ctx, []store.Mailbox{
		{ID: "target", AccountID: "account-1", Address: "gone@icloud.com", AnonymousID: "anon-gone", Status: "available"},
		{ID: "reserved", AccountID: "account-1", Address: "held@icloud.com", AnonymousID: "anon-held", Status: "reserved"},
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, st)

	deleted, err := svc.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-gone")
	if err != nil || !deleted {
		t.Fatalf("first delete = (%v, %v), want (true, nil)", deleted, err)
	}
	mailboxes, err := st.ListMailboxes(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range mailboxes {
		if mailbox.ID == "target" {
			t.Fatalf("deleted row still present: %#v", mailbox)
		}
		if mailbox.ID != "reserved" {
			t.Fatalf("unexpected mailbox remaining: %#v", mailbox)
		}
	}

	deleted, err = svc.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-gone")
	if err != nil || deleted {
		t.Fatalf("second delete = (%v, %v), want (false, nil)", deleted, err)
	}
	deleted, err = svc.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-held")
	if err != nil || deleted {
		t.Fatalf("reserved row delete = (%v, %v), want (false, nil)", deleted, err)
	}
}
