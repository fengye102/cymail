package fulfillment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	mailclient "icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

var ErrInvalidPickup = errors.New("invalid pickup credentials")
var otpPattern = regexp.MustCompile(`(?i)(?:code|验证码|otp|pin|verification)[^0-9]{0,24}([0-9]{4,8})|\b([0-9]{6})\b`)

type Service struct {
	mgr             *account.Manager
	store           store.Store
	pickupCollectMu sync.Mutex
	pickupCollected map[string]time.Time
}

func New(mgr *account.Manager, st store.Store) *Service {
	return &Service{mgr: mgr, store: st, pickupCollected: make(map[string]time.Time)}
}

func (s *Service) ListMailboxes(ctx context.Context, status string, limit int) ([]store.Mailbox, error) {
	return s.store.ListMailboxes(ctx, status, limit)
}

func (s *Service) ListOrders(ctx context.Context, limit int) ([]store.Order, error) {
	return s.store.ListOrders(ctx, limit)
}

func (s *Service) ListMessages(ctx context.Context, orderID string, limit int) ([]store.Message, error) {
	return s.store.ListMessagesByOrder(ctx, orderID, limit)
}

func (s *Service) ListUnifiedMessages(ctx context.Context, mailboxID, accountID, query string, limit int) ([]store.Message, error) {
	return s.store.ListMessages(ctx, strings.TrimSpace(mailboxID), strings.TrimSpace(accountID), strings.TrimSpace(query), limit)
}

func (s *Service) Stats(ctx context.Context) (*store.Stats, error) {
	return s.store.Stats(ctx)
}

func (s *Service) SyncAccount(ctx context.Context, accountID string) (int, error) {
	settings, err := s.mgr.RefreshForwarding(accountID)
	if err != nil {
		return 0, err
	}
	return s.syncAccountAliases(ctx, accountID, settings)
}

// syncAccountAliases converges the store inventory for one account to the
// aliases currently reported by iCloud. It UPSERTs every live alias and then
// prunes ghost rows: available rows whose address no longer exists on iCloud
// (e.g. aliases permanently deleted through the admin API) are removed. Rows
// in reserved/disabled/retired status are always preserved. A prune failure is
// logged but never fails the sync, so a store hiccup cannot block inventory
// refresh. It returns the number of aliases upserted.
func (s *Service) syncAccountAliases(ctx context.Context, accountID string, settings *hme.ForwardingSettings) (int, error) {
	aliases := settings.Aliases
	mailboxes := make([]store.Mailbox, 0, len(aliases))
	keepAddresses := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		status := "disabled"
		if alias.Active {
			status = "available"
		}
		createdAt, _ := hme.ParseAliasCreatedAt(alias.CreatedAt)
		mailboxes = append(mailboxes, store.Mailbox{
			ID:        uuid.NewSHA1(uuid.NameSpaceDNS, []byte(accountID+":"+strings.ToLower(alias.Email))).String(),
			AccountID: accountID, Address: alias.Email, AnonymousID: alias.AnonymousID,
			ForwardToEmail: firstNonEmpty(alias.ForwardToEmail, settings.SelectedForwardTo), Label: alias.Label, Status: status,
			CreatedAt: createdAt,
		})
		keepAddresses = append(keepAddresses, alias.Email)
	}
	if err := s.store.UpsertMailboxes(ctx, mailboxes); err != nil {
		return 0, err
	}
	if pruned, pruneErr := s.store.PruneMailboxes(ctx, accountID, keepAddresses); pruneErr != nil {
		log.Printf("SyncAccount: 清理账号 %s 的幽灵邮箱失败(忽略,等待下次同步): %v", accountID, pruneErr)
	} else if pruned > 0 {
		log.Printf("SyncAccount: 已清理账号 %s 的 %d 个幽灵邮箱", accountID, pruned)
	}
	return len(mailboxes), nil
}

// PruneMailboxes deletes available mailbox rows of the account whose address
// is not in keepAddresses. Non-available rows are preserved. Exposed for the
// admin API and tests; SyncAccount performs the same cleanup automatically.
func (s *Service) PruneMailboxes(ctx context.Context, accountID string, keepAddresses []string) (int, error) {
	return s.store.PruneMailboxes(ctx, accountID, keepAddresses)
}

