package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is a non-persistent Store intended only for local development and tests.
type Memory struct {
	mu        sync.RWMutex
	mailboxes map[string]Mailbox
	orders    map[string]Order
	messages  map[string]Message
}

func NewMemory() *Memory {
	return &Memory{mailboxes: make(map[string]Mailbox), orders: make(map[string]Order), messages: make(map[string]Message)}
}

func (m *Memory) Migrate(context.Context) error { return nil }
func (m *Memory) Close()                        {}

func (m *Memory) UpsertMailboxes(_ context.Context, items []Mailbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, item := range items {
		item.Address = strings.ToLower(strings.TrimSpace(item.Address))
		if item.Address == "" {
			continue
		}
		var existing *Mailbox
		for _, candidate := range m.mailboxes {
			if candidate.Address == item.Address {
				copy := candidate
				existing = &copy
				break
			}
		}
		if existing != nil {
			item.ID = existing.ID
			if item.CreatedAt.IsZero() {
				item.CreatedAt = existing.CreatedAt
			}
			if existing.Status == "reserved" || existing.Status == "retired" {
				item.Status = existing.Status
			}
		}
		if item.ID == "" {
			item.ID = uuid.NewString()
		}
		if item.Status == "" {
			item.Status = "available"
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		item.UpdatedAt = now
		m.mailboxes[item.ID] = item
	}
	return nil
}

func (m *Memory) PruneMailboxes(_ context.Context, accountID string, keepAddresses []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := make(map[string]struct{}, len(keepAddresses))
	for _, address := range keepAddresses {
		address = strings.ToLower(strings.TrimSpace(address))
		if address == "" {
			continue
		}
		keep[address] = struct{}{}
	}
	pruned := 0
	for id, mailbox := range m.mailboxes {
		if mailbox.AccountID != accountID || mailbox.Status != "available" {
			continue
		}
		if _, ok := keep[mailbox.Address]; ok {
			continue
		}
		delete(m.mailboxes, id)
		pruned++
	}
	return pruned, nil
}

func (m *Memory) DeleteMailboxByAnonymousID(_ context.Context, accountID, anonymousID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, mailbox := range m.mailboxes {
		if mailbox.AccountID != accountID || mailbox.AnonymousID != anonymousID || mailbox.Status != "available" {
			continue
		}
		delete(m.mailboxes, id)
		return true, nil
	}
	return false, nil
}

func (m *Memory) ListMailboxes(_ context.Context, status string, limit int) ([]Mailbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	items := make([]Mailbox, 0, len(m.mailboxes))
	for _, item := range m.mailboxes {
		if status == "" || item.Status == status {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (m *Memory) AllocateMailbox(_ context.Context, params AllocateParams) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var selected *Mailbox
	for _, mailbox := range m.mailboxes {
		if mailbox.Status == "available" &&
			(strings.TrimSpace(params.MailboxID) == "" || mailbox.ID == strings.TrimSpace(params.MailboxID)) &&
			(selected == nil || mailbox.CreatedAt.Before(selected.CreatedAt)) {
			copy := mailbox
			selected = &copy
		}
	}
	if selected == nil {
		return nil, ErrNoInventory
	}
	now := time.Now()
	validFor := normalizedValidFor(params.ValidFor, params.ExpiresAt, now)
	order := Order{ID: uuid.NewString(), ExternalID: strings.TrimSpace(params.ExternalID), MailboxID: selected.ID, MailboxAddress: selected.Address, PickupTokenHash: append([]byte(nil), params.PickupTokenHash...), PickupCodeHash: append([]byte(nil), params.PickupCodeHash...), Status: "active", ValidForSeconds: int64(validFor / time.Second), CreatedAt: now}
	m.orders[order.ID] = order
	selected.Status = "reserved"
	selected.UpdatedAt = now
	m.mailboxes[selected.ID] = *selected
	copy := order
	return &copy, nil
}

func (m *Memory) ListOrders(_ context.Context, limit int) ([]Order, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	items := make([]Order, 0, len(m.orders))
	for _, item := range m.orders {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (m *Memory) ReissueOrderPickup(_ context.Context, orderID string, tokenHash, codeHash []byte, expiresAt time.Time) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.orders[orderID]
	if !ok || order.Status != "active" {
		return nil, errors.New("active order not found")
	}
	order.PickupTokenHash = append([]byte(nil), tokenHash...)
	order.PickupCodeHash = append([]byte(nil), codeHash...)
	order.ValidForSeconds = int64(normalizedValidFor(0, expiresAt, time.Now()) / time.Second)
	order.ActivatedAt = nil
	order.ExpiresAt = time.Time{}
	m.orders[orderID] = order
	copy := order
	return &copy, nil
}

func (m *Memory) ActivatePickup(_ context.Context, tokenHash, codeHash []byte) (*Pickup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, order := range m.orders {
		if order.Status != "active" || subtle.ConstantTimeCompare(order.PickupTokenHash, tokenHash) != 1 || subtle.ConstantTimeCompare(order.PickupCodeHash, codeHash) != 1 {
			continue
		}
		if order.ActivatedAt != nil && !order.ExpiresAt.After(now) {
			return nil, errors.New("pickup not found")
		}
		if order.ActivatedAt == nil {
			activatedAt := now
			validFor := time.Duration(order.ValidForSeconds) * time.Second
			if validFor <= 0 {
				validFor = 24 * time.Hour
			}
			order.ActivatedAt = &activatedAt
			order.ExpiresAt = activatedAt.Add(validFor)
			m.orders[id] = order
		}
		return &Pickup{OrderID: order.ID, MailboxID: order.MailboxID, MailboxAddress: order.MailboxAddress, PickupCodeHash: append([]byte(nil), order.PickupCodeHash...), StartsAt: *order.ActivatedAt, ExpiresAt: order.ExpiresAt}, nil
	}
	return nil, errors.New("pickup not found")
}

func (m *Memory) SaveMessage(_ context.Context, message Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := message.AccountID + "\x00" + message.ProviderMessageID + "\x00" + message.MailboxID
	for id, existing := range m.messages {
		if existing.AccountID+"\x00"+existing.ProviderMessageID+"\x00"+existing.MailboxID == key {
			message.ID = id
			message.CreatedAt = existing.CreatedAt
			break
		}
	}
	if message.ID == "" {
		message.ID = uuid.NewString()
	}
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = time.Now()
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now()
	}
	m.messages[message.ID] = message
	return nil
}

func (m *Memory) ListMessages(_ context.Context, mailboxID, accountID, query string, limit int) ([]Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listMessagesLocked(mailboxID, accountID, strings.ToLower(strings.TrimSpace(query)), time.Time{}, time.Time{}, limit), nil
}

func (m *Memory) ListMessagesByOrder(_ context.Context, orderID string, limit int) ([]Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order, ok := m.orders[orderID]
	if !ok {
		return nil, errors.New("order not found")
	}
	items := m.listMessagesLocked(order.MailboxID, "", "", order.CreatedAt, order.ExpiresAt, limit)
	for i := range items {
		items[i].OrderID = order.ID
	}
	return items, nil
}

func (m *Memory) listMessagesLocked(mailboxID, accountID, query string, startsAt, expiresAt time.Time, limit int) []Message {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	items := make([]Message, 0, len(m.messages))
	for _, item := range m.messages {
		if mailboxID != "" && item.MailboxID != mailboxID || accountID != "" && item.AccountID != accountID {
			continue
		}
		if !startsAt.IsZero() && item.ReceivedAt.Before(startsAt) || !expiresAt.IsZero() && item.ReceivedAt.After(expiresAt) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Sender+"\n"+item.Recipient+"\n"+item.Subject+"\n"+item.BodyText), query) {
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ReceivedAt.After(items[j].ReceivedAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (m *Memory) ListCollectTargets(context.Context) ([]CollectTarget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]CollectTarget, 0, len(m.mailboxes))
	for _, mailbox := range m.mailboxes {
		if mailbox.Status == "available" || mailbox.Status == "reserved" {
			items = append(items, CollectTarget{MailboxID: mailbox.ID, AccountID: mailbox.AccountID, MailboxAddress: mailbox.Address, ForwardToEmail: mailbox.ForwardToEmail})
		}
	}
	return items, nil
}

func (m *Memory) Stats(context.Context) (*Stats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats := &Stats{TotalMessages: int64(len(m.messages))}
	for _, mailbox := range m.mailboxes {
		switch mailbox.Status {
		case "available":
			stats.AvailableMailboxes++
		case "reserved":
			stats.ReservedMailboxes++
		case "disabled":
			stats.DisabledMailboxes++
		}
	}
	for _, order := range m.orders {
		if order.Status == "active" && (order.ActivatedAt == nil || order.ExpiresAt.After(time.Now())) {
			stats.ActiveOrders++
		}
	}
	return stats, nil
}

func (m *Memory) ExpireOrders(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, order := range m.orders {
		if order.Status == "active" && order.ActivatedAt != nil && !order.ExpiresAt.After(now) {
			order.Status = "expired"
			m.orders[id] = order
			mailbox := m.mailboxes[order.MailboxID]
			mailbox.Status = "retired"
			mailbox.UpdatedAt = now
			m.mailboxes[mailbox.ID] = mailbox
		}
	}
	return nil
}

func normalizedValidFor(validFor time.Duration, expiresAt, now time.Time) time.Duration {
	if validFor <= 0 && expiresAt.After(now) {
		validFor = expiresAt.Sub(now)
	}
	if validFor <= 0 || validFor > 30*24*time.Hour {
		validFor = 24 * time.Hour
	}
	return validFor
}
