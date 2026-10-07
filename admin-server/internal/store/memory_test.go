package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestMemoryPickupValidityStartsOnFirstSuccessfulUse(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	if err := st.UpsertMailboxes(ctx, []Mailbox{{ID: "mailbox-activation", Address: "activation@icloud.com", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("token"))
	codeHash := sha256.Sum256([]byte("code"))
	wrongCodeHash := sha256.Sum256([]byte("wrong-code"))
	order, err := st.AllocateMailbox(ctx, AllocateParams{
		MailboxID: "mailbox-activation", PickupTokenHash: tokenHash[:], PickupCodeHash: codeHash[:], ValidFor: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if order.ActivatedAt != nil || !order.ExpiresAt.IsZero() {
		t.Fatalf("new order started before pickup: %#v", order)
	}
	if _, err := st.ActivatePickup(ctx, tokenHash[:], wrongCodeHash[:]); err == nil {
		t.Fatal("wrong pickup code activated the order")
	}
	before := time.Now()
	pickup, err := st.ActivatePickup(ctx, tokenHash[:], codeHash[:])
	if err != nil {
		t.Fatal(err)
	}
	if pickup.StartsAt.Before(before) || pickup.ExpiresAt.Sub(pickup.StartsAt) != 24*time.Hour {
		t.Fatalf("unexpected activation window: %#v", pickup)
	}
	repeated, err := st.ActivatePickup(ctx, tokenHash[:], codeHash[:])
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.StartsAt.Equal(pickup.StartsAt) || !repeated.ExpiresAt.Equal(pickup.ExpiresAt) {
		t.Fatalf("repeat pickup reset validity: first=%#v repeat=%#v", pickup, repeated)
	}
}

func TestMemoryAllocateMailboxHonorsMailboxID(t *testing.T) {
	st := NewMemory()
	err := st.UpsertMailboxes(context.Background(), []Mailbox{
		{ID: "mailbox-oldest", Address: "oldest@icloud.com", Status: "available", CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "mailbox-selected", Address: "selected@icloud.com", Status: "available", CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	order, err := st.AllocateMailbox(context.Background(), AllocateParams{MailboxID: "mailbox-selected", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if order.MailboxID != "mailbox-selected" || order.MailboxAddress != "selected@icloud.com" {
		t.Fatalf("unexpected selected mailbox: %#v", order)
	}
	if _, err := st.AllocateMailbox(context.Background(), AllocateParams{MailboxID: "mailbox-selected", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrNoInventory) {
		t.Fatalf("second allocation error = %v, want ErrNoInventory", err)
	}
}

func TestMemoryMessagePreservesHTMLPresentation(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	if err := st.UpsertMailboxes(ctx, []Mailbox{{ID: "mailbox-html", AccountID: "account-html", Address: "html@icloud.com", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	wantHTML := `<html><body><table style="color:#123"><tr><td>Hello</td></tr></table></body></html>`
	if err := st.SaveMessage(ctx, Message{
		MailboxID: "mailbox-html", AccountID: "account-html", ProviderMessageID: "provider-html",
		BodyText: "Hello", BodyHTML: wantHTML, ContentType: "text/html; charset=utf-8",
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := st.ListMessages(ctx, "mailbox-html", "account-html", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].BodyHTML != wantHTML || messages[0].ContentType != "text/html; charset=utf-8" {
		t.Fatalf("HTML message fields were not preserved: %#v", messages)
	}
}

func TestMemoryUpsertCorrectsMailboxCreationTime(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	firstSync := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	appleCreatedAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := st.UpsertMailboxes(ctx, []Mailbox{{ID: "mailbox-time", Address: "time@icloud.com", Status: "available", CreatedAt: firstSync}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{{Address: "time@icloud.com", Status: "available", CreatedAt: appleCreatedAt}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMailboxes(ctx, []Mailbox{{Address: "time@icloud.com", Status: "available"}}); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := st.ListMailboxes(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailboxes) != 1 || !mailboxes[0].CreatedAt.Equal(appleCreatedAt) {
		t.Fatalf("creation time was not corrected and preserved: %#v", mailboxes)
	}
}

func TestMemoryPruneMailboxesKeepsListedAndProtectsNonAvailable(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	seed := []Mailbox{
		{ID: "keep-1", AccountID: "account-1", Address: "keep1@icloud.com", Status: "available"},
		{ID: "keep-2", AccountID: "account-1", Address: "KEEP2@icloud.com", Status: "available"},
		{ID: "ghost-1", AccountID: "account-1", Address: "ghost1@icloud.com", Status: "available"},
		{ID: "ghost-2", AccountID: "account-1", Address: "Ghost2@icloud.com", Status: "available"},
		{ID: "reserved-1", AccountID: "account-1", Address: "reserved1@icloud.com", Status: "reserved"},
		{ID: "disabled-1", AccountID: "account-1", Address: "disabled1@icloud.com", Status: "disabled"},
		{ID: "retired-1", AccountID: "account-1", Address: "retired1@icloud.com", Status: "retired"},
		{ID: "other-account", AccountID: "account-2", Address: "other@icloud.com", Status: "available"},
		{ID: "other-ghost", AccountID: "account-2", Address: "otherghost@icloud.com", Status: "available"},
	}
	if err := st.UpsertMailboxes(ctx, seed); err != nil {
		t.Fatal(err)
	}

	pruned, err := st.PruneMailboxes(ctx, "account-1", []string{"keep1@icloud.com", " KEEP2@icloud.com "})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}

	remaining, err := st.ListMailboxes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Mailbox, len(remaining))
	for _, mailbox := range remaining {
		byID[mailbox.ID] = mailbox
	}
	for _, id := range []string{"ghost-1", "ghost-2"} {
		if _, ok := byID[id]; ok {
			t.Fatalf("ghost row %s was not pruned", id)
		}
	}
	for _, id := range []string{"keep-1", "keep-2", "reserved-1", "disabled-1", "retired-1", "other-account", "other-ghost"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("protected row %s was pruned", id)
		}
	}
}

func TestMemoryPruneMailboxesEmptyKeepWipesOnlyAvailableOfAccount(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	if err := st.UpsertMailboxes(ctx, []Mailbox{
		{ID: "available-1", AccountID: "account-1", Address: "a@icloud.com", Status: "available"},
		{ID: "available-2", AccountID: "account-1", Address: "b@icloud.com", Status: "available"},
		{ID: "reserved-1", AccountID: "account-1", Address: "c@icloud.com", Status: "reserved"},
		{ID: "other-account", AccountID: "account-2", Address: "d@icloud.com", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}
	pruned, err := st.PruneMailboxes(ctx, "account-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	remaining, err := st.ListMailboxes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]bool, len(remaining))
	for _, mailbox := range remaining {
		byID[mailbox.ID] = true
	}
	if byID["available-1"] || byID["available-2"] {
		t.Fatalf("available rows were not wiped: %#v", byID)
	}
	if !byID["reserved-1"] || !byID["other-account"] {
		t.Fatalf("non-available or other-account rows were pruned: %#v", byID)
	}
}

func TestMemoryDeleteMailboxByAnonymousID(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	if err := st.UpsertMailboxes(ctx, []Mailbox{
		{ID: "target", AccountID: "account-1", Address: "gone@icloud.com", AnonymousID: "anon-gone", Status: "available"},
		{ID: "reserved", AccountID: "account-1", Address: "held@icloud.com", AnonymousID: "anon-held", Status: "reserved"},
		{ID: "disabled", AccountID: "account-1", Address: "off@icloud.com", AnonymousID: "anon-off", Status: "disabled"},
		{ID: "other-account", AccountID: "account-2", Address: "other@icloud.com", AnonymousID: "anon-gone", Status: "available"},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-gone")
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	deleted, err = st.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-gone")
	if err != nil || deleted {
		t.Fatalf("repeat delete = (%v, %v), want (false, nil)", deleted, err)
	}
	deleted, err = st.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-held")
	if err != nil || deleted {
		t.Fatalf("reserved delete = (%v, %v), want (false, nil)", deleted, err)
	}
	deleted, err = st.DeleteMailboxByAnonymousID(ctx, "account-1", "anon-off")
	if err != nil || deleted {
		t.Fatalf("disabled delete = (%v, %v), want (false, nil)", deleted, err)
	}
	deleted, err = st.DeleteMailboxByAnonymousID(ctx, "account-2", "anon-gone")
	if err != nil || !deleted {
		t.Fatalf("other-account delete = (%v, %v), want (true, nil)", deleted, err)
	}

	remaining, err := st.ListMailboxes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Mailbox, len(remaining))
	for _, mailbox := range remaining {
		byID[mailbox.ID] = mailbox
	}
	for _, id := range []string{"target", "other-account"} {
		if _, ok := byID[id]; ok {
			t.Fatalf("deleted row %s still present", id)
		}
	}
	for _, id := range []string{"reserved", "disabled"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("protected row %s was deleted", id)
		}
	}
}