// DeleteMailboxByAnonymousID removes the available mailbox row of the account
// identified by its iCloud anonymous id. It reports whether a row was deleted.
func (s *Service) DeleteMailboxByAnonymousID(ctx context.Context, accountID, anonymousID string) (bool, error) {
	return s.store.DeleteMailboxByAnonymousID(ctx, accountID, anonymousID)
}

func (s *Service) Allocate(ctx context.Context, externalID string, ttl time.Duration) (*store.Order, string, string, error) {
	if ttl <= 0 || ttl > 30*24*time.Hour {
		ttl = 24 * time.Hour
	}
	token, err := randomURLToken(32)
	if err != nil {
		return nil, "", "", err
	}
	code, err := randomCode()
	if err != nil {
		return nil, "", "", err
	}
	tokenHash := sha256.Sum256([]byte(token))
	codeHash := sha256.Sum256([]byte(code))
	order, err := s.store.AllocateMailbox(ctx, store.AllocateParams{
		ExternalID: strings.TrimSpace(externalID), PickupTokenHash: tokenHash[:], PickupCodeHash: codeHash[:], ValidFor: ttl,
	})
	if err != nil {
		return nil, "", "", err
	}
	return order, token, code, nil
}

// AllocateWithPickupKey allocates an order with a single long bearer key.
// The key is only returned once; storage keeps only its SHA-256 digest.
func (s *Service) AllocateWithPickupKey(ctx context.Context, externalID string, ttl time.Duration) (*store.Order, string, error) {
	return s.AllocateMailboxWithPickupKey(ctx, "", externalID, ttl)
}

// AllocateMailboxWithPickupKey allocates a specific available mailbox when
// mailboxID is provided. An empty mailboxID preserves the original oldest-
// available allocation behavior.
func (s *Service) AllocateMailboxWithPickupKey(ctx context.Context, mailboxID, externalID string, ttl time.Duration) (*store.Order, string, error) {
	if ttl <= 0 || ttl > 30*24*time.Hour {
		ttl = 24 * time.Hour
	}
	key, err := randomPickupKey()
	if err != nil {
		return nil, "", err
	}
	keyHash := sha256.Sum256([]byte(key))
	order, err := s.store.AllocateMailbox(ctx, store.AllocateParams{
		MailboxID: strings.TrimSpace(mailboxID), ExternalID: strings.TrimSpace(externalID), PickupTokenHash: keyHash[:], PickupCodeHash: keyHash[:], ValidFor: ttl,
	})
	if err != nil {
		return nil, "", err
	}
	return order, key, nil
}

// ReissuePickupKey replaces an active order's bearer key. The previous key
// becomes invalid immediately and the new plaintext key is returned once.
func (s *Service) ReissuePickupKey(ctx context.Context, orderID string, ttl time.Duration) (*store.Order, string, error) {
	if ttl <= 0 || ttl > 30*24*time.Hour {
		ttl = 24 * time.Hour
	}
	key, err := randomPickupKey()
	if err != nil {
		return nil, "", err
	}
	keyHash := sha256.Sum256([]byte(key))
	order, err := s.store.ReissueOrderPickup(ctx, strings.TrimSpace(orderID), keyHash[:], keyHash[:], time.Now().Add(ttl))
	if err != nil {
		return nil, "", err
	}
	return order, key, nil
}

func (s *Service) Pickup(ctx context.Context, token, code string) (*store.Order, []store.Message, error) {
	token = strings.TrimSpace(token)
	code = strings.TrimSpace(code)
	if token == "" || code == "" || len(code) < 4 || len(code) > 8 {
		return nil, nil, ErrInvalidPickup
	}
	tokenHash := sha256.Sum256([]byte(token))
	codeHash := sha256.Sum256([]byte(code))
	pickup, err := s.store.ActivatePickup(ctx, tokenHash[:], codeHash[:])
	if err != nil || subtle.ConstantTimeCompare(codeHash[:], pickup.PickupCodeHash) != 1 {
		return nil, nil, ErrInvalidPickup
	}
	messages, err := s.store.ListMessagesByOrder(ctx, pickup.OrderID, 100)
	if err != nil {
		return nil, nil, err
	}
	activatedAt := pickup.StartsAt
	order := &store.Order{ID: pickup.OrderID, MailboxID: pickup.MailboxID, MailboxAddress: pickup.MailboxAddress, Status: "active", ActivatedAt: &activatedAt, CreatedAt: pickup.StartsAt, ExpiresAt: pickup.ExpiresAt}
	return order, messages, nil
}

