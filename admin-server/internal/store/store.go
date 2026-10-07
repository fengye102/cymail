package store

import (
	"context"
	"errors"
	"time"
)

var ErrNoInventory = errors.New("no available mailbox inventory")

type Mailbox struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"account_id"`
	Address        string    `json:"address"`
	ForwardToEmail string    `json:"forward_to_email"`
	AnonymousID    string    `json:"anonymous_id,omitempty"`
	Label          string    `json:"label,omitempty"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Order struct {
	ID              string     `json:"id"`
	ExternalID      string     `json:"external_id,omitempty"`
	MailboxID       string     `json:"mailbox_id"`
	MailboxAddress  string     `json:"mailbox_address"`
	PickupTokenHash []byte     `json:"-"`
	PickupCodeHash  []byte     `json:"-"`
	Status          string     `json:"status"`
	ValidForSeconds int64      `json:"valid_for_seconds"`
	ActivatedAt     *time.Time `json:"activated_at,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type AllocateParams struct {
	MailboxID       string
	ExternalID      string
	PickupTokenHash []byte
	PickupCodeHash  []byte
	ValidFor        time.Duration
	ExpiresAt       time.Time
}

type Pickup struct {
	OrderID        string
	MailboxID      string
	MailboxAddress string
	PickupCodeHash []byte
	StartsAt       time.Time
	ExpiresAt      time.Time
}

type Message struct {
	ID                string    `json:"id"`
	OrderID           string    `json:"order_id"`
	MailboxID         string    `json:"mailbox_id"`
	AccountID         string    `json:"account_id"`
	ProviderMessageID string    `json:"provider_message_id"`
	Sender            string    `json:"sender"`
	Recipient         string    `json:"recipient"`
	Subject           string    `json:"subject"`
	BodyText          string    `json:"body_text"`
	BodyHTML          string    `json:"body_html,omitempty"`
	ContentType       string    `json:"content_type,omitempty"`
	OTPCode           string    `json:"otp_code,omitempty"`
	ReceivedAt        time.Time `json:"received_at"`
	CreatedAt         time.Time `json:"created_at"`
}

type CollectTarget struct {
	MailboxID      string
	AccountID      string
	MailboxAddress string
	ForwardToEmail string
}

type Stats struct {
	AvailableMailboxes int64 `json:"available_mailboxes"`
	ReservedMailboxes  int64 `json:"reserved_mailboxes"`
	DisabledMailboxes  int64 `json:"disabled_mailboxes"`
	TotalMessages      int64 `json:"total_messages"`
	ActiveOrders       int64 `json:"active_orders"`
}

type Store interface {
	Migrate(context.Context) error
	UpsertMailboxes(context.Context, []Mailbox) error
	// PruneMailboxes deletes mailbox rows of the given account whose address is
	// not in keepAddresses. Only rows with status='available' are considered:
	// reserved/disabled/retired rows are always preserved so shipped orders and
	// message history are never broken. It returns the number of rows deleted.
	PruneMailboxes(context.Context, string, []string) (int, error)
	// DeleteMailboxByAnonymousID removes a single available mailbox row of the
	// given account identified by its iCloud anonymous id. Non-available rows
	// are preserved. It reports whether a row was deleted; a missing row is not
	// an error so repeated calls are idempotent.
	DeleteMailboxByAnonymousID(context.Context, string, string) (bool, error)
	ListMailboxes(context.Context, string, int) ([]Mailbox, error)
	AllocateMailbox(context.Context, AllocateParams) (*Order, error)
	ListOrders(context.Context, int) ([]Order, error)
	ReissueOrderPickup(context.Context, string, []byte, []byte, time.Time) (*Order, error)
	ActivatePickup(context.Context, []byte, []byte) (*Pickup, error)
	SaveMessage(context.Context, Message) error
	ListMessages(context.Context, string, string, string, int) ([]Message, error)
	ListMessagesByOrder(context.Context, string, int) ([]Message, error)
	ListCollectTargets(context.Context) ([]CollectTarget, error)
	Stats(context.Context) (*Stats, error)
	ExpireOrders(context.Context) error
	Close()
}
