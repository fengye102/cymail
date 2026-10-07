package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Postgres struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

func OpenPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return &Postgres{conn: conn}, nil
}

func (p *Postgres) Close() {
	if p == nil || p.conn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.conn.Close(context.Background())
}
func (p *Postgres) Migrate(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS mailboxes (
            id text PRIMARY KEY,
            account_id text NOT NULL,
            address text NOT NULL UNIQUE,
			forward_to_email text NOT NULL DEFAULT '',
            anonymous_id text NOT NULL DEFAULT '',
            label text NOT NULL DEFAULT '',
            status text NOT NULL CHECK (status IN ('available','reserved','disabled','retired')),
            created_at timestamptz NOT NULL DEFAULT now(),
            updated_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE INDEX IF NOT EXISTS mailboxes_status_created_idx ON mailboxes(status, created_at)`,
		`CREATE INDEX IF NOT EXISTS mailboxes_account_idx ON mailboxes(account_id)`,
		`ALTER TABLE mailboxes ADD COLUMN IF NOT EXISTS forward_to_email text NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS orders (
            id text PRIMARY KEY,
            external_id text,
            mailbox_id text NOT NULL UNIQUE REFERENCES mailboxes(id),
            pickup_token_hash bytea NOT NULL UNIQUE,
            pickup_code_hash bytea NOT NULL,
            status text NOT NULL CHECK (status IN ('active','expired','closed')),
			valid_for_seconds bigint NOT NULL DEFAULT 86400,
			activated_at timestamptz,
            expires_at timestamptz NOT NULL,
            created_at timestamptz NOT NULL DEFAULT now()
        )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS orders_external_id_unique ON orders(external_id) WHERE external_id IS NOT NULL AND external_id <> ''`,
		`CREATE INDEX IF NOT EXISTS orders_status_expiry_idx ON orders(status, expires_at)`,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema=current_schema() AND table_name='orders' AND column_name='activated_at'
			) THEN
				ALTER TABLE orders ADD COLUMN valid_for_seconds bigint NOT NULL DEFAULT 86400;
				ALTER TABLE orders ADD COLUMN activated_at timestamptz;
				UPDATE orders
				SET valid_for_seconds=GREATEST(1, EXTRACT(EPOCH FROM (expires_at-created_at))::bigint),
					activated_at=created_at;
			END IF;
		END $$`,
		`CREATE TABLE IF NOT EXISTS messages (
            id text PRIMARY KEY,
            order_id text REFERENCES orders(id),
            mailbox_id text NOT NULL REFERENCES mailboxes(id),
            account_id text NOT NULL,
            provider_message_id text NOT NULL,
            sender text NOT NULL DEFAULT '',
            recipient text NOT NULL DEFAULT '',
            subject text NOT NULL DEFAULT '',
            body_text text NOT NULL DEFAULT '',
			body_html text NOT NULL DEFAULT '',
			content_type text NOT NULL DEFAULT '',
            otp_code text NOT NULL DEFAULT '',
            received_at timestamptz NOT NULL,
            created_at timestamptz NOT NULL DEFAULT now(),
            UNIQUE(account_id, provider_message_id, mailbox_id)
        )`,
		`CREATE INDEX IF NOT EXISTS messages_order_received_idx ON messages(order_id, received_at DESC)`,
		`ALTER TABLE messages ALTER COLUMN order_id DROP NOT NULL`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS body_html text NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS content_type text NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS messages_mailbox_received_idx ON messages(mailbox_id, received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS messages_account_received_idx ON messages(account_id, received_at DESC)`,
	}
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) UpsertMailboxes(ctx context.Context, mailboxes []Mailbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(mailboxes) == 0 {
		return nil
	}
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, mailbox := range mailboxes {
		address := strings.ToLower(strings.TrimSpace(mailbox.Address))
		if address == "" {
			continue
		}
		if mailbox.ID == "" {
			mailbox.ID = uuid.NewString()
		}
		if mailbox.Status == "" {
			mailbox.Status = "available"
		}
		var createdAt *time.Time
		if !mailbox.CreatedAt.IsZero() {
			value := mailbox.CreatedAt
			createdAt = &value
		}
		_, err := tx.Exec(ctx, `
            INSERT INTO mailboxes (id, account_id, address, forward_to_email, anonymous_id, label, status, created_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,COALESCE($8::timestamptz, now()))
            ON CONFLICT (address) DO UPDATE SET
                account_id = EXCLUDED.account_id,
				forward_to_email = EXCLUDED.forward_to_email,
                anonymous_id = EXCLUDED.anonymous_id,
                label = EXCLUDED.label,
				created_at = COALESCE($8::timestamptz, mailboxes.created_at),
                status = CASE
                    WHEN mailboxes.status IN ('reserved','retired') THEN mailboxes.status
                    ELSE EXCLUDED.status
                END,
                updated_at = now()`,
			mailbox.ID, mailbox.AccountID, address, strings.ToLower(strings.TrimSpace(mailbox.ForwardToEmail)), mailbox.AnonymousID, mailbox.Label, mailbox.Status, createdAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// PruneMailboxes deletes available mailbox rows of the account whose address
// is not among keepAddresses. The delete runs inside a transaction; addresses
// are compared in their normalized lowercase form. Non-available rows are
// never touched, so orders and message history stay intact.
func (p *Postgres) PruneMailboxes(ctx context.Context, accountID string, keepAddresses []string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	keep := make([]string, 0, len(keepAddresses))
	for _, address := range keepAddresses {
		address = strings.ToLower(strings.TrimSpace(address))
		if address != "" {
			keep = append(keep, address)
		}
	}
	var tag pgconn.CommandTag
	if len(keep) == 0 {
		tag, err = tx.Exec(ctx, `DELETE FROM mailboxes WHERE account_id=$1 AND status='available'`, accountID)
	} else {
		tag, err = tx.Exec(ctx, `DELETE FROM mailboxes WHERE account_id=$1 AND status='available' AND NOT (address = ANY($2::text[]))`, accountID, keep)
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteMailboxByAnonymousID removes the single available mailbox row of the
// account identified by its iCloud anonymous id. Rows in any other status are
// preserved. It reports whether a row was deleted; a missing row returns false
// without an error so repeated calls are idempotent.
func (p *Postgres) DeleteMailboxByAnonymousID(ctx context.Context, accountID, anonymousID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tag, err := p.conn.Exec(ctx, `DELETE FROM mailboxes WHERE account_id=$1 AND anonymous_id=$2 AND status='available'`, accountID, anonymousID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) ListMailboxes(ctx context.Context, status string, limit int) ([]Mailbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, account_id, address, forward_to_email, anonymous_id, label, status, created_at, updated_at FROM mailboxes`
	args := []any{}
	if status != "" {
		query += ` WHERE status = $1`
		args = append(args, status)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := p.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mailbox
	for rows.Next() {
		var mailbox Mailbox
		if err := rows.Scan(&mailbox.ID, &mailbox.AccountID, &mailbox.Address, &mailbox.ForwardToEmail, &mailbox.AnonymousID, &mailbox.Label, &mailbox.Status, &mailbox.CreatedAt, &mailbox.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, mailbox)
	}
	return out, rows.Err()
}

func (p *Postgres) AllocateMailbox(ctx context.Context, params AllocateParams) (*Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var mailbox Mailbox
	err = tx.QueryRow(ctx, `
		SELECT id, account_id, address, forward_to_email, anonymous_id, label, status, created_at, updated_at
        FROM mailboxes
        WHERE status = 'available' AND ($1 = '' OR id = $1)
        ORDER BY created_at
        LIMIT 1
		FOR UPDATE SKIP LOCKED`, strings.TrimSpace(params.MailboxID)).
		Scan(&mailbox.ID, &mailbox.AccountID, &mailbox.Address, &mailbox.ForwardToEmail, &mailbox.AnonymousID, &mailbox.Label, &mailbox.Status, &mailbox.CreatedAt, &mailbox.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoInventory
	}
	if err != nil {
		return nil, err
	}

	order := &Order{
		ID:              uuid.NewString(),
		ExternalID:      strings.TrimSpace(params.ExternalID),
		MailboxID:       mailbox.ID,
		MailboxAddress:  mailbox.Address,
		PickupTokenHash: params.PickupTokenHash,
		PickupCodeHash:  params.PickupCodeHash,
		Status:          "active",
		ValidForSeconds: int64(normalizedValidFor(params.ValidFor, params.ExpiresAt, time.Now()) / time.Second),
		CreatedAt:       time.Now(),
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO orders (id, external_id, mailbox_id, pickup_token_hash, pickup_code_hash, status, valid_for_seconds, activated_at, expires_at, created_at)
		VALUES ($1, NULLIF($2,''), $3, $4, $5, $6, $7, NULL, $8, $9)`,
		order.ID, order.ExternalID, order.MailboxID, order.PickupTokenHash, order.PickupCodeHash, order.Status, order.ValidForSeconds, time.Time{}, order.CreatedAt)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE mailboxes SET status='reserved', updated_at=now() WHERE id=$1`, mailbox.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return order, nil
}

func (p *Postgres) ListOrders(ctx context.Context, limit int) ([]Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := p.conn.Query(ctx, `
		SELECT o.id, COALESCE(o.external_id,''), o.mailbox_id, m.address, o.status, o.valid_for_seconds,
		       COALESCE(o.activated_at, '0001-01-01 00:00:00+00'::timestamptz), o.expires_at, o.created_at
        FROM orders o JOIN mailboxes m ON m.id=o.mailbox_id
        ORDER BY o.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var order Order
		var activatedAt time.Time
		if err := rows.Scan(&order.ID, &order.ExternalID, &order.MailboxID, &order.MailboxAddress, &order.Status, &order.ValidForSeconds, &activatedAt, &order.ExpiresAt, &order.CreatedAt); err != nil {
			return nil, err
		}
		if !activatedAt.IsZero() {
			order.ActivatedAt = &activatedAt
		}
		out = append(out, order)
	}
	return out, rows.Err()
}

func (p *Postgres) ReissueOrderPickup(ctx context.Context, orderID string, tokenHash, codeHash []byte, expiresAt time.Time) (*Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var order Order
	validForSeconds := int64(normalizedValidFor(0, expiresAt, time.Now()) / time.Second)
	err := p.conn.QueryRow(ctx, `
		UPDATE orders
		SET pickup_token_hash=$2, pickup_code_hash=$3, valid_for_seconds=$4, activated_at=NULL, expires_at=$5
		WHERE id=$1 AND status='active'
		RETURNING id, COALESCE(external_id,''), mailbox_id, status, valid_for_seconds, expires_at, created_at`,
		orderID, tokenHash, codeHash, validForSeconds, time.Time{}).
		Scan(&order.ID, &order.ExternalID, &order.MailboxID, &order.Status, &order.ValidForSeconds, &order.ExpiresAt, &order.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := p.conn.QueryRow(ctx, `SELECT address FROM mailboxes WHERE id=$1`, order.MailboxID).Scan(&order.MailboxAddress); err != nil {
		return nil, err
	}
	return &order, nil
}

func (p *Postgres) ActivatePickup(ctx context.Context, tokenHash, codeHash []byte) (*Pickup, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var pickup Pickup
	err := p.conn.QueryRow(ctx, `
		UPDATE orders o
		SET activated_at=COALESCE(o.activated_at, now()),
			expires_at=CASE WHEN o.activated_at IS NULL THEN now() + (o.valid_for_seconds * interval '1 second') ELSE o.expires_at END
		FROM mailboxes m
		WHERE o.mailbox_id=m.id AND o.pickup_token_hash=$1 AND o.pickup_code_hash=$2 AND o.status='active'
			AND (o.activated_at IS NULL OR o.expires_at > now())
		RETURNING o.id, o.mailbox_id, m.address, o.pickup_code_hash, o.activated_at, o.expires_at`, tokenHash, codeHash).
		Scan(&pickup.OrderID, &pickup.MailboxID, &pickup.MailboxAddress, &pickup.PickupCodeHash, &pickup.StartsAt, &pickup.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &pickup, nil
}

func (p *Postgres) SaveMessage(ctx context.Context, message Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if message.ID == "" {
		message.ID = uuid.NewString()
	}
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = time.Now()
	}
	_, err := p.conn.Exec(ctx, `
		INSERT INTO messages (id, order_id, mailbox_id, account_id, provider_message_id, sender, recipient, subject, body_text, body_html, content_type, otp_code, received_at)
		VALUES ($1,NULLIF($2,''),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
        ON CONFLICT (account_id, provider_message_id, mailbox_id) DO UPDATE SET
            sender=EXCLUDED.sender, recipient=EXCLUDED.recipient, subject=EXCLUDED.subject,
			body_text=EXCLUDED.body_text, body_html=EXCLUDED.body_html, content_type=EXCLUDED.content_type,
			otp_code=EXCLUDED.otp_code, received_at=EXCLUDED.received_at`,
		message.ID, message.OrderID, message.MailboxID, message.AccountID, message.ProviderMessageID,
		message.Sender, message.Recipient, message.Subject, message.BodyText, message.BodyHTML, message.ContentType, message.OTPCode, message.ReceivedAt)
	return err
}

func (p *Postgres) ListMessages(ctx context.Context, mailboxID, accountID, query string, limit int) ([]Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	conditions := []string{"1=1"}
	args := []any{}
	if mailboxID != "" {
		args = append(args, mailboxID)
		conditions = append(conditions, fmt.Sprintf("msg.mailbox_id=$%d", len(args)))
	}
	if accountID != "" {
		args = append(args, accountID)
		conditions = append(conditions, fmt.Sprintf("msg.account_id=$%d", len(args)))
	}
	if query != "" {
		args = append(args, "%"+strings.ToLower(query)+"%")
		conditions = append(conditions, fmt.Sprintf("(lower(msg.sender) LIKE $%d OR lower(msg.subject) LIKE $%d OR lower(msg.recipient) LIKE $%d OR lower(msg.body_text) LIKE $%d)", len(args), len(args), len(args), len(args)))
	}
	args = append(args, limit)
	rows, err := p.conn.Query(ctx, `
		SELECT msg.id, COALESCE(msg.order_id,''), msg.mailbox_id, msg.account_id, msg.provider_message_id,
		       msg.sender, msg.recipient, msg.subject, msg.body_text, msg.body_html, msg.content_type, msg.otp_code, msg.received_at, msg.created_at
		FROM messages msg
		WHERE `+strings.Join(conditions, " AND ")+fmt.Sprintf(" ORDER BY msg.received_at DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (p *Postgres) ListMessagesByOrder(ctx context.Context, orderID string, limit int) ([]Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := p.conn.Query(ctx, `
		SELECT msg.id, o.id, msg.mailbox_id, msg.account_id, msg.provider_message_id,
		       msg.sender, msg.recipient, msg.subject, msg.body_text, msg.body_html, msg.content_type, msg.otp_code, msg.received_at, msg.created_at
		FROM orders o
		JOIN messages msg ON msg.mailbox_id=o.mailbox_id
		WHERE o.id=$1 AND msg.received_at >= o.created_at AND (o.activated_at IS NULL OR msg.received_at <= o.expires_at)
		ORDER BY msg.received_at DESC LIMIT $2`, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (p *Postgres) ListCollectTargets(ctx context.Context) ([]CollectTarget, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, err := p.conn.Query(ctx, `
		SELECT m.id, m.account_id, m.address, m.forward_to_email
		FROM mailboxes m
		WHERE m.status IN ('available','reserved')
		ORDER BY m.account_id, m.address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectTarget
	for rows.Next() {
		var target CollectTarget
		if err := rows.Scan(&target.MailboxID, &target.AccountID, &target.MailboxAddress, &target.ForwardToEmail); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

func (p *Postgres) Stats(ctx context.Context) (*Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var stats Stats
	err := p.conn.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status='available'),
			count(*) FILTER (WHERE status='reserved'),
			count(*) FILTER (WHERE status='disabled')
		FROM mailboxes`).Scan(&stats.AvailableMailboxes, &stats.ReservedMailboxes, &stats.DisabledMailboxes)
	if err != nil {
		return nil, err
	}
	if err := p.conn.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&stats.TotalMessages); err != nil {
		return nil, err
	}
	if err := p.conn.QueryRow(ctx, `SELECT count(*) FROM orders WHERE status='active' AND (activated_at IS NULL OR expires_at > now())`).Scan(&stats.ActiveOrders); err != nil {
		return nil, err
	}
	return &stats, nil
}

func scanMessages(rows pgx.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.OrderID, &message.MailboxID, &message.AccountID, &message.ProviderMessageID,
			&message.Sender, &message.Recipient, &message.Subject, &message.BodyText, &message.BodyHTML, &message.ContentType,
			&message.OTPCode, &message.ReceivedAt, &message.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, rows.Err()
}

func (p *Postgres) ExpireOrders(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.conn.Exec(ctx, `
        WITH expired AS (
            UPDATE orders SET status='expired'
			WHERE status='active' AND activated_at IS NOT NULL AND expires_at <= now()
            RETURNING mailbox_id
        )
        UPDATE mailboxes SET status='retired', updated_at=now()
        WHERE id IN (SELECT mailbox_id FROM expired)`)
	return err
}