// PickupKey validates the single long key used by the public pickup URL.
func (s *Service) PickupKey(ctx context.Context, key string) (*store.Order, []store.Message, error) {
	key = strings.TrimSpace(key)
	if !validPickupKey(key) {
		return nil, nil, ErrInvalidPickup
	}
	keyHash := sha256.Sum256([]byte(key))
	pickup, err := s.store.ActivatePickup(ctx, keyHash[:], keyHash[:])
	if err != nil || subtle.ConstantTimeCompare(keyHash[:], pickup.PickupCodeHash) != 1 {
		return nil, nil, ErrInvalidPickup
	}
	messages, err := s.store.ListMessagesByOrder(ctx, pickup.OrderID, 100)
	if err != nil {
		return nil, nil, err
	}
	activatedAt := pickup.StartsAt
	order := &store.Order{ID: pickup.OrderID, MailboxID: pickup.MailboxID, MailboxAddress: pickup.MailboxAddress, Status: "active", ActivatedAt: &activatedAt, CreatedAt: pickup.StartsAt, ExpiresAt: pickup.ExpiresAt}
	return order, messages, nil
}

func (s *Service) Collect(ctx context.Context) (int, []string, error) {
	return s.CollectFiltered(ctx, "", "")
}

// CollectMailbox synchronizes exactly one Hide My Email address for a valid
// pickup session. Repeated pickup-page polling is throttled per mailbox so one
// customer cannot repeatedly scan the whole forwarding inbox.
func (s *Service) CollectMailbox(ctx context.Context, mailboxID string) (processed int, refreshed bool, err error) {
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID == "" {
		return 0, false, fmt.Errorf("取件邮箱编号为空")
	}
	now := time.Now()
	s.pickupCollectMu.Lock()
	if last := s.pickupCollected[mailboxID]; !last.IsZero() && now.Sub(last) < 15*time.Second {
		s.pickupCollectMu.Unlock()
		return 0, false, nil
	}
	s.pickupCollected[mailboxID] = now
	s.pickupCollectMu.Unlock()

	clearThrottle := func() {
		s.pickupCollectMu.Lock()
		delete(s.pickupCollected, mailboxID)
		s.pickupCollectMu.Unlock()
	}
	targets, err := s.store.ListCollectTargets(ctx)
	if err != nil {
		clearThrottle()
		return 0, true, err
	}
	var target *store.CollectTarget
	for index := range targets {
		if targets[index].MailboxID == mailboxID {
			copy := targets[index]
			target = &copy
			break
		}
	}
	if target == nil {
		clearThrottle()
		return 0, true, fmt.Errorf("取件邮箱不存在或已经停用")
	}
	messages, err := s.collectAccount(target.AccountID, strings.ToLower(strings.TrimSpace(target.ForwardToEmail)), []store.CollectTarget{*target})
	if err != nil {
		clearThrottle()
		return 0, true, err
	}
	for _, message := range messages {
		if err := s.store.SaveMessage(ctx, message); err != nil {
			clearThrottle()
			return processed, true, err
		}
		processed++
	}
	return processed, true, nil
}

// CollectFiltered synchronizes only the requested account/forwarding target when provided.
func (s *Service) CollectFiltered(ctx context.Context, accountFilter, forwardFilter string) (int, []string, error) {
	if err := s.store.ExpireOrders(ctx); err != nil {
		return 0, nil, err
	}
	targets, err := s.store.ListCollectTargets(ctx)
	if err != nil {
		return 0, nil, err
	}
	type collectionGroup struct{ accountID, forwardTo string }
	grouped := make(map[collectionGroup][]store.CollectTarget)
	accountFilter = strings.TrimSpace(accountFilter)
	forwardFilter = strings.ToLower(strings.TrimSpace(forwardFilter))
	for _, target := range targets {
		if accountFilter != "" && target.AccountID != accountFilter {
			continue
		}
		forwardTo := strings.ToLower(strings.TrimSpace(target.ForwardToEmail))
		if forwardFilter != "" && forwardTo != forwardFilter {
			continue
		}
		key := collectionGroup{accountID: target.AccountID, forwardTo: forwardTo}
		grouped[key] = append(grouped[key], target)
	}
	collected := 0
	var failures []string
	for group, accountTargets := range grouped {
		messages, err := s.collectAccount(group.accountID, group.forwardTo, accountTargets)
		if err != nil {
			failures = append(failures, group.accountID+" / "+firstNonEmpty(group.forwardTo, "未指定转发目标")+": "+err.Error())
			continue
		}
		for _, item := range messages {
			if err := s.store.SaveMessage(ctx, item); err != nil {
				failures = append(failures, group.accountID+": save message: "+err.Error())
				continue
			}
			collected++
		}
	}
	return collected, failures, nil
}

// CollectTargets 返回当前收件目标 (可用 + 预留邮箱), 供 IDLE 收件 watcher 等
// 内部消费方复用 store 的同一过滤语义。
func (s *Service) CollectTargets(ctx context.Context) ([]store.CollectTarget, error) {
	return s.store.ListCollectTargets(ctx)
}

// SaveMessage 幂等保存一封邮件。store 层按 (account_id, provider_message_id,
// mailbox_id) 唯一 (Postgres ON CONFLICT DO UPDATE / 内存去重), 重复调用只更新
// 不新增行, 因此 IDLE watcher 与轮询并行入库并发安全。
func (s *Service) SaveMessage(ctx context.Context, message store.Message) error {
	return s.store.SaveMessage(ctx, message)
}

// ConvertIMAPMessage 将 IMAP 增量回调中的一封邮件转换为与 CollectFiltered 入库
// 一致的格式, 复用 convertMessage 的 OTP 提取与日期解析。ProviderMessageID 取
// IMAP UID (mail.Message.ID), 与轮询路径的网页提供商 ID 属于不同命名空间;
// 同一封物理邮件若两路径都入库会各存一行 (IDLE 按 UID 去重, 轮询按网页 ID 去重)。
func ConvertIMAPMessage(target store.CollectTarget, m mailclient.Message) store.Message {
	return convertMessage(target, m.ID, m.From, m.To, m.Subject, m.Preview, m.BodyHTML, m.ContentType, m.Date)
}

func (s *Service) collectAccount(accountID, forwardTo string, targets []store.CollectTarget) ([]store.Message, error) {
	var out []store.Message
	if forwardTo == "" {
		return nil, fmt.Errorf("隐藏邮箱缺少 Apple 转发目标，请先重新同步邮箱库存")
	}
	mc, err := s.mgr.ForwardWebMailClient(accountID, forwardTo)
	if err != nil {
		return nil, err
	}
	recipients := make([]string, 0, len(targets))
	for _, target := range targets {
		recipients = append(recipients, target.MailboxAddress)
	}
	grouped, err := mc.FindByRecipients(recipients, 200, 30)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		msgs := grouped[strings.ToLower(strings.TrimSpace(target.MailboxAddress))]
		for _, msg := range msgs {
			out = append(out, convertMessage(target, msg.ID, msg.From, msg.To, msg.Subject, msg.Preview, msg.BodyHTML, msg.ContentType, msg.Date))
		}
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func convertMessage(target store.CollectTarget, providerID, sender, recipient, subject, bodyText, bodyHTML, contentType, date string) store.Message {
	received := time.Now()
	if parsed, ok := mailclient.ParseMessageDate(date); ok {
		received = parsed
	}
	return store.Message{MailboxID: target.MailboxID, AccountID: target.AccountID, ProviderMessageID: providerID,
		Sender: sender, Recipient: recipient, Subject: subject, BodyText: bodyText, BodyHTML: bodyHTML, ContentType: contentType,
		OTPCode: extractOTP(subject + "\n" + bodyText), ReceivedAt: received}
}

func extractOTP(text string) string {
	matches := otpPattern.FindStringSubmatch(text)
	for i := 1; i < len(matches); i++ {
		if matches[i] != "" {
			return matches[i]
		}
	}
	return ""
}

func randomURLToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomPickupKey() (string, error) {
	token, err := randomURLToken(48)
	if err != nil {
		return "", err
	}
	return "tok_" + token, nil
}

func validPickupKey(key string) bool {
	return len(key) >= 32 && len(key) <= 160 && strings.HasPrefix(key, "tok_")
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
